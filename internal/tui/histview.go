package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// histEntry is one remembered free-form (s) query in a connection's history:
// the SQL as authored (trailing whitespace trimmed) plus the outcome of its most
// recent run, filled in once the result lands. Entries are deduped by sql and
// kept most-recent-first.
type histEntry struct {
	sql   string
	count int  // rows returned (read) or rows affected (write) on the last run
	read  bool // last run was a read → count is a row count
	ran   bool // a result has come back → count is meaningful
}

// histKey normalises a query for dedup/lookup: trailing whitespace doesn't make
// two runs of the same statement distinct.
func histKey(sql string) string { return strings.TrimRight(sql, " \t\r\n") }

// histView is the query-history buffer overlay (opened with `b`): a full-area,
// most-recent-first list of the free-form (s) queries run on the current
// connection, each annotated with its last result count. Enter runs a read
// directly (a write opens in $EDITOR for review); `s` opens any entry in $EDITOR
// to evolve it. A snapshot of the list is taken at open time.
type histView struct {
	active  bool
	entries []histEntry
	cursor  int
	off     int
	w, h    int
}

func (hv *histView) open(entries []histEntry, w, h int) {
	hv.active = true
	hv.entries = entries
	hv.cursor, hv.off = 0, 0
	hv.w, hv.h = w, h
}

func (hv *histView) rows() int {
	if n := hv.h - 1; n > 0 { // title line
		return n
	}
	return 1
}

func (hv *histView) move(d int) {
	hv.cursor = clamp(hv.cursor+d, 0, len(hv.entries)-1)
	if hv.cursor < hv.off {
		hv.off = hv.cursor
	}
	if hv.cursor >= hv.off+hv.rows() {
		hv.off = hv.cursor - hv.rows() + 1
	}
}

func (hv *histView) top()    { hv.cursor, hv.off = 0, 0 }
func (hv *histView) bottom() { hv.move(len(hv.entries)) }

func (hv *histView) selected() (histEntry, bool) {
	if hv.cursor >= 0 && hv.cursor < len(hv.entries) {
		return hv.entries[hv.cursor], true
	}
	return histEntry{}, false
}

// trailingLimitRe matches a query's own `LIMIT n` when it's the last clause, so
// the badge can hint (with a trailing +) that a full window may have more rows.
var trailingLimitRe = regexp.MustCompile(`(?i)limit\s+(\d+)\s*;?\s*$`)

// histBadge is the right-aligned annotation for an entry's last run: the row
// count for a read (with a trailing + when it exactly hit the query's own LIMIT,
// hinting there may be more) or the affected count for a write. Empty until it
// has run.
func histBadge(e histEntry) string {
	if !e.ran {
		return ""
	}
	if e.read {
		if m := trailingLimitRe.FindStringSubmatch(e.sql); m != nil {
			if lim, err := strconv.Atoi(m[1]); err == nil && e.count == lim {
				return fmt.Sprintf("%d+ rows", e.count)
			}
		}
		return fmt.Sprintf("%d rows", e.count)
	}
	return fmt.Sprintf("%d chg", e.count)
}

// histSnippet is the one-line preview of a query: its first non-blank,
// non-comment line, whitespace-collapsed.
func histSnippet(sql string) string {
	for _, line := range strings.Split(sql, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		return strings.Join(strings.Fields(t), " ")
	}
	return strings.Join(strings.Fields(sql), " ")
}

func (hv *histView) View() string {
	var b strings.Builder
	faint := lipgloss.NewStyle().Faint(true)
	b.WriteString(headerStyle.Render("query history") +
		faint.Render("  enter runs a read · s edits · esc closes"))

	if len(hv.entries) == 0 {
		b.WriteString("\n" + faint.Render(" no queries yet"))
		return b.String()
	}
	end := min(hv.off+hv.rows(), len(hv.entries))
	for i := hv.off; i < end; i++ {
		b.WriteByte('\n')
		prefix := "  "
		if i == hv.cursor {
			prefix = "▸ "
		}
		badge := histBadge(hv.entries[i])
		bw := runewidth.StringWidth(badge)
		// Reserve room for the badge (plus a gap) on the right.
		avail := hv.w - bw - 1
		if avail < 1 {
			avail = 1
		}
		left := runewidth.FillRight(runewidth.Truncate(prefix+histSnippet(hv.entries[i].sql), avail, "…"), avail)
		if i == hv.cursor {
			left = selStyle.Render(left)
		}
		b.WriteString(left)
		if badge != "" {
			b.WriteString(" " + faint.Render(badge))
		}
	}
	return b.String()
}
