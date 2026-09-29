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
	check := func(name, src, refused string) {
		t.Helper()
		m, err := schema.Parse([]byte(src))
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
	if len(dirs) == 0 {
		return
	}
	cmd := exec.Command("go", append([]string{"vet"}, dirs...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated code does not build:\n%s", out)
	}
}
