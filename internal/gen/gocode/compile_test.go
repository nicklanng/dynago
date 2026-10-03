package gocode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/schema"
)

// Schemas that once generated Go that didn't compile. Each is either refused by the resolver with
// the given message, or generates code that must build and vet: names chosen by the generator
// (locals, parameters) must never collide with names from the schema.
var awkward = []struct {
	name, entity, refused string
}{
	{"field named version", "      version: int\n", "Version() method"},
	{"enum values differing in case", "      mood: { type: enum, values: [Open, open] }\n", "same Go name"},
	{"enum values differing in separator", "      mood: { type: enum, values: [in-progress, in_progress] }\n", "same Go name"},
	{"enum value with symbols", "      lang: { type: enum, values: [c++, go] }\n", ""},
	{"enum value of symbols only", "      lang: { type: enum, values: [\"++\"] }\n", "no letters or digits"},
	{"fields differing in initialism case", "      userId: string\n      userID: string\n", "same Go name"},
	{"field shadowing an item attribute", "      rev: int\n", "used by the generated item"},
	{"when on a key field", "      W: { update: [name], when: { thingId: x } }\n", "primary key field"},
	{"percent in a when value", "      W: { update: [name], when: { note: \"50% off\" } }\n", ""},
	{"unique fields named like locals", "      claim: string\n      it: string\n      fmt: string\n    unique:\n      U: { fields: [claim, it, fmt] }\n    access:\n      ByU: { get: { unique: U } }\n", ""},
	{"unique set, lowered and not last in its key", "      emails: string_set\n    unique:\n      Email: { fields: [tenantId, emails], pk: \"E#{emails|lower}#T#{tenantId}\" }\n    access:\n      ByEmail: { get: { unique: Email } }\n", ""},
	{"scans, one of an expiring entity", "      until: time\n    ttl: until\n    access:\n      All: { scan: true, reason: nightly export, consistent: true }\n      Page: { scan: true, page: 10, max_page: 20, reason: backfill }\n", ""},
	{"not and in on every type", "      open: bool\n      rank: int\n      mood: { type: enum, values: [calm, cross, glad] }\n" +
		"    indexes:\n      Live: { pk: \"L#{tenantId}\", sk: \"T#{thingId}\", project: keys, where: { open: { not: false }, rank: { in: [1, 2] }, note: { not: \"\" }, mood: { not: cross } } }\n" +
		"      Kept: { strategy: copy, pk: \"K#{tenantId}\", sk: \"T#{thingId}\", project: [name], where: { name: { in: [\"a && b\", \"c || d\"] } } }\n" +
		"    counters:\n      Moods: { pk: \"M#{tenantId}\", sk: \"MOODS\", values: { happy: { count: true, where: { mood: { in: [calm, glad] }, rank: { not: 0 } } } } }\n" +
		"    access:\n      Get: get\n      Live: { query: Live }\n      Kept: { query: Kept }\n", ""},
	{"not and in as preconditions", "      W: { update: [name], when: { name: { not: \"x && y\" }, note: { in: [\"\", \"50% || 60%\"] } } }\n", ""},
	{"batch get of an expiring entity, and counters read together", "      until: time\n      mood: { type: enum, values: [calm, cross] }\n    ttl: until\n" +
		"    counters:\n      Moods: { pk: \"M#{tenantId}\", sk: \"MOOD#{mood}#N#{name}#END\", values: { key: count, things: count } }\n" +
		"    access:\n      Several: { get: key, batch: 25 }\n      Moods: { counter: Moods, all: true, page: 10 }\n      Mood: { counter: Moods }\n", ""},
	{"range query key field named from", "      from: string\n    indexes:\n      ByFrom: { pk: \"F#{from}\", sk: \"AT#{at}\", project: keys }\n    access:\n      L: { query: ByFrom, range: at }\n", "collides with the range bound"},
}

const awkwardBase = `
dynago: 1
package: %s
table: { name: things }
entities:
  Thing:
    fields:
      tenantId: string
      thingId: string
      name: string
      note: string
      at: time
%s    key: { pk: "T#{tenantId}", sk: "THING#{thingId}" }
    writes:
      Make: create
%s`

// Entity-level collisions need a second entity.
var awkwardEntities = []struct {
	name, extra, refused string
}{
	{"requires with not and in", "  Part:\n    fields:\n      tenantId: string\n      thingId: string\n      partId: string\n      label: string\n" +
		"    key: { pk: \"T#{tenantId}\", sk: \"PART#{thingId}#{partId}\" }\n    writes:\n" +
		"      Add: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, when: { name: { not: \"{label}\" }, note: { in: [a, b] } } } } }\n" +
		"      Mark: { update: [label], requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, when: { note: { not: gone } }, set: { name: \"{label}\" } } } }\n", ""},
	{"requires with add and patch of every type", "  Box:\n    fields:\n      tenantId: string\n      boxId: string\n      parts: int\n      weight: int\n      name: string\n      at: time\n      note: string\n" +
		"    key: { pk: \"T#{tenantId}\", sk: \"BOX#{boxId}\" }\n    counters:\n      Weights: { pk: \"T#{tenantId}\", sk: \"WEIGHTS\", values: { total: { sum: weight } } }\n    writes:\n      Make: create\n" +
		"  Part:\n    fields:\n      tenantId: string\n      boxId: string\n      partId: string\n      weight: int\n      seen: time\n      label: string\n" +
		"    key: { pk: \"T#{tenantId}\", sk: \"PART#{boxId}#{partId}\" }\n    writes:\n" +
		"      Add: { create: true, requires: { Box: { key: { tenantId: tenantId, boxId: boxId }, add: { parts: 1, weight: \"{weight}\" }, patch: { name: \"{label}\", at: \"{seen}\" } } } }\n" +
		"      Drop: { delete: true, requires: { Box: { key: { tenantId: tenantId, boxId: boxId }, add: { parts: -1 }, when: { note: kept } } } }\n", ""},
	{"partition read of an expiring entity and a singleton", "  Summary:\n    fields:\n      tenantId: string\n      until: time\n    ttl: until\n    key: { pk: \"T#{tenantId}\", sk: \"SUMMARY\" }\n" +
		"    access:\n      Whole: { query: partition, of: [Thing, Summary], order: desc, page: 10, max_page: 20 }\n      Mine: { query: partition, of: [Summary] }\n", ""},
	{"requires that create their target", "  Crate:\n    fields:\n      tenantId: string\n      crateId: string\n      label: { type: string, required: true }\n      emails: string_set\n      parts: int\n      until: time\n      mood: { type: enum, values: [calm, cross] }\n" +
		"    ttl: until\n    key: { pk: \"T#{tenantId}\", sk: \"CRATE#{crateId}\" }\n" +
		"    indexes:\n      ByLabel: { strategy: copy, pk: \"L#{tenantId}\", sk: \"L#{label}#{crateId}\", project: [parts] }\n" +
		"    unique:\n      Label: { fields: [tenantId, label] }\n" +
		"    counters:\n      Crates: { pk: \"T#{tenantId}\", sk: \"CRATES\", values: { n: count, calm: { count: true, where: { mood: calm } } } }\n" +
		"  Part:\n    fields:\n      tenantId: string\n      crateId: string\n      partId: string\n      label: string\n      seen: time\n" +
		"    key: { pk: \"T#{tenantId}\", sk: \"PART#{crateId}#{partId}\" }\n    writes:\n" +
		"      Add: { create: true, requires: { Crate: { key: { tenantId: tenantId, crateId: crateId }, ensure: { label: \"{label}\", mood: calm }, add: { parts: 1 }, patch: { until: \"{seen}\" }, when: { mood: { not: cross } } } } }\n" +
		"      Move: { update: [label], requires: { Crate: { key: { tenantId: tenantId, crateId: crateId }, ensure: { label: unlabelled } } } }\n" +
		"      Drop: { delete: true, requires: { Crate: { key: { tenantId: tenantId, crateId: crateId }, ensure: { label: \"{label}\" }, set: { mood: cross } } } }\n", ""},
	{"entity named like another's store", "  ThingStore:\n    fields: { id: string }\n    key: { pk: \"S#{id}\", sk: \"S\" }\n", "collides"},
	{"entities differing in initialism case", "  THING:\n    fields: { id: string }\n    key: { pk: \"U#{id}\", sk: \"U\" }\n", "collides"},
	{"entity named Store", "  Store:\n    fields: { id: string }\n    key: { pk: \"S#{id}\", sk: \"S\" }\n", "collides"},
}

func TestAwkwardSchemasCompileOrAreRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds generated packages with the go command")
	}
	root := filepath.Join("testdata", "compile")
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	var dirs []string
	check := func(name, src, refused string, adjust ...func(*schema.Model)) {
		t.Helper()
		m, err := schema.Parse([]byte(src))
		if err == nil {
			for _, f := range adjust {
				f(m)
			}
		}
		switch {
		case refused != "" && (err == nil || !strings.Contains(err.Error(), refused)):
			t.Errorf("%s: want an error containing %q, got %v", name, refused, err)
			return
		case refused != "":
			return
		case err != nil:
			t.Errorf("%s: %v", name, err)
			return
		}
		out, err := Generate(m, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		dir := filepath.Join(root, m.Package)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gen.go"), out, 0o644); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, "./"+filepath.ToSlash(dir))
	}
	pkg := func(i int) string { return "awkward" + string(rune('a'+i)) }
	fill := func(pkg, entity, writes string) string {
		return strings.Replace(strings.Replace(strings.Replace(awkwardBase, "%s", pkg, 1), "%s", entity, 1), "%s", writes, 1)
	}
	for i, c := range awkward {
		entity, writes := c.entity, ""
		if strings.HasPrefix(c.entity, "      W:") {
			entity, writes = "", c.entity
		}
		check(c.name, fill(pkg(i), entity, writes), c.refused)
	}
	for i, c := range awkwardEntities {
		src := fill("ent"+pkg(i), "", "")
		check(c.name, src+c.extra, c.refused)
	}
	// A new generation in which no entity carries over from the previous one (all renamed).
	check("nothing carries over", fill("migratenone", "", ""), "", func(m *schema.Model) {
		m.Table.Generation = 2
		m.Previous = &schema.Previous{Generation: 1, Entities: map[string][]schema.PreviousField{}}
	})
	if len(dirs) == 0 {
		return
	}
	cmd := exec.Command("go", append([]string{"vet"}, dirs...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated code does not build:\n%s", out)
	}
}
