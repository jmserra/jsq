package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// prettyPHP recognises PHP's serialize() format and renders it as an indented,
// var_dump-style tree. It returns ok=false for anything that isn't a fully
// well-formed serialized array or object, so a cell that merely starts with a
// stray "a" can't be mistaken for one. Only array/object tops are handled —
// serialized scalars need no pretty-printing and rendering "i:5;" as "5" would
// misrepresent the stored value.
func prettyPHP(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != 'a' && s[0] != 'O') {
		return "", false
	}
	p := &phpParser{s: s}
	v, ok := p.value()
	if !ok || p.pos != len(p.s) {
		return "", false
	}
	var b strings.Builder
	writePHP(&b, v, 0)
	return b.String(), true
}

type phpPair struct{ key, val any }
type phpArray struct{ pairs []phpPair }
type phpObject struct {
	class string
	pairs []phpPair
}

// phpParser is a hand-rolled recursive-descent reader over the byte-oriented
// serialize() grammar. String/name lengths are byte counts (Go slicing matches).
type phpParser struct {
	s   string
	pos int
}

func (p *phpParser) value() (any, bool) {
	if p.pos >= len(p.s) {
		return nil, false
	}
	switch p.s[p.pos] {
	case 'N':
		if p.expect("N;") {
			return nil, true
		}
	case 'b':
		if p.expect("b:1;") {
			return true, true
		}
		if p.expect("b:0;") {
			return false, true
		}
	case 'i':
		if p.expect("i:") {
			if tok, ok := p.readUntil(';'); ok {
				if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
					return n, true
				}
			}
		}
	case 'd':
		if p.expect("d:") {
			if tok, ok := p.readUntil(';'); ok {
				return parseDouble(tok)
			}
		}
	case 's':
		return p.parseString()
	case 'a':
		return p.parseContainer("a:", "")
	case 'O':
		return p.parseContainer("O:", "O")
	}
	return nil, false
}

func parseDouble(tok string) (any, bool) {
	switch strings.ToUpper(tok) {
	case "INF":
		return math.Inf(1), true
	case "-INF":
		return math.Inf(-1), true
	case "NAN":
		return math.NaN(), true
	}
	f, err := strconv.ParseFloat(tok, 64)
	return f, err == nil
}

// parseString reads s:<len>:"<bytes>";
func (p *phpParser) parseString() (any, bool) {
	if !p.expect("s:") {
		return nil, false
	}
	n, ok := p.readLen()
	if !ok || !p.expect(`"`) || p.pos+n > len(p.s) {
		return nil, false
	}
	str := p.s[p.pos : p.pos+n]
	p.pos += n
	if !p.expect(`";`) {
		return nil, false
	}
	return str, true
}

// parseContainer reads both a:<n>:{...} and O:<len>:"<class>":<n>:{...}; kind is
// "O" for objects (which carry a class name before the count) and "" for arrays.
func (p *phpParser) parseContainer(prefix, kind string) (any, bool) {
	if !p.expect(prefix) {
		return nil, false
	}
	var class string
	if kind == "O" {
		n, ok := p.readLen()
		if !ok || !p.expect(`"`) || p.pos+n > len(p.s) {
			return nil, false
		}
		class = p.s[p.pos : p.pos+n]
		p.pos += n
		if !p.expect(`":`) {
			return nil, false
		}
	}
	count, ok := p.readLen()
	if !ok || count < 0 || count > len(p.s) || !p.expect("{") {
		return nil, false
	}
	pairs := make([]phpPair, 0, count)
	for i := 0; i < count; i++ {
		k, ok := p.value()
		if !ok {
			return nil, false
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		pairs = append(pairs, phpPair{key: k, val: v})
	}
	if !p.expect("}") {
		return nil, false
	}
	if kind == "O" {
		return &phpObject{class: class, pairs: pairs}, true
	}
	return &phpArray{pairs: pairs}, true
}

// readLen reads decimal digits followed by ':' (the length-then-colon that
// precedes strings, class names, and container counts).
func (p *phpParser) readLen() (int, bool) {
	start := p.pos
	for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == start {
		return 0, false
	}
	n, err := strconv.Atoi(p.s[start:p.pos])
	if err != nil || !p.expect(":") {
		return 0, false
	}
	return n, true
}

func (p *phpParser) readUntil(ch byte) (string, bool) {
	start := p.pos
	for p.pos < len(p.s) && p.s[p.pos] != ch {
		p.pos++
	}
	if p.pos >= len(p.s) {
		return "", false
	}
	tok := p.s[start:p.pos]
	p.pos++ // consume ch
	return tok, true
}

func (p *phpParser) expect(lit string) bool {
	if strings.HasPrefix(p.s[p.pos:], lit) {
		p.pos += len(lit)
		return true
	}
	return false
}

func writePHP(b *strings.Builder, v any, depth int) {
	switch t := v.(type) {
	case nil:
		b.WriteString("NULL")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case string:
		b.WriteString(strconv.Quote(t))
	case *phpArray:
		writeContainer(b, fmt.Sprintf("array(%d)", len(t.pairs)), t.pairs, depth)
	case *phpObject:
		writeContainer(b, fmt.Sprintf("object(%s)(%d)", t.class, len(t.pairs)), t.pairs, depth)
	}
}

func writeContainer(b *strings.Builder, header string, pairs []phpPair, depth int) {
	if len(pairs) == 0 {
		b.WriteString(header + " {}")
		return
	}
	b.WriteString(header + " {\n")
	ind := strings.Repeat("  ", depth+1)
	for _, pr := range pairs {
		b.WriteString(ind)
		switch k := pr.key.(type) {
		case int64:
			fmt.Fprintf(b, "[%d]", k)
		case string:
			fmt.Fprintf(b, "[%s]", strconv.Quote(k))
		default:
			b.WriteString("[?]")
		}
		b.WriteString(" => ")
		writePHP(b, pr.val, depth+1)
		b.WriteByte('\n')
	}
	b.WriteString(strings.Repeat("  ", depth) + "}")
}
