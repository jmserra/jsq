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
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

// spinnerInterval is the header activity spinner's frame rate.
const spinnerInterval = 100 * time.Millisecond

// tickCmd schedules the next spinner frame.
func tickCmd() tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// dbErr maps a command's error to a message. A cancelled context (an Esc kill,
// see App.stop) is swallowed to a nil message so it never surfaces as an error
// screen; any other failure is a real errMsg.
func dbErr(ctx context.Context, err error) tea.Msg {
	if ctx.Err() != nil {
		return nil
	}
	return errMsg{err}
}

// connectCmd runs the whole connect flow off the Update loop (§6 async rule):
// start the `cmd` helper (if any) and wait for the URL's host:port, then open
// the engine and list tables. read_only is enforced at the DB session level too,
// not just by the app-layer guard. The helper is registered globally the instant
// it starts (KillRunHelpers reaps it on exit); on any failure here we kill it.
func connectCmd(c config.Conn) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var proc *runProc
		if c.Cmd != "" {
			p, err := startRun(c.Cmd)
			if err != nil {
				return connectErrMsg{err}
			}
			proc = p
			// The helper is typically a tunnel to the URL's port — wait for it to
			// answer before connecting (no-op for SQLite / a portless DSN).
			if addr := db.HostPort(c.URL); addr != "" {
				if err := waitPort(addr, proc, waitTimeout); err != nil {
					proc.kill()
					return connectErrMsg{err}
				}
			}
		}
		eng, err := db.Open(ctx, c.URL, db.ReadOnly(c.ReadOnly))
		if err != nil {
			proc.kill()
			return connectErrMsg{err}
		}
		tables, err := eng.Tables(ctx)
		if err != nil {
			eng.Close()
			proc.kill()
			return connectErrMsg{err}
		}
		return connectedMsg{engine: eng, name: c.Name, dbName: db.DatabaseName(c.URL), tables: tables}
	}
}

// eqPred is an equality predicate applied to a load in addition to the column
// filters — a base filter that carries a followed foreign key (refCol = value).
type eqPred struct {
	col string
	val any
}

// loadCmd loads the first window of a table with the active sort (J/K), any base
// predicates (a followed FK), and the active column filters (§7.1), server-side.
func loadCmd(ctx context.Context, eng db.Engine, t db.Table, limit int, sortCol string, sortAsc bool, base []eqPred, filters []filterSpec) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		pk, err := eng.PrimaryKey(ctx, ref)
		if err != nil {
			return dbErr(ctx, err)
		}
		where, args := whereClause(eng, base, filters)
		q := fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d",
			eng.QualifiedName(ref), where, orderClause(eng, sortCol, sortAsc, pk), limit)
		rs, err := eng.Query(ctx, q, args...)
		if err != nil {
			return dbErr(ctx, err)
		}
		rs.Table = &ref
		rs.PK = pk
		// FKs drive the header marker and in-place follow (f). Best-effort — a
		// failure here just means no marker / no follow, never a failed load.
		rs.FKs, _ = eng.ForeignKeys(ctx, ref)
		return rowsMsg{table: ref, rs: rs, full: len(rs.Rows) == limit}
	}
}

// databasesCmd lists the databases on the current connection (for the T picker).
func databasesCmd(ctx context.Context, eng db.Engine) tea.Cmd {
	return func() tea.Msg {
		names, err := eng.Databases(ctx)
		if err != nil {
			return dbErr(ctx, err)
		}
		return databasesMsg{names: names}
	}
}

// openEngineCmd is the mid-session (re)connect: optionally start c's `cmd` tunnel
// (only the first time for a connection — startTunnel), open a fresh engine on
// dsn, list its tables, then close the old engine. Used for both database switches
// (startTunnel=false, same connection) and connection switches. A failure is a
// mid-session errMsg — the old engine stays usable until the new one is ready.
func openEngineCmd(old db.Engine, c config.Conn, dsn string, startTunnel bool) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		if startTunnel && c.Cmd != "" {
			p, err := startRun(c.Cmd)
			if err != nil {
				return errMsg{err}
			}
			if addr := db.HostPort(dsn); addr != "" {
				if err := waitPort(addr, p, waitTimeout); err != nil {
					p.kill()
					return errMsg{err}
				}
			}
		}
		eng, err := db.Open(ctx, dsn, db.ReadOnly(c.ReadOnly))
		if err != nil {
			return errMsg{err}
		}
		tables, err := eng.Tables(ctx)
		if err != nil {
			eng.Close()
			return errMsg{err}
		}
		if old != nil {
			old.Close()
		}
		return connectedMsg{engine: eng, name: c.Name, dbName: db.DatabaseName(dsn), tables: tables}
	}
}

// loadMoreCmd fetches the next window (continuous scroll) via LIMIT/OFFSET,
// preserving the active sort and filters.
func loadMoreCmd(ctx context.Context, eng db.Engine, t db.Table, sortCol string, sortAsc bool, base []eqPred, filters []filterSpec, offset, limit int) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		pk, err := eng.PrimaryKey(ctx, ref)
		if err != nil {
			return dbErr(ctx, err)
		}
		where, args := whereClause(eng, base, filters)
		q := fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d OFFSET %d",
			eng.QualifiedName(ref), where, orderClause(eng, sortCol, sortAsc, pk), limit, offset)
		rs, err := eng.Query(ctx, q, args...)
		if err != nil {
			return dbErr(ctx, err)
		}
		return moreRowsMsg{rows: rs.Rows, full: len(rs.Rows) == limit}
	}
}

// execEditCmd runs a quick-path keyed UPDATE (§8): SET col = val WHERE <full PK>.
// The new value binds as a parameter; PK values bind from the edited row. Every
// statement is keyed on the full PK — never a bare UPDATE.
func execEditCmd(ctx context.Context, eng db.Engine, req editReq) tea.Cmd {
	return func() tea.Msg {
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
			return dbErr(ctx, err)
		}
		return editDoneMsg{col: req.col, val: req.val, affected: n}
	}
}

// editorCmd writes the seed SQL to a temp file, opens $EDITOR on it via
// tea.ExecProcess (which releases and restores the terminal) with the cursor on
// the value and (vim-family) the value pre-selected, and on exit reads it back
// (the E/o full paths). An emptied buffer or a quit-without-save (:q!) aborts; a
// save (:wq) — whether edited or run as-is — submits the SQL to run verbatim.
func editorCmd(seed editorSeed) tea.Cmd {
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
		msg := editorResult(seed.sql, string(data), mtimeBumped)
		if sub, ok := msg.(editorSubmitMsg); ok {
			sub.remember = seed.remember // carry the s "remember for table" marker
			return sub
		}
		return msg
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
func editorInvocation(path string, seed editorSeed) (string, []string) {
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
func positionArgs(editor string, seed editorSeed) []string {
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
func execRawCmd(ctx context.Context, eng db.Engine, query string) tea.Cmd {
	return func() tea.Msg {
		n, err := eng.Exec(ctx, query)
		if err != nil {
			return dbErr(ctx, err)
		}
		return execDoneMsg{sql: query, affected: n}
	}
}

// prepareInsertCmd fetches the table's enriched columns (off the Update loop, as
// it's a DB call) and builds the blank-INSERT seed for the o full path; the seed
// then opens in $EDITOR via editorCmd.
func prepareInsertCmd(ctx context.Context, eng db.Engine, t db.Table) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		cols, err := eng.Columns(ctx, ref)
		if err != nil {
			return dbErr(ctx, err)
		}
		return editorReadyMsg{seed: buildInsertStmt(eng, ref, cols)}
	}
}

// runQueryCmd runs a free-form read (s/S) and returns its result set to display.
// The result carries no table/PK provenance, so the grid renders it read-only.
func runQueryCmd(ctx context.Context, eng db.Engine, query string) tea.Cmd {
	return func() tea.Msg {
		rs, err := eng.Query(ctx, query)
		if err != nil {
			return dbErr(ctx, err)
		}
		return queryResultMsg{rs: rs}
	}
}

// prepareDuplicateCmd fetches columns and builds the p (duplicate) seed from the
// captured row values (keyed by column name); it then opens in $EDITOR.
func prepareDuplicateCmd(ctx context.Context, eng db.Engine, t db.Table, vals map[string]any) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		cols, err := eng.Columns(ctx, ref)
		if err != nil {
			return dbErr(ctx, err)
		}
		return editorReadyMsg{seed: buildDuplicateStmt(eng, ref, cols, vals)}
	}
}

// whereClause builds "WHERE p1 AND p2 …" from the base equality predicates (a
// followed FK) followed by the column filters, stacking AND. Base predicates bind
// as exact `col = $i`; filters bind their pattern via FilterPredicate. Parameter
// indexes are shared and 1-based across both.
func whereClause(eng db.Engine, base []eqPred, filters []filterSpec) (string, []any) {
	if len(base) == 0 && len(filters) == 0 {
		return "", nil
	}
	preds := make([]string, 0, len(base)+len(filters))
	args := make([]any, 0, len(base)+len(filters))
	i := 1
	for _, b := range base {
		preds = append(preds, eng.QuoteIdent(b.col)+" = "+eng.Placeholder(i))
		args = append(args, b.val)
		i++
	}
	for _, f := range filters {
		preds = append(preds, eng.FilterPredicate(eng.QuoteIdent(f.col), i))
		args = append(args, f.pattern)
		i++
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
