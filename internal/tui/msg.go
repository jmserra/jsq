package tui

import "github.com/jmserra/jsq/internal/db"

// connectedMsg is delivered when an engine opens and its tables are listed.
type connectedMsg struct {
	engine db.Engine
	name   string
	dbName string
	tables []db.Table
}

// rowsMsg is delivered when a table's first window of rows loads. full is true
// if the window came back completely filled (so more rows may exist).
type rowsMsg struct {
	table db.TableRef
	rs    *db.ResultSet
	full  bool
}

// moreRowsMsg is delivered when the next window is fetched for continuous scroll.
type moreRowsMsg struct {
	rows [][]any
	full bool
}

// editDoneMsg is delivered after a quick-path cell UPDATE runs (§8). affected is
// the reported row count (should be exactly 1 for a keyed edit).
type editDoneMsg struct {
	col      string
	val      string
	affected int64
}

// editorSubmitMsg carries the SQL the user saved in $EDITOR (E full path). It is
// run verbatim.
type editorSubmitMsg struct{ sql string }

// editorAbortedMsg means the editor closed without saving (:q!) or the buffer was
// cleared — nothing runs.
type editorAbortedMsg struct{}

// execDoneMsg is delivered after a full-path statement runs (E; later o/D/p/s).
type execDoneMsg struct {
	sql      string
	affected int64
}

// errMsg carries any async failure.
type errMsg struct{ err error }
