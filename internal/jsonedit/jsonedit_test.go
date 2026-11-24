package jsonedit

import (
	"strings"
	"testing"
)

func TestRoundTripKeepsOrderAndNumbers(t *testing.T) {
	in := `{"b":1,"a":{"z":true,"y":null},"c":[1,2.50,"x"],"e":{},"f":[]}`
	v, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Marshal(v, "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "b": 1,
  "a": {
    "z": true,
    "y": null
  },
  "c": [
    1,
    2.50,
    "x"
  ],
  "e": {},
  "f": []
}
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestObjectEditing(t *testing.T) {
	o := NewObject()
	o.Set("first", "1")
	o.Set("second", "2")
	o.Set("first", "one")
	o.Set("third", "3")
	o.Delete("second")
	o.Delete("missing")
	if o.Len() != 2 {
		t.Fatalf("Len = %d", o.Len())
	}
	if v, ok := o.Get("first"); !ok || v != "one" {
		t.Errorf("first = %v, %v", v, ok)
	}
	if _, ok := o.Get("second"); ok {
		t.Error("second was not deleted")
	}
	got, err := Marshal(o, "\t")
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n\t\"first\": \"one\",\n\t\"third\": \"3\"\n}\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNoHTMLEscaping(t *testing.T) {
	o := NewObject()
	o.Set("command", "a && b <c>")
	got, err := Marshal(o, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"a && b <c>"`) {
		t.Errorf("got %s", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{`{} {}`, `{"a":}`, `[1,`, `{"a":1`, ``} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		}
	}
	if _, err := Parse([]byte("{\"a\": 1}\n\n")); err != nil {
		t.Errorf("trailing white space: %v", err)
	}
}

func TestIndent(t *testing.T) {
	cases := map[string]string{
		"{\n    \"a\": 1\n}":      "    ",
		"{\n\t\"a\": 1\n}":        "\t",
		"{}":                      "  ",
		"{\n\n  \"a\": {\n  }\n}": "  ",
	}
	for in, want := range cases {
		if got := Indent([]byte(in)); got != want {
			t.Errorf("Indent(%q) = %q, want %q", in, got, want)
		}
	}
}
