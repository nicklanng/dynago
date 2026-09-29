package gocode

import (
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/schema"
)

const src = `
dynago: 1
package: things
table: { name: things }
entities:
  Thing:
    version: %s
    fields:
      tenantId: string
      thingId: string
      kind: { type: enum, values: [a, b] }
    key: { pk: "T#{tenantId}", sk: "THING#{thingId}" }
%s    writes:
      Create: create
      SetKind: { update: [kind] }
      Delete: delete
`

const counter = `    counters:
      Kinds:
        pk: "K#{tenantId}"
        sk: "KINDS"
        values:
          a: { count: true, where: { kind: a } }
`

func parse(t *testing.T, version, extra string) *schema.Model {
	t.Helper()
	m, err := schema.Parse([]byte(strings.Replace(strings.Replace(src, "%s", version, 1), "%s", extra, 1)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Read-modify-writes keep attributes they don't know: within a table generation, newer
// compatible code may have written fields this code has never heard of.
func TestRewritesKeepUnknownAttributes(t *testing.T) {
	m := parse(t, "1", counter)
	out, err := Generate(m, "things.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	code := string(out)
	for _, want := range []string{
		`var thingKnown = map[string]bool{"PK": true, "SK": true, "_t": true, "_v": true, "_rev": true, "tenantId": true, "thingId": true, "kind": true}`,
		"item, err := dynago.KeepUnknown(it.raw, thingKnown, thingToItem(&after, key, it.Rev+1))",
		`s.t.Put(item).If("$ = ?", "_rev", it.Rev)`,
		// Revisions start random so a delete + re-create cannot satisfy a stale revision guard.
		"rev := dynago.NewRev()",
		"dynago.CreateOp(key, put, ErrThingExists, rev)",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
	if strings.Contains(code, "ErrNewerSchema") || strings.Contains(code, "AtLeastVersion") {
		t.Error("generated code still refuses items written by newer code")
	}
}

func TestFastPathWhenNothingDerivedChanges(t *testing.T) {
	m := parse(t, "1", "")
	out, err := Generate(m, "things.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	code := string(out)
	if !strings.Contains(code, "dynago.UpdateFields(ctx, s.t, key, sets, nil, guard, nil)") {
		t.Error("SetKind should be a single UpdateItem when nothing derives from kind")
	}
	if !strings.Contains(code, "dynago.DeleteIfExists(ctx, s.t, key, guard)") {
		t.Error("Delete should be a single conditional DeleteItem when nothing derives from the entity")
	}
}

// An update whose counter change is fixed by `when` and `set` runs without reading the item.
func TestReadFreeTransition(t *testing.T) {
	src := strings.Replace(strings.Replace(src, "%s", "1", 1), "%s", counter, 1)
	src = strings.Replace(src, "      SetKind: { update: [kind] }", "      SetKind: { update: [kind] }\n      MakeA: { set: { kind: a }, when: { kind: b } }", 1)
	m, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var makeA, setKind *schema.Write
	for _, w := range m.Entities[0].Writes {
		switch w.Name {
		case "MakeA":
			makeA = w
		case "SetKind":
			setKind = w
		}
	}
	if !makeA.Transition || setKind.Transition || !setKind.ReadFirst {
		t.Fatalf("MakeA transition=%v, SetKind transition=%v readFirst=%v", makeA.Transition, setKind.Transition, setKind.ReadFirst)
	}
	out, err := Generate(m, "things.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "before := Thing{TenantID: k.TenantID, ThingID: k.ThingID, Kind: ThingKindB}") {
		t.Error("MakeA should build its before state from the key and `when`")
	}
}

func TestArticle(t *testing.T) {
	for word, want := range map[string]string{"User": "a", "Event": "an", "Unique": "a", "Update": "an", "Hold": "a", "Item": "an"} {
		if got := article(word); got != want {
			t.Errorf("article(%q) = %q, want %q", word, got, want)
		}
	}
}

const linked = `
dynago: 1
package: shop
table: { name: shop }
entities:
  Box:
    fields:
      shopId: string
      boxId: string
    key: { pk: "S#{shopId}", sk: "BOX#{boxId}" }
    writes:
      Add: create
  Parcel:
    fields:
      shopId: string
      parcelId: string
      boxId: string
      courierId: string
    key: { pk: "S#{shopId}", sk: "PARCEL#{parcelId}" }
    indexes:
      InBox: { strategy: copy, pk: "S#{shopId}#BOX#{boxId}#P", sk: "P#{parcelId}", project: keys }
    writes:
      Pack:
        create: true
        requires:
          Box: { key: { shopId: shopId, boxId: boxId } }
          Courier: { key: { shopId: shopId, courierId: courierId }, optional: true, when: { active: true } }
  Courier:
    fields:
      shopId: string
      courierId: string
      active: bool
    key: { pk: "S#{shopId}", sk: "COURIER#{courierId}" }
`

func generateLinked(t *testing.T) string {
	t.Helper()
	m, err := schema.Parse([]byte(linked))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate(m, "linked")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// An empty field keying a requirement must fail the write, not skip the rule; only an optional
// requirement is skipped.
func TestRequirementsWithEmptyKeysFail(t *testing.T) {
	out := generateLinked(t)
	if !strings.Contains(out, `if e.ShopID == "" || e.BoxID == "" {`) || !strings.Contains(out, `Parcel.Pack requires the Box, keyed by shopId and boxId", dynago.ErrFieldRequired)`) {
		t.Error("Pack does not refuse a parcel without a box")
	}
	if !strings.Contains(out, `if e.ShopID != "" && e.CourierID != "" {`) {
		t.Error("Pack should skip its optional courier requirement when the parcel names no courier")
	}
}

// Box.boxId is last in the box's own key, but not in the parcel's InBox copy key. The parcel
// requires the box by boxId, so the box must refuse "#" in boxId when it is created.
func TestSeparatorsFollowRequiresLinks(t *testing.T) {
	out := generateLinked(t)
	start := strings.Index(out, "func (e *Box) checkKeyParts() error {")
	end := strings.Index(out[start:], "\n}\n")
	if !strings.Contains(out[start:start+end], `dynago.CheckKeyPart("boxId", e.BoxID, "#")`) {
		t.Errorf("Box does not check boxId:\n%s", out[start:start+end])
	}
}

func TestNegate(t *testing.T) {
	for in, want := range map[string]string{
		`k.A != "" && k.B != ""`:        `k.A == "" || k.B == ""`,
		`k.A != "" && !k.At.IsZero()`:   `k.A == "" || k.At.IsZero()`,
		`e.Status == LoanStatusActive`:  `e.Status != LoanStatusActive`,
		`e.Name == "a && b" && e.On`:    `!(e.Name == "a && b" && e.On)`,
		`e.On`:                          `!e.On`,
		`!e.On && e.Kind == ThingKindA`: `e.On || e.Kind != ThingKindA`,
	} {
		if got := negate(in); got != want {
			t.Errorf("negate(%s) = %s, want %s", in, got, want)
		}
	}
}

// versioned: required holds on the read-free path too, not only when the item is read.
func TestReadFreeWriteRequiresAVersion(t *testing.T) {
	src := strings.Replace(strings.Replace(src, "%s", "1", 1), "%s", counter, 1)
	src = strings.Replace(src, "      SetKind: { update: [kind] }", "      SetKind: { update: [kind] }\n      MakeA: { set: { kind: a }, when: { kind: b }, versioned: required }", 1)
	m, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate(m, "things.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	code := string(out)
	start := strings.Index(code, "func (s *ThingStore) MakeA(")
	fast := code[start : start+strings.Index(code[start:], "err = dynago.Retry")]
	if !strings.Contains(fast, "if expect == 0 {\n\t\t\treturn dynago.ErrVersionRequired") {
		t.Errorf("MakeA's read-free path doesn't require a version:\n%s", fast)
	}
}

// Each element of a unique set gets its own claim, and elements are checked for the separator
// that follows the set in the claim key.
func TestUniqueSetClaimsEachElement(t *testing.T) {
	src := strings.Replace(strings.Replace(src, "%s", "1", 1), "%s", `    unique:
      Alias: { fields: [tenantId, aliases], pk: "A#{aliases|lower}#T#{tenantId}" }
`, 1)
	src = strings.Replace(src, "      kind: { type: enum, values: [a, b] }", "      kind: { type: enum, values: [a, b] }\n      aliases: string_set", 1)
	m, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate(m, "things.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	code := string(out)
	for _, want := range []string{
		"for _, elem := range e.Aliases {",
		`Key: dynago.Key{PK: "A#" + dynago.Lower(elem) + "#T#" + e.TenantID, SK: "UNIQUE"}`,
		`if err := dynago.CheckKeyPart("aliases", elem, "#"); err != nil {`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
}
