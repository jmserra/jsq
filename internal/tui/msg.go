package tui

import "github.com/jmserra/jsq/internal/db"

// connectedMsg is delivered when an engine opens and its tables are listed. gen
// is the connect's op token: a cancelled (Esc) or superseded connect bumps the
// token, so its late connectedMsg is dropped (and its orphaned engine closed)
// instead of yanking the user into a connection they backed out of. The initial
// connect carries gen 0 (never stale — it can't be cancelled in place).
type connectedMsg struct {
	engine db.Engine
	name   string
	dbName string
	tables []db.Table
	gen    int
}

// rowsMsg is delivered when a table's first window of rows loads. full is true
// if the window came back completely filled (so more rows may exist). gen is the
// dispatching op's token (App.gen), so a stale load can be ignored.
type rowsMsg struct {
	table db.TableRef
	rs    *db.ResultSet
	full  bool
	gen   int
}

// moreRowsMsg is delivered when the next window is fetched for continuous scroll.
type moreRowsMsg struct {
	rows [][]any
	full bool
	gen  int
}

// editDoneMsg is delivered after a quick-path cell UPDATE runs (§8). affected is
// the reported row count (should be exactly 1 for a keyed edit).
type editDoneMsg struct {
	col      string
	val      string
	null     bool // the cell was set to SQL NULL
	affected int64
	gen      int
	rowIdx   int // target cell (grid.rows / grid.cols index) for the in-memory write-back
	colIdx   int
}

// editorReadyMsg carries a seed that was built off the Update loop (e.g. o, which
// must fetch column metadata first) and is ready to open in $EDITOR.
type editorReadyMsg struct {
	seed editorSeed
	gen  int
}

// editorSubmitMsg carries the SQL the user saved in $EDITOR (any full path), run
// verbatim. remember (Name set) is the table to store this as the last scratch
// query for — set only for s.
type editorSubmitMsg struct {
	sql      string
	remember db.Table
	scratch  bool // free-form table-list scratch → record in `b` history without a table
}

// editorAbortedMsg means the editor closed without saving (:q!) or the buffer was
// cleared — nothing runs.
type editorAbortedMsg struct{}

// execDoneMsg is delivered after a full-path mutation runs (E/o/D/p, or an s/S
// write).
type execDoneMsg struct {
	sql      string
	affected int64
	gen      int
}

// queryResultMsg is delivered when a free-form read (s/S) returns rows to show.
// sql is the query that produced them, kept so `r` can re-run it.
type queryResultMsg struct {
	rs  *db.ResultSet
	sql string
	gen int
}

// databasesMsg carries the databases available on the current connection (T).
type databasesMsg struct {
	names []string
	gen   int
}

// errMsg carries any async failure that happens mid-session. It's recoverable —
// the engine stays usable — so the handler surfaces it in the status line and
// stays on the current screen (a genuinely fatal connect failure uses
// connectErrMsg instead). gen is the dispatching op's token when the failure came
// from a cancellable DB op (0 for connect/editor errors, which are never stale).
type errMsg struct {
	err error
	gen int
}

// connectErrMsg is a failure during the initial connect (bad DSN, tunnel never
// opened the port, engine wouldn't open). There's no session to fall back to, so
// the App quits and main prints the error to stderr — unless gen marks it stale
// (a picker connect the user cancelled with Esc before it failed), in which case
// it's ignored and we stay on the picker.
type connectErrMsg struct {
	err error
	gen int
}

// tickMsg drives the header activity spinner; it self-perpetuates once the
// connection is up (see the connectedMsg handler).
type tickMsg struct{}
