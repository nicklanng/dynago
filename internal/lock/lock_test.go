package lock

import (
	"strconv"
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/schema"
)

const v1 = `
dynago: 1
package: things
table: { name: things, generation: %g }
entities:
  Thing:
    version: %d
    fields:
      tenantId: string
      thingId: string
      kind: { type: enum, values: [a, b] }
%f    key: { pk: "T#{tenantId}", sk: "THING#{thingId}" }
%s`

const counter = `    counters:
      Kinds:
        pk: "K#{tenantId}"
        sk: "KINDS"
        values:
          a: { count: true, where: { kind: a } }
`

// model parses the test schema at a table generation and entity version, with extra fields and
// extra entity sections.
func model(t *testing.T, gen, version int, fields, extra string) *schema.Model {
	t.Helper()
	r := strings.NewReplacer("%g", strconv.Itoa(gen), "%d", strconv.Itoa(version), "%f", fields, "%s", extra)
	m, err := schema.Parse([]byte(r.Replace(v1)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func empty() *File { return &File{Dynago: 1, Entities: map[string]*History{}} }

func apply(t *testing.T, m *schema.Model, prev *File) *File {
	t.Helper()
	next, _, err := Apply(m, prev, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestFirstGenerateRecordsVersionAndTable(t *testing.T) {
	next := apply(t, model(t, 1, 1, "", counter), empty())
	if h := next.Entities["Thing"]; len(h.Versions) != 1 || h.Versions[0].Version != 1 || h.Versions[0].gen() != 1 {
		t.Fatalf("history = %+v", h)
	}
	if next.Generation != 1 || next.Table(1) == nil {
		t.Fatalf("generation %d, tables %+v", next.Generation, next.Tables)
	}
}

func TestShapeChangeNeedsVersionBump(t *testing.T) {
	l1 := apply(t, model(t, 1, 1, "", ""), empty())
	if _, _, err := Apply(model(t, 1, 1, "      note: string\n", ""), l1, Options{}); err == nil || !strings.Contains(err.Error(), "version: 2") {
		t.Fatalf("got %v", err)
	}
	l2 := apply(t, model(t, 1, 2, "      note: string\n", ""), l1)
	if _, _, err := Apply(model(t, 1, 1, "", ""), l2, Options{}); err == nil || !strings.Contains(err.Error(), "older than") {
		t.Fatalf("got %v", err)
	}
}

// Changes existing items still fit stay in the table generation; others need a new one.
func TestCompatibleChangesStayInTheGeneration(t *testing.T) {
	l1 := apply(t, model(t, 1, 1, "      old: string\n", counter), empty())
	for name, fields := range map[string]string{
		"optional field added": "      old: string\n      note: string\n",
		"field removed":        "",
	} {
		if _, _, err := Apply(model(t, 1, 2, fields, counter), l1, Options{}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := Apply(model(t, 1, 2, "      old: string\n", ""), l1, Options{}); err != nil {
		t.Errorf("counter removed: %v", err)
	}
}

func TestIncompatibleChangesNeedANewGeneration(t *testing.T) {
	l1 := apply(t, model(t, 1, 1, "      old: string\n", ""), empty())
	for name, c := range map[string]struct{ fields, extra, want string }{
		"counter added":        {"      old: string\n", counter, "counter value Kinds.a added"},
		"required field added": {"      old: string\n      must: { type: string, required: true }\n", "", "field must added as required"},
		"field type changed":   {"      old: int\n", "", "field old changed"},
		"attribute renamed":    {"      old: { type: string, attr: o }\n", "", "field old changed"},
		"unique claim added":   {"      old: string\n", "    unique:\n      Old: { fields: [old] }\n", "unique claim Old added"},
	} {
		_, _, err := Apply(model(t, 1, 2, c.fields, c.extra), l1, Options{})
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "table.generation: 2") {
			t.Errorf("%s: got %v", name, err)
		}
		// In a new generation the same change is fine, and the previous shapes are known.
		m := model(t, 2, 2, c.fields, c.extra)
		if _, _, err := Apply(m, l1, Options{}); err != nil {
			t.Errorf("%s in a new generation: %v", name, err)
			continue
		}
		if m.Previous == nil || m.Previous.Generation != 1 || len(m.Previous.Entities["Thing"]) != 4 {
			t.Errorf("%s: previous = %+v", name, m.Previous)
		}
	}
	src := strings.Replace(v1, "values: [a, b]", "values: [a]", 1)
	m, err := schema.Parse([]byte(strings.NewReplacer("%g", "1", "%d", "2", "%f", "      old: string\n", "%s", "").Replace(src)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply(m, l1, Options{}); err == nil || !strings.Contains(err.Error(), "enum kind no longer has b") {
		t.Errorf("enum value removed: got %v", err)
	}
}

func TestGenerationsGoUpByOne(t *testing.T) {
	l1 := apply(t, model(t, 1, 1, "", ""), empty())
	if _, _, err := Apply(model(t, 3, 1, "", ""), l1, Options{}); err == nil || !strings.Contains(err.Error(), "must go up by one") {
		t.Fatalf("got %v", err)
	}
	l2 := apply(t, model(t, 2, 1, "", ""), l1)
	if _, _, err := Apply(model(t, 1, 1, "", ""), l2, Options{}); err == nil || !strings.Contains(err.Error(), "only go up") {
		t.Fatalf("got %v", err)
	}
	if l2.Table(1) == nil || l2.Table(2) == nil {
		t.Fatalf("tables = %+v", l2.Tables)
	}
}

func TestRetainNeedsARecordedTable(t *testing.T) {
	src := strings.Replace(v1, "generation: %g }", "generation: %g, retain: [1] }", 1)
	m, err := schema.Parse([]byte(strings.NewReplacer("%g", "2", "%d", "1", "%f", "", "%s", "").Replace(src)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply(m, empty(), Options{NewHistory: true}); err == nil || !strings.Contains(err.Error(), "no record of generation 1") {
		t.Fatalf("got %v", err)
	}
}

// A lost lock file must not silently restart the history.
func TestMissingHistoryIsRefused(t *testing.T) {
	if _, _, err := Apply(model(t, 1, 3, "", counter), empty(), Options{}); err == nil || !strings.Contains(err.Error(), "no history for it") {
		t.Fatalf("got %v", err)
	}
	if _, _, err := Apply(model(t, 2, 1, "", ""), empty(), Options{}); err == nil || !strings.Contains(err.Error(), "generation 2, but the lock file has no history") {
		t.Fatalf("got %v", err)
	}
	next, _, err := Apply(model(t, 1, 3, "", counter), empty(), Options{NewHistory: true})
	if err != nil || next.Entities["Thing"].Versions[0].Version != 3 {
		t.Fatalf("with NewHistory: %v", err)
	}
}

// Reordering fields changes nothing stored, so it needs no version bump.
func TestFieldOrderIsNotAShapeChange(t *testing.T) {
	l1 := apply(t, model(t, 1, 1, "", counter), empty())
	m := model(t, 1, 1, "", counter)
	e := m.Entities[0]
	e.Fields[0], e.Fields[len(e.Fields)-1] = e.Fields[len(e.Fields)-1], e.Fields[0]
	if _, _, err := Apply(m, l1, Options{}); err != nil {
		t.Fatalf("reordered fields: %v", err)
	}
}

func TestShapeRecordsTheTTLAttribute(t *testing.T) {
	a := Shape{TTL: "expiresAt", TTLAttr: "ttl"}
	b := Shape{TTL: "expiresAt", TTLAttr: "expires"}
	cs := Changes(a, b, nil)
	if len(cs) != 1 || cs[0].Compatible || !strings.Contains(cs[0].Text, `TTL attribute changed from "ttl" to "expires"`) {
		t.Fatalf("changes = %+v", cs)
	}
}

// A lock written before generations existed reads as generation 1.
func TestLocksFromBeforeGenerations(t *testing.T) {
	l1 := apply(t, model(t, 1, 1, "", ""), empty())
	l1.Generation, l1.Tables = 0, nil
	for i := range l1.Entities["Thing"].Versions {
		l1.Entities["Thing"].Versions[i].Generation = 0
	}
	next := apply(t, model(t, 1, 1, "", ""), l1)
	if next.Generation != 1 || next.Entities["Thing"].Versions[0].gen() != 1 {
		t.Fatalf("upgraded lock = %+v", next)
	}
}
