package lock

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/schema"
)

const v1 = `
dynago: 1
package: things
table: { name: things }
entities:
  Thing:
    version: %d
    fields:
      tenantId: string
      thingId: string
      kind: { type: enum, values: [a, b] }
    key: { pk: "T#{tenantId}", sk: "THING#{thingId}" }
%s`

const counter = `    counters:
      Kinds:
        pk: "K#{tenantId}"
        sk: "KINDS"
        values:
          a: { count: true, where: { kind: a } }
`

func model(t *testing.T, version int, extra string) *schema.Model {
	t.Helper()
	src := strings.Replace(strings.Replace(v1, "%d", strconv.Itoa(version), 1), "%s", extra, 1)
	m, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFirstGenerateRecordsVersion(t *testing.T) {
	empty := &File{Dynago: 1, Entities: map[string]*History{}}
	m := model(t, 1, counter)
	next, notes, err := Apply(m, empty, Options{})
	if err != nil || len(notes) != 0 {
		t.Fatalf("apply: %v %v", notes, err)
	}
	if h := next.Entities["Thing"]; len(h.Versions) != 1 || h.Versions[0].Version != 1 {
		t.Fatalf("history = %+v", h)
	}
	if m.Entities[0].Counters[0].Since != 1 {
		t.Fatalf("since = %d", m.Entities[0].Counters[0].Since)
	}
}

func TestShapeChangeNeedsVersionBump(t *testing.T) {
	empty := &File{Dynago: 1, Entities: map[string]*History{}}
	lock1, _, err := Apply(model(t, 1, ""), empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Same version, new counter: refused, with the change named.
	_, _, err = Apply(model(t, 1, counter), lock1, Options{})
	if err == nil || !strings.Contains(err.Error(), "version: 2") || !strings.Contains(err.Error(), "counter Kinds added") {
		t.Fatalf("want a bump error, got %v", err)
	}
	// Versions never go backwards.
	lock2, _, err := Apply(model(t, 2, ""), lock1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply(model(t, 1, ""), lock2, Options{}); err == nil || !strings.Contains(err.Error(), "older than") {
		t.Fatalf("want a backwards error, got %v", err)
	}
}

func TestNewCounterOnlyCountsNewVersions(t *testing.T) {
	empty := &File{Dynago: 1, Entities: map[string]*History{}}
	lock1, _, err := Apply(model(t, 1, ""), empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m2 := model(t, 2, counter)
	lock2, notes, err := Apply(m2, lock1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Items written at v1 never contributed to the counter.
	if got := m2.Entities[0].Counters[0].Since; got != 2 {
		t.Fatalf("since = %d, want 2", got)
	}
	if len(notes) != 1 || !notes[0].Warning || !strings.Contains(notes[0].Message, "needs a backfill") {
		t.Fatalf("notes = %+v", notes)
	}
	// A later version that keeps the counter unchanged keeps its introduction version.
	m3 := model(t, 3, counter)
	if _, _, err := Apply(m3, lock2, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := m3.Entities[0].Counters[0].Since; got != 2 {
		t.Fatalf("since after v3 = %d, want 2", got)
	}
	// Unchanged schema at the same version is a no-op.
	again, notes, err := Apply(model(t, 2, counter), lock2, Options{})
	if err != nil || len(notes) != 0 || len(again.Entities["Thing"].Versions) != 2 {
		t.Fatalf("re-apply: %+v %v %v", again, notes, err)
	}
}

const counterPlus = `    counters:
      Kinds:
        pk: "K#{tenantId}"
        sk: "KINDS"
        values:
          a: { count: true, where: { kind: a } }
          b: { count: true, where: { kind: b } }
`

// Adding a value to an existing counter must not re-gate the values older items already count.
func TestGatingIsPerCounterValue(t *testing.T) {
	empty := &File{Dynago: 1, Entities: map[string]*History{}}
	l1, _, err := Apply(model(t, 1, counter), empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m2 := model(t, 2, counterPlus)
	if _, _, err := Apply(m2, l1, Options{}); err != nil {
		t.Fatal(err)
	}
	c := m2.Entities[0].Counters[0]
	if c.Since != 1 || c.Values[0].Since != 1 || c.Values[1].Since != 2 {
		t.Fatalf("since: counter %d, a %d, b %d", c.Since, c.Values[0].Since, c.Values[1].Since)
	}
}

func TestInPlaceChangesAreRefused(t *testing.T) {
	empty := &File{Dynago: 1, Entities: map[string]*History{}}
	l1, _, err := Apply(model(t, 1, counter), empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(counter, "where: { kind: a }", "where: { kind: b }", 1)
	if _, _, err := Apply(model(t, 2, changed), l1, Options{}); err == nil || !strings.Contains(err.Error(), "cannot change what it counts in place") {
		t.Fatalf("want refusal, got %v", err)
	}
	moved := strings.Replace(counter, `pk: "K#{tenantId}"`, `pk: "K2#{tenantId}"`, 1)
	if _, _, err := Apply(model(t, 2, moved), l1, Options{}); err == nil || !strings.Contains(err.Error(), "cannot change its keys") {
		t.Fatalf("want refusal, got %v", err)
	}
}

// A lost lock file must not silently restart history: everything would look introduced at the
// current version, and deleting older items would never release what they contributed.
func TestMissingHistoryAboveVersionOneIsRefused(t *testing.T) {
	empty := &File{Dynago: 1, Entities: map[string]*History{}}
	_, _, err := Apply(model(t, 3, counter), empty, Options{})
	if err == nil || !strings.Contains(err.Error(), "no history for it") {
		t.Fatalf("got %v", err)
	}
	next, _, err := Apply(model(t, 3, counter), empty, Options{NewHistory: true})
	if err != nil || next.Entities["Thing"].Versions[0].Version != 3 {
		t.Fatalf("with NewHistory: %v", err)
	}
}

func TestLateCallerLimitOnARequiredEntityIsRefused(t *testing.T) {
	src := func(version int, value string) *schema.Model {
		m, err := schema.Parse([]byte(fmt.Sprintf(`
dynago: 1
package: things
table: { name: things }
entities:
  Box:
    version: %d
    fields:
      boxId: string
      state: { type: enum, values: [empty, full] }
    key: { pk: "BOX#{boxId}", sk: "BOX" }
    counters:
      Boxes: { pk: "BOXES", sk: "C", values: { empty: count%s } }
  Fill:
    fields:
      boxId: string
    key: { pk: "FILL#{boxId}", sk: "FILL" }
    writes:
      Do: { create: true, requires: { Box: { key: { boxId: boxId }, when: { state: empty }, set: { state: full } } } }
`, version, value)))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	l1, _, err := Apply(src(1, ""), &File{Dynago: 1, Entities: map[string]*History{}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Apply(src(2, ", all: { count: true, limit: arg }"), l1, Options{})
	if err == nil || !strings.Contains(err.Error(), "Fill.Do would fail with ErrLimitRequired") {
		t.Fatalf("got %v", err)
	}
}

// Reordering fields changes nothing stored, so it needs no version bump.
func TestFieldOrderIsNotAShapeChange(t *testing.T) {
	empty := &File{Dynago: 1, Entities: map[string]*History{}}
	l1, _, err := Apply(model(t, 1, counter), empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := model(t, 1, counter)
	e := m.Entities[0]
	e.Fields[0], e.Fields[len(e.Fields)-1] = e.Fields[len(e.Fields)-1], e.Fields[0]
	if _, _, err := Apply(m, l1, Options{}); err != nil {
		t.Fatalf("reordered fields: %v", err)
	}
}

func TestShapeRecordsTheTTLAttribute(t *testing.T) {
	a := Shape{TTL: "expiresAt", TTLAttr: "ttl"}
	b := Shape{TTL: "expiresAt", TTLAttr: "expires"}
	if d := strings.Join(Diff(a, b), "\n"); !strings.Contains(d, `TTL attribute changed from "ttl" to "expires"`) {
		t.Fatalf("diff = %s", d)
	}
}

func TestGSIProjectionChangeIsARebuild(t *testing.T) {
	a := Shape{Indexes: []IndexShape{{Name: "ByName", Strategy: "gsi", PK: "N#{name}", Projection: "include", Project: []string{"a"}}}}
	b := Shape{Indexes: []IndexShape{{Name: "ByName", Strategy: "gsi", PK: "N#{name}", Projection: "include", Project: []string{"a", "b"}}}}
	if d := strings.Join(Diff(a, b), "\n"); !strings.Contains(d, "deleted and re-created") {
		t.Fatalf("diff = %s", d)
	}
}
