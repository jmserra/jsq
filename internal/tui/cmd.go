package tui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// tablesCmd re-lists the current database's tables, for `r` on the table list.
func tablesCmd(ctx context.Context, gen int, eng db.Engine) tea.Cmd {
	return func() tea.Msg {
		tables, err := eng.Tables(ctx)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		return tablesMsg{tables: tables, gen: gen}
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
			sub.after = seed.after       // and what the write should refresh
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
	case selectWord:
		return []string{cur, `+call feedkeys("viw", "n")`}
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
		return execDoneMsg{sql: query, affected: n, after: seed.after, gen: gen}
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
func runQueryCmd(ctx context.Context, gen int, eng db.Engine, query string, seed editorSeed, args ...any) tea.Cmd {
	return func() tea.Msg {
		rs, err := eng.Query(ctx, query, args...)
		if err != nil {
			return dbErrSeed(ctx, gen, err, seed)
		}
		return queryResultMsg{rs: rs, sql: query, args: args, gen: gen}
	}
}

// usersCmd lists the server's users/roles (for the `u` picker).
func usersCmd(ctx context.Context, gen int, eng db.Engine) tea.Cmd {
	return func() tea.Msg {
		users, err := eng.Users(ctx)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		return usersMsg{users: users, gen: gen}
	}
}

// grantsCmd shows one user's privileges. The engine composes the query (it may
// probe what its server exposes first), then it runs as an ordinary ad-hoc read
// — so the result lands in the grid like an `s` query, and `r` re-runs the same
// SQL and args without re-probing. Values are bound, not inlined (invariant 7).
func grantsCmd(ctx context.Context, gen int, eng db.Engine, u db.User) tea.Cmd {
	return func() tea.Msg {
		query, args, err := eng.GrantsSQL(ctx, u)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		if query == "" {
			return errMsg{err: fmt.Errorf("no user privileges on this engine"), gen: gen}
		}
		rs, err := eng.Query(ctx, query, args...)
		if err != nil {
			return dbErr(ctx, gen, err)
		}
		return queryResultMsg{rs: rs, sql: query, args: args, user: u, gen: gen}
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

// --- SQL export (`e` on the table list) ---

// liveExportTemps tracks the temp file of every running export so CleanupExports
// — deferred by main, exactly like KillRunHelpers — removes it however the
// program quits. writeDump's own defer can't cover a Ctrl-C: the process exits
// without unwinding the export's goroutine, which would leave a half-written
// file sitting next to the one you asked for.
var liveExportTemps = struct {
	mu sync.Mutex
	m  map[string]struct{}
}{m: map[string]struct{}{}}

func trackExportTemp(p string) {
	liveExportTemps.mu.Lock()
	liveExportTemps.m[p] = struct{}{}
	liveExportTemps.mu.Unlock()
}

func untrackExportTemp(p string) {
	liveExportTemps.mu.Lock()
	delete(liveExportTemps.m, p)
	liveExportTemps.mu.Unlock()
}

// CleanupExports removes the temp file of any export still in flight.
func CleanupExports() {
	liveExportTemps.mu.Lock()
	paths := make([]string, 0, len(liveExportTemps.m))
	for p := range liveExportTemps.m {
		paths = append(paths, p)
	}
	liveExportTemps.m = map[string]struct{}{}
	liveExportTemps.mu.Unlock()
	for _, p := range paths {
		os.Remove(p)
	}
}

// exportReq is one dump: which tables, how much of each, and where it goes.
type exportReq struct {
	path      string
	tables    []db.Table
	limit     int  // newest-N rows per table (0 = the whole table)
	structure bool // DROP/CREATE (Postgres: DELETE) before each table's rows
	conn      string
	dbName    string
}

// exportCmd starts an export job: a command that writes the file while reporting
// progress into ch, batched with the one that waits for ch's first message.
//
// It is deliberately NOT a gen-stamped op like every other DB command. Those
// share one slot, so the next thing you do cancels the one before it — right for
// a query, fatal for an export, which is exactly the thing you start and then
// walk away from. The job carries its own ctx and its own token instead.
//
// The progress loop is the standard bubbletea shape: the handler for each
// message re-issues exportWaitCmd, so messages keep arriving until the terminal
// exportDoneMsg, after which nobody waits again.
func exportCmd(ctx context.Context, job int, eng db.Engine, req exportReq, ch chan tea.Msg) tea.Cmd {
	return tea.Batch(exportRunCmd(ctx, job, eng, req, ch), exportWaitCmd(ch))
}

// exportRunCmd does the writing. Progress sends are non-blocking — a dropped
// progress message costs nothing and must never stall the export — while the
// terminal message is sent blocking, because the App clears its job state on it
// and there is always exactly one waiter pending to receive it.
func exportRunCmd(ctx context.Context, job int, eng db.Engine, req exportReq, ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		rows, err := writeDump(ctx, eng, req, func(p exportProgressMsg) {
			p.job = job
			select {
			case ch <- p:
			default:
			}
		})
		// A cancelled export still reports: err is the ctx's, which the handler
		// renders as "cancelled" rather than a failure.
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		ch <- exportDoneMsg{path: req.path, tables: len(req.tables), rows: rows, err: err, job: job}
		return nil
	}
}

// exportWaitCmd blocks for the job's next message.
func exportWaitCmd(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// exportProgressEvery is how many rows a table writes between progress reports.
// A var so a test can lower it rather than having to write a big table.
var exportProgressEvery int64 = 2000

// writeDump streams every selected table into one file and returns the total row
// count. It writes to a sibling temp file and renames on success, so an
// interrupted or failed export never leaves a truncated file looking like a
// finished one. The temp name is unique rather than "<path>.part": a second
// export dispatched over a running one (begin() cancels the first, but its
// cleanup still runs) would otherwise delete the newer export's file.
//
// Rows stream: a table is read one row at a time and rendered straight to the
// buffered writer, so dumping a table far larger than memory costs nothing. The
// exception is a row cap, which reads the NEWEST rows (primary key descending)
// and therefore has to buffer them to write them back in insertion order — but
// that buffer is exactly the cap the user asked for.
func writeDump(ctx context.Context, eng db.Engine, req exportReq, report func(exportProgressMsg)) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(req.path), filepath.Base(req.path)+".*.part")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	trackExportTemp(tmp)
	w := bufio.NewWriter(f)
	done := false
	defer func() {
		f.Close()
		untrackExportTemp(tmp)
		if !done {
			os.Remove(tmp)
		}
	}()

	// The heading names whichever of connection/database jsq actually has — a
	// bare-DSN session has no connection name, and an empty one would leave a
	// dangling separator.
	var from []string
	for _, part := range []string{req.conn, req.dbName} {
		if part != "" && (len(from) == 0 || from[0] != part) {
			from = append(from, part)
		}
	}
	fmt.Fprintf(w, "-- jsq export · %s\n-- %s\n",
		strings.Join(from, " · "), time.Now().Format("2006-01-02 15:04:05"))
	if req.limit > 0 {
		fmt.Fprintf(w, "-- PARTIAL: newest %d rows per table, by primary key descending\n", req.limit)
	}
	fmt.Fprintf(w, "\n%s\n", eng.DumpPrologue())

	var total int64
	for i, t := range req.tables {
		step := exportProgressMsg{table: tableLabel(t), index: i + 1, total: len(req.tables)}
		if report != nil {
			step.rows = total
			report(step)
		}
		// The per-table callback reports mid-table progress, so one huge table
		// isn't a silent wait — which is the case the whole job design exists for.
		n, err := dumpTable(ctx, w, eng, t, req, func(done int64) {
			if report != nil {
				step.rows = total + done
				report(step)
			}
		})
		if err != nil {
			// A cancelled export is not a failure to report — the caller turns the
			// ctx error into "cancelled" — but it must still stop here.
			return total, fmt.Errorf("exporting %s: %w", tableLabel(t), err)
		}
		total += n
	}

	fmt.Fprintf(w, "%s", eng.DumpEpilogue())
	if err := w.Flush(); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, req.path); err != nil {
		return 0, err
	}
	done = true
	return total, nil
}

// dumpTable writes one table's section: its heading, the optional structure
// block, then an INSERT per row. The row count lands in a trailing comment
// because streaming means we don't know it until the scan ends.
func dumpTable(ctx context.Context, w *bufio.Writer, eng db.Engine, t db.Table, req exportReq, report func(int64)) (int64, error) {
	ref := t.Ref()
	qual := eng.QualifiedName(ref)

	// The row cap takes the newest rows, which needs the key to order by. Without
	// a primary key there is no "newest", so the cap becomes an arbitrary slice —
	// said out loud in the file rather than silently.
	var pk []string
	order, note := "", ""
	if req.limit > 0 {
		var err error
		if pk, err = eng.PrimaryKey(ctx, ref); err != nil {
			return 0, err
		}
		if len(pk) > 0 {
			order = orderClauseKeys(eng, orderKeys("", false, pk))
			note = fmt.Sprintf(" · newest %d by %s desc", req.limit, strings.Join(pk, ", "))
		} else {
			note = fmt.Sprintf(" · an arbitrary %d rows (no primary key to take the newest by)", req.limit)
		}
	}

	fmt.Fprintf(w, "-- %s%s\n", tableLabel(t), note)
	if req.structure {
		ddl, err := eng.StructureSQL(ctx, ref)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(w, "%s", ddl)
	}

	q := "SELECT * FROM " + qual + order
	if req.limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", req.limit)
	}

	var prefix string
	var types []string
	var n int64
	var buf []string // only used for a capped read, which arrives newest-first
	err := eng.Stream(ctx, q, nil, db.RowStream{
		Head: func(cols, colTypes []string) error {
			quoted := make([]string, len(cols))
			for i, c := range cols {
				quoted[i] = eng.QuoteIdent(c)
			}
			prefix = "INSERT INTO " + qual + " (" + strings.Join(quoted, ", ") + ") VALUES ("
			types = colTypes
			return nil
		},
		Row: func(vals []any) error {
			lits := make([]string, len(vals))
			for i, v := range vals {
				lits[i] = eng.SQLLiteral(v, types[i])
			}
			line := prefix + strings.Join(lits, ", ") + ");\n"
			n++
			if report != nil && n%exportProgressEvery == 0 {
				report(n)
			}
			if len(pk) > 0 { // read backwards — hold it and write it forwards
				buf = append(buf, line)
				return nil
			}
			_, err := w.WriteString(line)
			return err
		},
	})
	if err != nil {
		return 0, err
	}
	for i := len(buf) - 1; i >= 0; i-- {
		if _, err := w.WriteString(buf[i]); err != nil {
			return 0, err
		}
	}

	fmt.Fprintf(w, "-- %d %s\n\n", n, plural(int(n), "row"))
	return n, nil
}
