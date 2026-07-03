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

// errMsg carries any async failure.
type errMsg struct{ err error }
