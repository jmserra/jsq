package tui

import (
	"strings"
	"unicode/utf8"
)

// listRank is how well a list label matches the typed filter; lower sorts first.
type listRank struct {
	tier int // 0 exact · 1 prefix · 2 word prefix · 3 substring · 4 subsequence
	span int // tier 4 only: runes from first to last matched letter (tighter = better)
	n    int // label length in runes: among equals, the shorter name is the closer one
}

func (a listRank) less(b listRank) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	if a.span != b.span {
		return a.span < b.span
	}
	return a.n < b.n
}

// rankLabel scores label against the typed query q (case-insensitive); ok=false
// means it doesn't match at all. Every match is kept — the tiers only order
// them — so a fuzzy hit is still reachable, it just sits below the real ones.
// "Exact" also accepts the part after a schema dot, so `users` is an exact hit
// on `public.users`; a "word" starts after `_`, `.`, `-` or a space, so `items`
// word-prefixes `order_items`. The subsequence tier is fzf's basic idea: the
// letters in order with gaps allowed (`oit` → `order_items`), ranked by how
// tightly they cluster.
func rankLabel(label, q string) (r listRank, ok bool) {
	l, q := strings.ToLower(label), strings.ToLower(q)
	r.n = utf8.RuneCountInString(l)
	switch {
	case l == q || l[strings.LastIndexByte(l, '.')+1:] == q:
		r.tier = 0
	case strings.HasPrefix(l, q):
		r.tier = 1
	case wordPrefix(l, q):
		r.tier = 2
	case strings.Contains(l, q):
		r.tier = 3
	default:
		span, hit := subsequenceSpan([]rune(l), []rune(q))
		if !hit {
			return r, false
		}
		r.tier, r.span = 4, span
	}
	return r, true
}

// wordPrefix reports whether q occurs in l right after a word separator.
func wordPrefix(l, q string) bool {
	for i := 1; i < len(l); i++ {
		if strings.ContainsRune("_.- ", rune(l[i-1])) && strings.HasPrefix(l[i:], q) {
			return true
		}
	}
	return false
}

// subsequenceSpan finds q's runes in l in order and returns the narrowest window
// holding them: from every occurrence of q's first rune, match the rest greedily
// and keep the shortest. Quadratic in the label, which is a table name.
func subsequenceSpan(l, q []rune) (int, bool) {
	best, found := 0, false
	for start := range l {
		if l[start] != q[0] {
			continue
		}
		j, end := 1, start
		for i := start + 1; i < len(l) && j < len(q); i++ {
			if l[i] == q[j] {
				j, end = j+1, i
			}
		}
		if j < len(q) {
			break // no later start can fit what this one couldn't
		}
		if span := end - start + 1; !found || span < best {
			best, found = span, true
		}
	}
	return best, found
}
