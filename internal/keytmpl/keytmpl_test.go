package keytmpl

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tpl, err := Parse("TENANT#{tenantId}#EVENT#{eventId}")
	if err != nil {
		t.Fatal(err)
	}
	want := []Segment{{Literal: "TENANT#"}, {Field: "tenantId"}, {Literal: "#EVENT#"}, {Field: "eventId"}}
	if !reflect.DeepEqual(tpl.Segments, want) {
		t.Fatalf("segments = %#v", tpl.Segments)
	}
	if got := tpl.Fields(); !reflect.DeepEqual(got, []string{"tenantId", "eventId"}) {
		t.Fatalf("fields = %v", got)
	}
	if got := tpl.LiteralPrefix(); got != "TENANT#" {
		t.Fatalf("prefix = %q", got)
	}
	if got := tpl.Render(map[string]string{"tenantId": "t1"}); got != "TENANT#t1#EVENT#{eventId}" {
		t.Fatalf("render = %q", got)
	}
}

func TestTransform(t *testing.T) {
	tpl := MustParse("MEMBERS#{name|lower}#{id}")
	if tpl.Segments[1] != (Segment{Field: "name", Transform: "lower"}) || tpl.Fields()[0] != "name" {
		t.Fatalf("segments = %#v", tpl.Segments)
	}
	if got := tpl.Render(map[string]string{"name": "Zoe Q", "id": "M1"}); got != "MEMBERS#zoe q#M1" {
		t.Fatalf("render = %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, raw := range []string{"", "A#{a}{b}", "A#{", "A#{}", "A#}", "{a b}", "A#{a|shout}", "A#{|lower}"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q): expected error", raw)
		}
	}
}

func TestConstant(t *testing.T) {
	tpl := MustParse("STATS")
	if !tpl.IsConstant() || tpl.LiteralPrefix() != "STATS" || tpl.FirstField() != "" {
		t.Fatalf("constant template misparsed: %#v", tpl)
	}
}

func TestOverlap(t *testing.T) {
	cases := []struct {
		a, b    string
		overlap bool
		prefix  bool
	}{
		{"TENANT#{t}", "TENANT#{t}#EVENT#{e}", true, true},
		{"USER#{u}", "TENANT#{t}", false, false},
		{"REG#{u}", "EVENT", false, false},
		{"EVENT", "EVENT", true, true},
		{"EVENT", "EVENTS#{x}", false, true},
		{"STATS", "S#{x}", false, false},
		{"S", "S#{x}", false, true},
	}
	for _, c := range cases {
		a, b := MustParse(c.a), MustParse(c.b)
		if got := MayOverlap(a, b); got != c.overlap {
			t.Errorf("MayOverlap(%q, %q) = %v", c.a, c.b, got)
		}
		if got := PrefixCollides(a, b); got != c.prefix {
			t.Errorf("PrefixCollides(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
