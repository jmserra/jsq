package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/db"
)

// pane is one independently-navigable view of the database: a grid plus the
// metadata describing what it's showing. Panes are the unit `<space>v` splits.
//
// Deliberately small. Only state that genuinely differs between two side-by-side
// views lives here; everything else stays on App:
//
//   - The connection/database/engine are session-wide — every pane looks at the
//     same database (a conn/db switch while split is refused, see goToView).
//   - status/pendingPos/resetGrid/postExecStatus stay on App because there is one
//     op slot, so at most one of them can be pending at a time.
//   - cell/help/jumps/histView stay on App: they're modal and full-area, not
//     per-pane.
//
// The jumplist (views/viewIdx) IS per-pane — like vim, where each window has its
// own — so Ctrl-O in one pane walks only that pane's history.
type pane struct {
	id int // stable identity; survives reordering, unlike an index

	grid         grid
	currentTable db.Table
	basePreds    []eqPred // followed-FK equality filter, AND-ed into every load
	baseNote     string   // human form of basePreds, shown in the header
	sortCol      string
	sortAsc      bool
	adHoc        bool   // grid shows a free-form (s) query result, not a table
	adHocQuery   string // SQL behind the adHoc result, so `r` can re-run it

	// Per-pane jumplist: visited views oldest→newest, viewIdx = current (-1
	// before the first). See App.views' old doc comment for the semantics.
	views   []viewState
	viewIdx int

	// col is which column this pane lives in, left→right. The layout is columns
	// of stacked panes: `<space>v` opens a new column, `<space>s` stacks into the
	// current one. a.panes is kept ordered by (col, then top→bottom), so each
	// column is a contiguous run — see paneCols.
	col int

	// Rect within the body, assigned by layoutPanes. Focus movement (`<space>hjkl`)
	// is computed from these rather than from the pane order or the column
	// numbering, so it stays correct for any arrangement.
	x, y, w, h int
}

// newPane builds an empty pane with a fresh id and an empty jumplist.
func (a *App) newPane() pane {
	a.nextPaneID++
	return pane{id: a.nextPaneID, grid: newGrid(), viewIdx: -1}
}

// clone returns an independent copy of the pane under a fresh id — what
// `<space>v` puts on the right. It duplicates exactly what the pane is showing,
// including a free-form (s) query result and committed column filters.
//
// It clones the live pane rather than going through viewState because viewState
// models a *jumplist entry*, not a view: it has no adHoc/adHocQuery (so an s
// result would silently clone as the underlying table) and no committed filters
// (those live in gridSnapshot, and are lost once it's evicted).
//
// views must be deep-copied. syncCurrent writes `views[viewIdx] = v`, and the
// clone starts at an identical viewIdx, so a shared backing array would have the
// first navigation in either pane overwrite the other's history. The *gridSnapshot
// pointers inside are shared, which is fine — nothing writes one in place except
// the intentional edit reflection.
func (a *App) clonePane(src *pane) pane {
	c := *src
	c.id = 0 // assigned below; never inherit the source's identity
	c.grid = src.grid.clone()
	c.basePreds = append([]eqPred(nil), src.basePreds...)
	c.views = append([]viewState(nil), src.views...)
	a.nextPaneID++
	c.id = a.nextPaneID
	return c
}

// p returns a pointer to the focused pane, for mutation.
//
// Safe to call on a value receiver: Update copy-on-writes the panes slice, so
// &a.panes[i] points into an array this model owns.
//
// IMPORTANT: never hold the returned pointer across a mutation of the a.panes
// slice header. An append may reallocate, leaving the pointer aimed at the
// orphaned array where writes are silently lost. Re-take p() after any
// append/remove.
func (a *App) p() *pane { return &a.panes[a.focus] }

// g is shorthand for the focused pane's grid — by far the most-used pane field.
func (a *App) g() *grid { return &a.p().grid }

// paneByID resolves a pane by its stable id. Returns false once that pane has
// been closed, which is how a DB result for a since-closed pane gets dropped.
func (a *App) paneByID(id int) (*pane, bool) {
	for i := range a.panes {
		if a.panes[i].id == id {
			return &a.panes[i], true
		}
	}
	return nil, false
}

// handleLeaderKey runs the key that followed <space>. The leader flag is already
// cleared by the caller, so every path here is one-shot.
func (a App) handleLeaderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "v": // vertical split: a copy of this pane, to the right
		return a.splitVertical()
	case "s": // horizontal split: a copy of this pane, below
		return a.splitHorizontal()
	case "h", "j", "k", "l":
		a.focusDir(msg.String())
		return a, nil
	}
	// Anything else (including <space><esc>) just cancels the leader. Esc must not
	// fall through to the op-kill: it was aimed at the leader, not a query.
	return a, nil
}

// maxPanes bounds the split count. Past this the panes are too narrow to read,
// and every pane costs a cached snapshot budget.
const maxPanes = 4

// splitVertical duplicates the focused pane into a new column on its right and
// focuses the copy — so `<space>v` then navigating leaves the original behind.
//
// The new column spans the full body height, even if the focused pane was one of
// several stacked in its own column. That's the one place this layout differs
// from vim, which would split only the focused pane's region.
func (a App) splitVertical() (tea.Model, tea.Cmd) {
	if !a.canSplit() {
		return a, nil
	}
	c := a.clonePane(a.p())
	cur := a.p().col
	c.col = cur + 1
	// Everything from the next column rightwards shifts one across to make room.
	for i := range a.panes {
		if a.panes[i].col > cur {
			a.panes[i].col++
		}
	}
	// Insert after the focused column's last pane, keeping a.panes ordered by col.
	at := 0
	for i := range a.panes {
		if a.panes[i].col == cur {
			at = i + 1
		}
	}
	a.insertPane(at, c)
	return a, nil
}

// splitHorizontal duplicates the focused pane directly below it, within the same
// column, and focuses the copy.
func (a App) splitHorizontal() (tea.Model, tea.Cmd) {
	if !a.canSplit() {
		return a, nil
	}
	c := a.clonePane(a.p())
	c.col = a.p().col
	a.insertPane(a.focus+1, c) // stacked panes are adjacent in the slice
	return a, nil
}

// canSplit reports whether there's room for another pane, reporting why not.
func (a *App) canSplit() bool {
	if len(a.panes) >= maxPanes {
		a.status = fmt.Sprintf("at most %d panes", maxPanes)
		return false
	}
	return true
}

// insertPane puts p at index at, focuses it, and re-lays out.
//
// Note this takes the pane by value and re-lays out AFTER the append: any *pane
// held across it would aim at the orphaned array (see p()), and the clone's
// rowOff was computed for the pre-split height, which setSize re-clamps.
func (a *App) insertPane(at int, p pane) {
	a.panes = append(a.panes, pane{})
	copy(a.panes[at+1:], a.panes[at:])
	a.panes[at] = p
	a.focus = at
	a.layout()
}

// closePane drops the focused pane. A no-op on the last one — there's always at
// least one pane, so p()/g() can't fail.
func (a App) closePane() (tea.Model, tea.Cmd) {
	if len(a.panes) == 1 {
		a.status = "only pane"
		return a, nil
	}
	col := a.p().col
	a.panes = append(a.panes[:a.focus], a.panes[a.focus+1:]...)
	if a.focus >= len(a.panes) {
		a.focus = len(a.panes) - 1
	}
	// If that emptied its column, close the gap: paneCols indexes by col, so a
	// hole would render as an empty column.
	empty := true
	for i := range a.panes {
		if a.panes[i].col == col {
			empty = false
			break
		}
	}
	if empty {
		for i := range a.panes {
			if a.panes[i].col > col {
				a.panes[i].col--
			}
		}
	}
	a.layout()
	return a, nil
}

// focusDir moves focus to the nearest pane in the given direction, by rect
// geometry rather than by pane order or column numbering.
//
// A candidate must overlap on the perpendicular axis — otherwise `j` from a
// left-hand pane could land in a pane that merely happens to sit lower in
// another column, rather than the one actually below it. Ties (several stacked
// panes in the target column) go to the one nearest along that axis.
func (a *App) focusDir(dir string) {
	cur := a.panes[a.focus]
	best, bestD, bestPerp := -1, 0, 0
	for i := range a.panes {
		if i == a.focus {
			continue
		}
		p := a.panes[i]
		var d, perp int
		switch dir {
		case "h", "l":
			if p.y >= cur.y+cur.h || cur.y >= p.y+p.h { // no vertical overlap
				continue
			}
			if dir == "h" {
				if p.x >= cur.x {
					continue
				}
				d = cur.x - p.x
			} else {
				if p.x <= cur.x {
					continue
				}
				d = p.x - cur.x
			}
			perp = absInt(p.y - cur.y)
		case "j", "k":
			if p.x >= cur.x+cur.w || cur.x >= p.x+p.w { // no horizontal overlap
				continue
			}
			if dir == "k" {
				if p.y >= cur.y {
					continue
				}
				d = cur.y - p.y
			} else {
				if p.y <= cur.y {
					continue
				}
				d = p.y - cur.y
			}
			perp = absInt(p.x - cur.x)
		}
		if best < 0 || d < bestD || (d == bestD && perp < bestPerp) {
			best, bestD, bestPerp = i, d, perp
		}
	}
	if best >= 0 {
		a.focus = best
	}
}
