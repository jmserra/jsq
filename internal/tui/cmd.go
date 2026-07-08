package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aymanbagabas/go-osc52/v2"
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

// yankCmd copies s to the system clipboard via an OSC 52 escape sequence — it
// goes through the terminal, so it works over SSH with no external binary and
// stays cgo-free. It's written to stderr (a single atomic write) so it never
// interleaves with bubbletea's stdout render stream.
func yankCmd(s string) tea.Cmd {
	return func() tea.Msg {
		os.Stderr.WriteString(osc52.New(s).String())
		return nil
	}
}

// dbErr maps a command's error to a message. A cancelled context (an Esc kill,
// see App.stop) is swallowed to a nil message so it never surfaces as an error
// screen; any other failure is a real errMsg stamped with the op's gen so a
// stale failure can't clobber a newer op.
func dbErr(ctx context.Context, gen int, err error) tea.Msg {
	if ctx.Err() != nil {
		return nil
	}
	return errMsg{err: err, gen: gen}
}

// dbErrSeed is dbErr for a failed user-authored statement: it attaches the seed
// so the errMsg handler can arm the errView modal (full error + statement, with
// e/Enter to reopen it in $EDITOR) instead of collapsing it to a status line. A
// cancelled context is still swallowed to nil, exactly as dbErr does.
func dbErrSeed(ctx context.Context, gen int, err error, seed editorSeed) tea.Msg {
	if ctx.Err() != nil {
		return nil
	}
	return errMsg{err: err, gen: gen, seed: &seed}
}

// connectCmd runs the whole connect flow off the Update loop (§6 async rule):
// start the `cmd` helper (if any) and wait for the URL's host:port, then open
// the engine and list tables. The helper is registered globally the instant
// it starts (KillRunHelpers reaps it on exit); on any failure here we kill it.
func connectCmd(gen int, c config.Conn) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var proc *runProc
		if c.Cmd != "" {
			p, err := startRun(c.Cmd)
			if err != nil {
				return connectErrMsg{err, gen}
			}
			proc = p
			// The helper is typically a tunnel to the URL's port — wait for it to
			// answer before connecting (no-op for SQLite / a portless DSN).
			if addr := db.HostPort(c.URL); addr != "" {
				if err := waitPort(addr, proc, waitTimeout); err != nil {
					proc.kill()
					return connectErrMsg{err, gen}
				}
			}
		}
		eng, err := db.Open(ctx, c.URL)
		if err != nil {
			proc.kill()
			return connectErrMsg{err, gen}
		}
		tables, err := eng.Tables(ctx)
		if err != nil {
			eng.Close()
			proc.kill()
			return connectErrMsg{err, gen}
		}
		return connectedMsg{engine: eng, name: c.Name, dbName: db.DatabaseName(c.URL), tables: tables, gen: gen}
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
func loadCmd(ctx context.Context, gen int, eng db.Engine, t db.Table, limit int, sortCol string, sortAsc bool, base []eqPred, filters []filterSpec) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		pk, err := eng.PrimaryKey(ctx, ref)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		where, args := whereClause(eng, base, filters)
		q := fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d",
			eng.QualifiedName(ref), where, orderClauseKeys(eng, orderKeys(sortCol, sortAsc, pk)), limit)
		rs, err := eng.Query(ctx, q, args...)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		rs.Table = &ref
		rs.PK = pk
		// FKs drive the header marker and in-place follow (f). Best-effort — a
		// failure here just means no marker / no follow, never a failed load.
		rs.FKs, _ = eng.ForeignKeys(ctx, ref)
		return rowsMsg{table: ref, rs: rs, full: len(rs.Rows) == limit, gen: gen}
	}
}

// databasesCmd lists the databases on the current connection (for the T picker).
func databasesCmd(ctx context.Context, gen int, eng db.Engine) tea.Cmd {
	return func() tea.Msg {
		names, err := eng.Databases(ctx)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		return databasesMsg{names: names, gen: gen}
	}
}

// openEngineCmd is the mid-session (re)connect: optionally start c's `cmd` tunnel
// (only the first time for a connection — startTunnel), open a fresh engine on
// dsn, and list its tables. Used for both database switches (startTunnel=false,
// same connection) and connection switches. A failure is a mid-session errMsg
// stamped with gen — the old engine stays usable until the new one is ready.
// Closing the old engine is left to the connectedMsg handler, so a cancelled
// connect (whose result is dropped by gen) never closes the engine we fell back to.
func openEngineCmd(gen int, c config.Conn, dsn string, startTunnel bool) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		if startTunnel && c.Cmd != "" {
			p, err := startRun(c.Cmd)
			if err != nil {
				return errMsg{err: err, gen: gen}
			}
			if addr := db.HostPort(dsn); addr != "" {
				if err := waitPort(addr, p, waitTimeout); err != nil {
					p.kill()
					return errMsg{err: err, gen: gen}
				}
			}
		}
		eng, err := db.Open(ctx, dsn)
		if err != nil {
			return errMsg{err: err, gen: gen}
		}
		tables, err := eng.Tables(ctx)
		if err != nil {
			eng.Close()
			return errMsg{err: err, gen: gen}
		}
		return connectedMsg{engine: eng, name: c.Name, dbName: db.DatabaseName(dsn), tables: tables, gen: gen}
	}
}

// loadMoreCmd fetches the next window for continuous scroll, preserving the
// active sort and filters. When the ordering is keyset-safe (all PK columns —
// see keysetEligible) it pages by a keyset cursor — WHERE the row key is strictly
// past the last loaded row — which is stable under concurrent writes (no
// dup/skip) and jumps to the anchor via the index instead of scanning-and-
// discarding. It falls back to LIMIT/OFFSET (today's behavior) otherwise.
func loadMoreCmd(ctx context.Context, gen int, eng db.Engine, t db.Table, sortCol string, sortAsc bool, base []eqPred, filters []filterSpec, anchor map[string]any, offset, limit int) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		pk, err := eng.PrimaryKey(ctx, ref)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		keys := orderKeys(sortCol, sortAsc, pk)
		order := orderClauseKeys(eng, keys)
		var q string
		var args []any
		where, ksArgs, ok := keysetWhere(eng, base, filters, keys, anchor)
		if ok && keysetEligible(sortCol, pk) {
			q = fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d",
				eng.QualifiedName(ref), where, order, limit)
			args = ksArgs
		} else {
			where, args = whereClause(eng, base, filters)
			q = fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d OFFSET %d",
				eng.QualifiedName(ref), where, order, limit, offset)
		}
		rs, err := eng.Query(ctx, q, args...)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		return moreRowsMsg{rows: rs.Rows, full: len(rs.Rows) == limit, gen: gen}
	}
}

// execEditCmd runs a quick-path keyed UPDATE (§8): SET col = val WHERE <full PK>.
// The new value binds as a parameter; PK values bind from the edited row. Every
// statement is keyed on the full PK — never a bare UPDATE.
func execEditCmd(ctx context.Context, gen int, eng db.Engine, req editReq) tea.Cmd {
	return func() tea.Msg {
		args := make([]any, 0, len(req.keys)+1)
		set := eng.QuoteIdent(req.col) + " = " + eng.Placeholder(1)
		var newVal any = req.val
		if req.null {
			newVal = nil // SET col = NULL (bound, not inlined)
		}
		args = append(args, newVal)
		preds := make([]string, len(req.keys))
		for i, k := range req.keys {
			preds[i] = eng.QuoteIdent(k.col) + " = " + eng.Placeholder(i+2)
			args = append(args, k.val)
		}
		q := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
			eng.QualifiedName(req.table), set, strings.Join(preds, " AND "))
		n, err := eng.Exec(ctx, q, args...)
		if err != nil {
			// A failed quick-path edit reopens as the equivalent E full-path UPDATE
			// (values inlined) so the user can fix and re-run it in $EDITOR.
			var val any = req.val
			if req.null {
				val = nil
			}
			return dbErrSeed(ctx, gen, err, buildUpdateStmt(eng, req.table, req.col, val, req.keys))
		}
		return editDoneMsg{col: req.col, val: req.val, null: req.null, affected: n, gen: gen, rowIdx: req.rowIdx, colIdx: req.colIdx}
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
		return func() tea.Msg { return errMsg{err: err} }
	}
	path := f.Name()
	if _, err := f.WriteString(seed.sql); err != nil {
		f.Close()
		os.Remove(path)
		return func() tea.Msg { return errMsg{err: err} }
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
			return errMsg{err: runErr}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return errMsg{err: err}
		}
		mtimeBumped := false
		if fi, err := os.Stat(path); err == nil && fi.ModTime().After(seedMtime) {
			mtimeBumped = true
		}
		msg := editorResult(seed.sql, string(data), mtimeBumped)
		if sub, ok := msg.(editorSubmitMsg); ok {
			sub.remember = seed.remember // carry the s "remember for table" marker
			sub.scratch = seed.scratch   // and the no-table scratch marker
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
// $EDITOR, so it is not parameterized (unlike the keyed quick-path edit). seed
// reopens the statement in $EDITOR if it fails (see errView).
func execRawCmd(ctx context.Context, gen int, eng db.Engine, query string, seed editorSeed) tea.Cmd {
	return func() tea.Msg {
		n, err := eng.Exec(ctx, query)
		if err != nil {
			return dbErrSeed(ctx, gen, err, seed)
		}
		return execDoneMsg{sql: query, affected: n, gen: gen}
	}
}

// prepareInsertCmd fetches the table's enriched columns (off the Update loop, as
// it's a DB call) and builds the blank-INSERT seed for the o full path; the seed
// then opens in $EDITOR via editorCmd.
func prepareInsertCmd(ctx context.Context, gen int, eng db.Engine, t db.Table) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		cols, err := eng.Columns(ctx, ref)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		return editorReadyMsg{seed: buildInsertStmt(eng, ref, cols), gen: gen}
	}
}

// runQueryCmd runs a free-form read (s/S) and returns its result set to display.
// The result carries no table/PK provenance, so the grid renders it read-only.
// seed reopens the query in $EDITOR if it fails (see errView).
func runQueryCmd(ctx context.Context, gen int, eng db.Engine, query string, seed editorSeed) tea.Cmd {
	return func() tea.Msg {
		rs, err := eng.Query(ctx, query)
		if err != nil {
			return dbErrSeed(ctx, gen, err, seed)
		}
		return queryResultMsg{rs: rs, sql: query, gen: gen}
	}
}

// prepareDuplicateCmd fetches columns and builds the p (duplicate) seed from the
// captured row values (keyed by column name); it then opens in $EDITOR.
func prepareDuplicateCmd(ctx context.Context, gen int, eng db.Engine, t db.Table, vals map[string]any) tea.Cmd {
	return func() tea.Msg {
		ref := t.Ref()
		cols, err := eng.Columns(ctx, ref)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		return editorReadyMsg{seed: buildDuplicateStmt(eng, ref, cols, vals), gen: gen}
	}
}

// filterPreds builds the predicate strings and bind args for the base equality
// predicates (a followed FK) followed by the column filters, numbering
// placeholders from startIdx (1-based, shared across both). Base predicates bind
// as exact `col = $i`; filters bind their pattern via FilterPredicate. Returns
// the next free placeholder index so a keyset cursor can continue the numbering.
func filterPreds(eng db.Engine, base []eqPred, filters []filterSpec, startIdx int) ([]string, []any, int) {
	preds := make([]string, 0, len(base)+len(filters))
	args := make([]any, 0, len(base)+len(filters))
	i := startIdx
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
	return preds, args, i
}

// whereClause builds "WHERE p1 AND p2 …" from the base predicates and column
// filters (empty when there are none).
func whereClause(eng db.Engine, base []eqPred, filters []filterSpec) (string, []any) {
	preds, args, _ := filterPreds(eng, base, filters, 1)
	if len(preds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(preds, " AND "), args
}

// orderKey is one column of the total ordering that drives both the ORDER BY and
// the keyset scroll cursor.
type orderKey struct {
	col string
	asc bool
}

// orderKeys is the total ordering for a load: the explicit sort column (J/K)
// followed by every primary-key column not already named (tiebreakers, in the
// same direction), or — with no explicit sort — the full PK descending (newest
// first). Appending the *whole* PK makes the order total, which is what lets
// keyset paging page cleanly across tied rows. Empty (no sort and no PK) → no
// ORDER BY, and loadMoreCmd falls back to OFFSET.
func orderKeys(sortCol string, sortAsc bool, pk []string) []orderKey {
	if sortCol != "" {
		keys := []orderKey{{sortCol, sortAsc}}
		for _, p := range pk {
			if p != sortCol {
				keys = append(keys, orderKey{p, sortAsc})
			}
		}
		return keys
	}
	keys := make([]orderKey, 0, len(pk))
	for _, p := range pk {
		keys = append(keys, orderKey{p, false}) // default: PK descending
	}
	return keys
}

// orderClauseKeys renders the ORDER BY for a total ordering (empty for none).
func orderClauseKeys(eng db.Engine, keys []orderKey) string {
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		dir := "ASC"
		if !k.asc {
			dir = "DESC"
		}
		parts[i] = eng.QuoteIdent(k.col) + " " + dir
	}
	return " ORDER BY " + strings.Join(parts, ", ")
}

// keysetEligible reports whether a load's ordering is safe to page by keyset:
// every ordering key must be a primary-key column. PK columns are NOT NULL, so
// the order has no NULLs anywhere — which sidesteps the one thing that makes
// keyset silently *skip* rows: a nullable leading sort column whose NULL group
// the engine sorts to the far end (and the engines disagree on which end / by
// direction), so a `col < anchor` cursor would never reach it. The default sort
// (PK descending) and an explicit sort on a PK column qualify; any other explicit
// sort falls back to OFFSET (no worse than before). Requires a PK to exist.
func keysetEligible(sortCol string, pk []string) bool {
	if len(pk) == 0 {
		return false
	}
	if sortCol == "" {
		return true // default: ordered by the PK only
	}
	for _, p := range pk {
		if p == sortCol {
			return true // explicit sort on a PK column → all keys are PK columns
		}
	}
	return false
}

// keysetWhere builds the full WHERE for a keyset-paged scroll fetch: the base
// predicates and column filters, AND-ed with a cursor selecting rows strictly
// past the anchor (the last loaded row) in the total order `keys`. ok is false —
// the caller falls back to OFFSET — when there's no total order, no anchor, or
// any anchor key is NULL (a keyset comparison against NULL is ambiguous). The
// caller also gates on keysetEligible so the keys are all non-null PK columns.
func keysetWhere(eng db.Engine, base []eqPred, filters []filterSpec, keys []orderKey, anchor map[string]any) (string, []any, bool) {
	if len(keys) == 0 || anchor == nil {
		return "", nil, false
	}
	for _, k := range keys {
		if anchor[k.col] == nil {
			return "", nil, false
		}
	}
	preds, args, next := filterPreds(eng, base, filters, 1)
	cursor, cArgs := keysetCursor(eng, keys, anchor, next)
	preds = append(preds, cursor)
	args = append(args, cArgs...)
	return " WHERE " + strings.Join(preds, " AND "), args, true
}

// keysetCursor renders the lexicographic "strictly after the anchor" predicate
// for the total order `keys`: an OR over each key i of (equal on keys 0..i-1 AND
// key i past the anchor), where "past" is `>` for an ascending key and `<` for a
// descending one. Expanding it this way handles mixed ASC/DESC directions (a
// plain `(a,b) > (x,y)` row-value comparison cannot) and stays portable across
// the engines. Every anchor value binds as a parameter; placeholders start at
// startIdx.
func keysetCursor(eng db.Engine, keys []orderKey, anchor map[string]any, startIdx int) (string, []any) {
	terms := make([]string, 0, len(keys))
	var args []any
	idx := startIdx
	for i := range keys {
		conj := make([]string, 0, i+1)
		for j := 0; j < i; j++ {
			conj = append(conj, eng.QuoteIdent(keys[j].col)+" = "+eng.Placeholder(idx))
			args = append(args, anchor[keys[j].col])
			idx++
		}
		cmp := "<"
		if keys[i].asc {
			cmp = ">"
		}
		conj = append(conj, eng.QuoteIdent(keys[i].col)+" "+cmp+" "+eng.Placeholder(idx))
		args = append(args, anchor[keys[i].col])
		idx++
		terms = append(terms, "("+strings.Join(conj, " AND ")+")")
	}
	return "(" + strings.Join(terms, " OR ") + ")", args
}
