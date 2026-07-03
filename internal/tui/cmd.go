package tui

import (
	"context"
	"fmt"
	"strings"

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
