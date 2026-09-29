package schema

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestExamplesLoad(t *testing.T) {
	paths, _ := filepath.Glob("../../examples/*/*.dynago.yaml")
	if len(paths) == 0 {
		t.Fatal("no example schemas found")
	}
	for _, p := range paths {
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestResolvedModel(t *testing.T) {
	m, err := Load("../../examples/toollibrary/toollibrary.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writes := map[string]*Write{}
	for _, e := range m.Entities {
		for _, w := range e.Writes {
			writes[e.Name+"."+w.Name] = w
		}
	}
	for name, want := range map[string]struct{ readFirst, transition bool }{
		"Loan.Borrow":          {false, false}, // a create never reads
		"Member.SetLoanCap":    {false, false}, // feeds nothing derived: one UpdateItem
		"Member.Reinstate":     {true, false},  // the stewards count depends on role, which it doesn't pin
		"Member.UpdateProfile": {true, false},  // a patch of an index key field
		"Loan.Return":          {true, false},  // moves the loan out of the ByMember copy index
		"Tool.Retire":          {true, true},   // counters fixed by when/set, and the Hold's key is known
		"Tool.EditDetails":     {true, false},
	} {
		w := writes[name]
		if w.ReadFirst != want.readFirst || w.Transition != want.transition {
			t.Errorf("%s: readFirst=%v transition=%v, want %+v", name, w.ReadFirst, w.Transition, want)
		}
	}
	// Only writes that can grow a limit:arg value take its limit.
	if len(writes["Loan.Borrow"].Limits) != 1 || len(writes["Loan.Return"].Limits) != 0 || len(writes["Loan.Extend"].Limits) != 0 {
		t.Error("limit planning wrong")
	}
	rq := writes["Loan.Borrow"].Requires
	if len(rq) != 3 || rq[0].Target.Name != "Member" || rq[1].Target.Name != "Tool" || rq[2].Target.Name != "Hold" {
		t.Fatalf("requires = %+v", rq)
	}
	switch {
	case rq[0].Writes() || !rq[1].Writes() || !rq[2].Writes():
		t.Error("Borrow checks the Member, and changes the Tool and the Hold")
	case !rq[1].Fast:
		t.Error("the Tool's counter change is known from when/set, so it needs no read")
	case !rq[2].Fast || !rq[2].Optional || rq[2].When[0].Source == nil || rq[2].When[0].Source.Name != "memberId":
		t.Errorf("the Hold is optional, consumed without a read, and must be the borrower's: %+v", rq[2])
	}
	if lv := writes["Member.Leave"].Requires; len(lv) != 1 || lv[0].Counter == nil || lv[0].Counter.Name != "MemberLoans" || lv[0].CounterWhen[0].Equals != 0 {
		t.Errorf("Leave requires = %+v", lv)
	}
	for _, g := range m.GSIs {
		if g.Name == "ByCode" && !contains(g.NonKeyAttrs, "ttl") {
			t.Errorf("ByCode must project the TTL attribute so queries can leave out expired holds: %v", g.NonKeyAttrs)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

const base = `
dynago: 1
package: things
table: { name: things }
entities:
  Thing:
    fields:
      tenantId: string
      thingId: string
      name: string
      count: int
      status: { type: enum, values: [a, b] }
      at: time
      tags: string_set
    key: { pk: "T#{tenantId}", sk: "THING#{thingId}" }
`

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name, extra, want string
	}{
		{"unknown field type", "      bad: widget\n", `unknown type "widget"`},
		{"template field missing", "    indexes:\n      ByX: { pk: \"X#{nope}\", project: keys }\n", "{nope} is not a field"},
		{"typo in key", "    acess: {}\n", `unknown key "acess"`},
		{"index without project", "    indexes:\n      ByName: { pk: \"N#{name}\", sk: \"T#{thingId}\" }\n", "project is required"},
		{"projected field unknown", "    indexes:\n      ByName: { pk: \"N#{name}\", project: [nope] }\n", "projected field nope"},
		{"list field in key", "    indexes:\n      ByTag: { pk: \"N#{tags}\", project: keys }\n", "cannot be part of a key"},
		{"consistent gsi", "    indexes:\n      ByName: { pk: \"N#{name}\", project: keys }\n    access:\n      L: { query: ByName, consistent: true }\n", "cannot be read consistently"},
		{"range not after prefix", "    access:\n      L: { query: key, range: name }\n", "must be the first placeholder"},
		{"update key field", "    writes:\n      W: { update: [thingId] }\n", "part of the primary key"},
		{"when type mismatch", "    writes:\n      W: { update: [name], when: { count: yes } }\n", "does not match field type"},
		{"enum value", "    writes:\n      W: { set: { status: c } }\n", `"c" is not one of`},
		{"sum of non-int", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", values: { n: { sum: name } } }\n", "must be an int"},
		{"sharded limit", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", shards: 4, values: { n: { count: true, limit: 5 } } }\n", "cannot be sharded"},
		{"sharded min", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", shards: 2, values: { n: { count: true, min: 1 } } }\n", "cannot be sharded"},
		{"colliding sort keys", "    counters:\n      C: { pk: \"T#{tenantId}\", sk: \"THING#stats\", values: { n: count } }\n", "not distinguishable by prefix"},
		{"unique on list", "      aliases: string_list\n    unique:\n      U: { fields: [aliases] }\n", "make it a string_set"},
		{"unique on a map", "      attrs: string_map\n    unique:\n      U: { fields: [attrs] }\n", "cannot be unique"},
		{"unique on two sets", "      codes: string_set\n    unique:\n      U: { fields: [tags, codes] }\n", "are both sets"},
		{"duplicate method", "    access:\n      Get: get\n    writes:\n      Get: create\n", "declared more than once"},
		{"missing unique for get", "    access:\n      G: { get: { unique: Nope } }\n", "not a unique constraint"},
		{"copy key misses entity key", "    indexes:\n      ByName: { strategy: copy, pk: \"N#{tenantId}\", sk: \"NAME#{name}\", project: keys }\n", "must include every primary key field"},
		{"requires unknown entity", "    writes:\n      W: { create: true, requires: { Nope: { key: { a: b } } } }\n", "Nope is not an entity"},
		{"requires missing key", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId } } } }\n", "must give Thing's key field thingId"},
		{"copy_of self", "      copy: { type: string, copy_of: Thing.name }\n", "copies within one entity"},
		{"project on index", "    indexes:\n      ByName: { pk: \"N#{name}\", project: keys }\n    access:\n      L: { query: ByName, project: [name] }\n", "project applies to queries on the entity's own partition"},
		{"patch key field", "    writes:\n      W: { patch: [thingId] }\n", "part of the primary key"},
		{"field named key", "      key: string\n", "clash with the generated Key() method"},
		{"flow comma", "      note: { type: string, doc: one, two }\n", "quote the value"},
		{"update on a create", "    writes:\n      W: { create: true, update: [name] }\n", "declare exactly one of create, update or delete"},
		{"set on a delete", "    writes:\n      W: { delete: true, set: { name: x } }\n", "set applies to creates and updates"},
		{"requires set and consume", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, set: { name: x }, consume: true } } }\n", "set and consume are exclusive"},
		{"requires that does nothing", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, optional: true } } }\n", "checks nothing"},
		{"requires reference of another type", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, when: { count: \"{name}\" } } } }\n", "Thing.name is a string, but Thing.count is a int"},
		{"requires reference to unknown field", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, when: { name: \"{nope}\" } } } }\n", "nope is not a field of Thing"},
		{"requires sets a key field", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, set: { thingId: x } } } }\n", "part of Thing's primary key"},
		{"requires changes a counter", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", values: { n: count } }\n    writes:\n      W: { delete: true, requires: { C: { key: { tenantId: tenantId }, set: { n: 0 } } } }\n", "can only be checked with when"},
		{"requires sharded counter", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", shards: 2, values: { n: count } }\n    writes:\n      W: { delete: true, requires: { C: { key: { tenantId: tenantId }, when: { n: 0 } } } }\n", "is sharded"},
		{"copy key lowers the entity key", "    indexes:\n      ByName: { strategy: copy, pk: \"N#{tenantId}\", sk: \"NAME#{name}#{thingId|lower}\", project: keys }\n", "untransformed"},
		{"field named like a GSI key", "      byNamePK: { type: string, attr: ByNamePK }\n    indexes:\n      ByName: { pk: \"N#{name}\", project: keys }\n", "key attribute of GSI ByName"},
		{"create false", "    writes:\n      W: { create: false, set: { name: x } }\n", "create: false is not meaningful"},
		{"requires its own item", "    writes:\n      W: { update: [name], requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, when: { count: 1 } } } }\n", "that is the item being written"},
		{"requires a counter it updates", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", values: { n: count } }\n    writes:\n      W: { delete: true, requires: { C: { key: { tenantId: tenantId }, when: { n: 1 } } } }\n", "also updates counter C"},
		{"requires unknown counter value", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", values: { n: count } }\n    writes:\n      W: { delete: true, requires: { C: { key: { tenantId: tenantId }, when: { m: 0 } } } }\n", "m is not a value of counter C"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := base
			if strings.HasPrefix(c.extra, "      ") {
				// Extra field lines go under fields:.
				src = strings.Replace(src, "      tags: string_set\n", "      tags: string_set\n"+c.extra, 1)
			} else {
				src += c.extra
			}
			_, err := Parse([]byte(src))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatalf("base schema should be valid: %v", err)
	}
}

func TestGoName(t *testing.T) {
	for in, want := range map[string]string{
		"tenantId": "TenantID", "descriptionHtml": "DescriptionHTML", "in_person": "InPerson",
		"url": "URL", "HTMLBody": "HTMLBody", "userIds": "UserIds", "a": "A",
	} {
		if got := GoName(in); got != want {
			t.Errorf("GoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequiredFields(t *testing.T) {
	src := strings.Replace(base, "      name: string\n", "      name: { type: string, required: true }\n", 1)
	if _, err := Parse([]byte(src + "    writes:\n      Add: create\n      Rename: { update: [name] }\n")); err != nil {
		t.Fatal(err)
	}
	_, err := Parse([]byte(src + "    writes:\n      Clear: { set: { name: \"\" } }\n"))
	if err == nil || !strings.Contains(err.Error(), "name is required, so it cannot be set to its zero value") {
		t.Fatalf("got %v", err)
	}
}

// A requirement may not change another entity's counter whose limit that entity's callers supply:
// there is nowhere to pass the limit.
func TestRequiresCannotGrowCallerLimits(t *testing.T) {
	src := base + `    counters:
      Active: { pk: "C#{tenantId}", sk: "C", values: { n: { count: true, where: { status: a }, limit: arg } } }
    writes:
      Make: create
  Other:
    fields:
      tenantId: string
      thingId: string
    key: { pk: "O#{tenantId}", sk: "O#{thingId}" }
    writes:
      Poke:
        create: true
        requires:
          Thing: { key: { tenantId: tenantId, thingId: thingId }, when: { status: b }, set: { status: a } }
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "can grow Active.n, whose limit callers supply") {
		t.Fatalf("got %v", err)
	}
}

// A read-free write must know the key of every counter it changes: here the counter is keyed by
// customerId, which Close neither pins nor is given, so Close must read the order first.
func TestTransitionNeedsTheCounterKey(t *testing.T) {
	m, err := Parse([]byte(`
dynago: 1
package: orders
table: { name: orders }
entities:
  Order:
    fields:
      shopId: string
      orderId: string
      customerId: string
      status: { type: enum, values: [open, closed] }
    key: { pk: "SHOP#{shopId}", sk: "ORDER#{orderId}" }
    counters:
      ShopOrders: { pk: "SHOP#{shopId}", sk: "COUNTS", values: { open: { count: true, where: { status: open } } } }
      CustomerOrders: { pk: "CUST#{customerId}", sk: "ORDERS", values: { open: { count: true, where: { status: open } } } }
    writes:
      Close: { set: { status: closed }, when: { status: open } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if w := m.Entities[0].Writes[0]; !w.ReadFirst || w.Transition {
		t.Fatalf("Close: readFirst=%v transition=%v; it can't know the customer's counter key", w.ReadFirst, w.Transition)
	}
}

// DynamoDB allows 100 projected attributes per table, summed across all indexes.
func TestProjectedAttributesAreCountedAcrossGSIs(t *testing.T) {
	var fields, names strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&fields, "      f%d: string\n", i)
		fmt.Fprintf(&names, "f%d, ", i)
	}
	list := strings.TrimSuffix(names.String(), ", ")
	src := strings.Replace(base, "      tags: string_set\n", "      tags: string_set\n"+fields.String(), 1) + `    indexes:
      A: { pk: "A#{name}", project: [` + list + `] }
      B: { pk: "B#{name}", project: [` + list + `] }
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "attributes in total") {
		t.Fatalf("got %v", err)
	}
}

// A copy index exists for read-your-writes, so its queries read consistently unless told not to.
func TestCopyQueriesDefaultToConsistent(t *testing.T) {
	src := base + `    indexes:
      ByName: { strategy: copy, pk: "N#{tenantId}", sk: "NAME#{name}#{thingId}", project: keys }
    access:
      Mine: { query: ByName }
      Cheap: { query: ByName, consistent: false }
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	access := m.Entities[0].Access
	if !access[0].Consistent || access[1].Consistent {
		t.Fatalf("Mine consistent=%t, Cheap consistent=%t", access[0].Consistent, access[1].Consistent)
	}
}
