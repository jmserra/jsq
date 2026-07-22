package tui

import "testing"

func TestPrettyPHP(t *testing.T) {
	in := `a:3:{i:0;a:1:{s:5:"fname";s:2:"id";}i:1;a:1:{s:5:"fname";s:5:"group";}i:2;a:1:{s:5:"fname";s:4:"name";}}`
	want := `array(3) {
  [0] => array(1) {
    ["fname"] => "id"
  }
  [1] => array(1) {
    ["fname"] => "group"
  }
  [2] => array(1) {
    ["fname"] => "name"
  }
}`
	got, ok := prettyPHP(in)
	if !ok {
		t.Fatalf("prettyPHP returned ok=false for valid input")
	}
	if got != want {
		t.Fatalf("prettyPHP mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestPrettyPHPScalarsAndObject(t *testing.T) {
	in := `O:8:"stdClass":4:{s:2:"id";i:42;s:2:"ok";b:1;s:3:"amt";d:3.5;s:4:"note";N;}`
	want := `object(stdClass)(4) {
  ["id"] => 42
  ["ok"] => true
  ["amt"] => 3.5
  ["note"] => NULL
}`
	got, ok := prettyPHP(in)
	if !ok {
		t.Fatalf("prettyPHP returned ok=false for valid object")
	}
	if got != want {
		t.Fatalf("object mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestPrettyPHPEmptyArray(t *testing.T) {
	got, ok := prettyPHP(`a:0:{}`)
	if !ok || got != "array(0) {}" {
		t.Fatalf("empty array: got %q ok=%v", got, ok)
	}
}

func TestPrettyPHPRejects(t *testing.T) {
	cases := []string{
		"",                         // empty
		"hello world",              // plain text
		"array data",               // starts with 'a' but not serialized
		`a:3:{i:0;s:2:"id";}`,      // count mismatch (trailing bytes unconsumed)
		`a:1:{i:0;s:5:"id";}`,      // string length overruns
		`i:5;`,                     // scalar top-level not pretty-printed
		`s:2:"hi";`,                // scalar top-level not pretty-printed
		`a:1:{i:0;s:2:"id";}extra`, // trailing garbage
	}
	for _, c := range cases {
		if _, ok := prettyPHP(c); ok {
			t.Errorf("prettyPHP(%q) = ok, want rejected", c)
		}
	}
}

// A string of PHP-serialized data must render through the cell viewer.
func TestValueLinesPHP(t *testing.T) {
	lines := valueLines(`a:1:{s:1:"a";i:1;}`)
	if len(lines) != 3 || lines[0] != "array(1) {" || lines[1] != `  ["a"] => 1` {
		t.Fatalf("valueLines PHP: %#v", lines)
	}
}
