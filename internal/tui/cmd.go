package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/db"
)

// connectCmd opens the engine and lists tables, off the Update loop (§6 async rule).
func connectCmd(dsn, name string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		eng, err := db.Open(ctx, dsn)
		if err != nil {
			return errMsg{err}
		}
		tables, err := eng.Tables(ctx)
		if err != nil {
			eng.Close()
			return errMsg{err}
		}
		return connectedMsg{engine: eng, name: name, dbName: db.DatabaseName(dsn), tables: tables}
	}
}

// loadCmd loads the first window of a table with the active sort (J/K) and the
// active column filters (§7.1) applied server-side.
func loadCmd(eng db.Engine, t db.Table, limit int, sortCol string, sortAsc bool, filters []filterSpec) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		ref := t.Ref()
		pk, err := eng.PrimaryKey(ctx, ref)
		if err != nil {
			return errMsg{err}
		}
		where, args := whereClause(eng, filters)
		q := fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d",
			eng.QualifiedName(ref), where, orderClause(eng, sortCol, sortAsc, pk), limit)
		rs, err := eng.Query(ctx, q, args...)
		if err != nil {
			return errMsg{err}
		}
		rs.Table = &ref
		rs.PK = pk
		return rowsMsg{table: ref, rs: rs, full: len(rs.Rows) == limit}
	}
}

// loadMoreCmd fetches the next window (continuous scroll) via LIMIT/OFFSET,
// preserving the active sort and filters.
func loadMoreCmd(eng db.Engine, t db.Table, sortCol string, sortAsc bool, filters []filterSpec, offset, limit int) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		ref := t.Ref()
		pk, err := eng.PrimaryKey(ctx, ref)
		if err != nil {
			return errMsg{err}
		}
		where, args := whereClause(eng, filters)
		q := fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d OFFSET %d",
			eng.QualifiedName(ref), where, orderClause(eng, sortCol, sortAsc, pk), limit, offset)
		rs, err := eng.Query(ctx, q, args...)
		if err != nil {
			return errMsg{err}
		}
		return moreRowsMsg{rows: rs.Rows, full: len(rs.Rows) == limit}
	}
}

// execEditCmd runs a quick-path keyed UPDATE (§8): SET col = val WHERE <full PK>.
// The new value binds as a parameter; PK values bind from the edited row. Every
// statement is keyed on the full PK — never a bare UPDATE.
func execEditCmd(eng db.Engine, req editReq) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		args := make([]any, 0, len(req.keys)+1)
		set := eng.QuoteIdent(req.col) + " = " + eng.Placeholder(1)
		args = append(args, req.val)
		preds := make([]string, len(req.keys))
		for i, k := range req.keys {
			preds[i] = eng.QuoteIdent(k.col) + " = " + eng.Placeholder(i+2)
			args = append(args, k.val)
		}
		q := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
			eng.QualifiedName(req.table), set, strings.Join(preds, " AND "))
		n, err := eng.Exec(ctx, q, args...)
		if err != nil {
			return errMsg{err}
		}
		return editDoneMsg{col: req.col, val: req.val, affected: n}
	}
}

// editorCmd writes the seed SQL to a temp file, opens $EDITOR on it via
// tea.ExecProcess (which releases and restores the terminal) with the cursor on
// the value and (vim-family) the value pre-selected, and on exit reads it back
// (E full path). An emptied buffer or a quit-without-save (:q!) aborts; a save
// (:wq) — whether edited or run as-is — submits the SQL to run verbatim.
func editorCmd(seed updateSeed) tea.Cmd {
	f, err := os.CreateTemp("", "jsq-*.sql")
	if err != nil {
		return func() tea.Msg { return errMsg{err} }
	}
	path := f.Name()
	if _, err := f.WriteString(seed.sql); err != nil {
		f.Close()
		os.Remove(path)
		return func() tea.Msg { return errMsg{err} }
	}
	f.Close()

	var seedMtime time.Time
	if fi, err := os.Stat(path); err == nil {
		seedMtime = fi.ModTime()
	}

	name, args := editorInvocation(path, seed)
	c := exec.Command(name, args...)
	return tea.ExecProcess(c, func(runErr error) tea.Msg {
		defer os.Remove(path)
		if runErr != nil {
			return errMsg{runErr}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return errMsg{err}
		}
		mtimeBumped := false
		if fi, err := os.Stat(path); err == nil && fi.ModTime().After(seedMtime) {
			mtimeBumped = true
		}
		return editorResult(seed.sql, string(data), mtimeBumped)
	})
}

// editorResult decides run-vs-abort from the editor's outcome. An emptied buffer
// (cleared → cancel) aborts. Otherwise a save runs the SQL: mtime bumps on :wq
// even without edits, and a content change covers the case where mtime
// granularity can't tell an as-is :wq from a :q!. Neither → :q! → abort.
func editorResult(seed, post string, mtimeBumped bool) tea.Msg {
	if strings.TrimSpace(stripSQLComments(post)) == "" {
		return editorAbortedMsg{}
	}
	if post != seed || mtimeBumped {
		return editorSubmitMsg{sql: post}
	}
	return editorAbortedMsg{}
}

// editorInvocation resolves $EDITOR (falling back to vi) into a command name and
// its args: the editor's own flags (whitespace-split, so "code -w" works), then
// any vim-family cursor/selection commands, then the file path.
func editorInvocation(path string, seed updateSeed) (string, []string) {
	ed := os.Getenv("EDITOR")
	if ed == "" {
		ed = "vi"
	}
	parts := strings.Fields(ed)
	name := parts[0]
	args := append([]string{}, parts[1:]...)
	args = append(args, positionArgs(name, seed)...)
	return name, append(args, path)
}

// positionArgs returns vim-family startup commands to place the cursor on the
// value and pre-select it in Visual mode (empty for non-vim editors, which just
// open the file). feedkeys — not :normal — is used so the selection persists
// into the interactive session.
func positionArgs(editor string, seed updateSeed) []string {
	if seed.line < 1 || seed.col < 1 || !isVimFamily(editor) {
		return nil
	}
	cur := fmt.Sprintf("+call cursor(%d,%d)", seed.line, seed.col)
	switch seed.kind {
	case selectInsideQuotes:
		return []string{cur, `+call feedkeys("vi'", "n")`}
	case selectToken:
		return []string{cur, `+call feedkeys("v$", "n")`}
	default:
		return []string{cur}
	}
}

// isVimFamily reports whether the editor command is a vim variant that
// understands the +call cursor/feedkeys startup commands.
func isVimFamily(editor string) bool {
	switch filepath.Base(editor) {
	case "vim", "nvim", "vi", "view", "gvim", "mvim", "rvim", "vimx":
		return true
	}
	return false
}

// execRawCmd runs a full-path statement verbatim — the user authored it in
// $EDITOR, so it is not parameterized (unlike the keyed quick-path edit).
func execRawCmd(eng db.Engine, query string) tea.Cmd {
	return func() tea.Msg {
		n, err := eng.Exec(context.Background(), query)
		if err != nil {
			return errMsg{err}
		}
		return execDoneMsg{sql: query, affected: n}
	}
}

// whereClause builds "WHERE p1 AND p2 …" from the column filters (stacking AND),
// binding each pattern as a parameter via the engine's FilterPredicate.
func whereClause(eng db.Engine, filters []filterSpec) (string, []any) {
	if len(filters) == 0 {
		return "", nil
	}
	preds := make([]string, 0, len(filters))
	args := make([]any, 0, len(filters))
	for i, f := range filters {
		preds = append(preds, eng.FilterPredicate(eng.QuoteIdent(f.col), i+1))
		args = append(args, f.pattern)
	}
	return " WHERE " + strings.Join(preds, " AND "), args
}

func orderClause(eng db.Engine, sortCol string, sortAsc bool, pk []string) string {
	dir := "ASC"
	if !sortAsc {
		dir = "DESC"
	}
	if sortCol != "" {
		s := " ORDER BY " + eng.QuoteIdent(sortCol) + " " + dir
		if len(pk) > 0 && pk[0] != sortCol {
			s += ", " + eng.QuoteIdent(pk[0]) + " " + dir
		}
		return s
	}
	// Default (no explicit J/K sort): PK descending — newest rows first.
	if len(pk) > 0 {
		return " ORDER BY " + eng.QuoteIdent(pk[0]) + " DESC"
	}
	return ""
}
