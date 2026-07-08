package tui

import (
	"strings"
	"unicode"
)

// textField is a single-line text input with a rune-indexed cursor, shared by the
// grid column filter and the list (sidebar) filter so both edit the same way
// (arrows, Home/End, Ctrl-W word-delete). The quick-path cell edit keeps its own
// richer overlay (block caret) and does not use this.
type textField struct {
	val string
	pos int // caret as a rune index into val, in [0, len(runes)]
}

// setVal replaces the text and parks the caret at the end (used to pre-fill an
// existing pattern when re-entering the filter).
func (f *textField) setVal(s string) { f.val, f.pos = s, len([]rune(s)) }

func (f *textField) clear() { f.val, f.pos = "", 0 }

// insert adds s at the caret and advances past it.
func (f *textField) insert(s string) {
	r, ins := []rune(f.val), []rune(s)
	r = append(r[:f.pos:f.pos], append(ins, r[f.pos:]...)...)
	f.val = string(r)
	f.pos += len(ins)
}

// backspace deletes the rune before the caret.
func (f *textField) backspace() {
	if f.pos > 0 {
		r := []rune(f.val)
		r = append(r[:f.pos-1], r[f.pos:]...)
		f.val = string(r)
		f.pos--
	}
}

// del removes the rune at the caret (forward delete); a no-op at end of text.
func (f *textField) del() {
	r := []rune(f.val)
	if f.pos < len(r) {
		r = append(r[:f.pos], r[f.pos+1:]...)
		f.val = string(r)
	}
}

// deleteWord deletes from the start of the word before the caret up to the caret
// (Ctrl-W): trailing spaces first, then the run of non-spaces.
func (f *textField) deleteWord() {
	r := []rune(f.val)
	i := f.pos
	for i > 0 && unicode.IsSpace(r[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(r[i-1]) {
		i--
	}
	r = append(r[:i], r[f.pos:]...)
	f.val = string(r)
	f.pos = i
}

func (f *textField) left() {
	if f.pos > 0 {
		f.pos--
	}
}

func (f *textField) right() {
	if f.pos < len([]rune(f.val)) {
		f.pos++
	}
}

func (f *textField) home() { f.pos = 0 }
func (f *textField) end()  { f.pos = len([]rune(f.val)) }

// render returns prefix followed by the text with a bar caret at the cursor, e.g.
// render("⌕") → "⌕ab▏c". Matches the prior end-caret look when the cursor is at
// the end.
func (f *textField) render(prefix string) string {
	r := []rune(f.val)
	pos := clamp(f.pos, 0, len(r))
	return prefix + string(r[:pos]) + "▏" + string(r[pos:])
}

// filterPatterns returns the LIKE pattern(s) to try for raw filter text, in order:
// the accurate prefix search (`raw%`) first, and — only when the user typed no
// wildcards of their own — a lazy substring fallback (`%raw%`) to try when the
// prefix matched nothing. An empty raw yields a single empty pattern (match all).
func filterPatterns(raw string) []string {
	if raw == "" {
		return []string{""}
	}
	prefix := searchPattern(raw)
	if strings.Contains(raw, "%") { // user controls the wildcards → no fallback
		return []string{prefix}
	}
	return []string{prefix, "%" + raw + "%"}
}
