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
		{"requires set and consume", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: thingId }, set: { name: x }, consume: true } } }\n", "exclusive with consume"},
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
		{"freshness on a GSI", "    indexes:\n      ByName: { strategy: gsi, pk: \"N#{tenantId}\", sk: \"N#{thingId}\", project: keys }\n    access:\n      L: { query: ByName, freshness: immediate }\n", "freshness: immediate needs read-your-writes, but ByName is a GSI"},
		{"freshness contradicts consistent", "    access:\n      G: { get: key, freshness: eventual, consistent: true }\n", "contradicts freshness: eventual"},
		{"immediate but not consistent", "    access:\n      G: { get: key, freshness: immediate, consistent: false }\n", "needs a consistent read"},
		{"unknown freshness", "    access:\n      G: { get: key, freshness: soon }\n", "freshness must be immediate or eventual"},
		{"inferred copy with bad keys", "    indexes:\n      ByName: { pk: \"N#{tenantId}\", sk: \"{name}\", project: keys }\n    access:\n      L: { query: ByName, freshness: immediate }\n", "(a copy, because L needs immediate freshness)"},
		{"scan without reason", "    access:\n      All: { scan: true }\n", "give the reason it is needed"},
		{"reason on a query", "    access:\n      L: { query: key, reason: because }\n", "reason applies to scans"},
		{"scan with order", "    access:\n      All: { scan: true, reason: export, order: desc }\n", "a scan takes page and max_page"},
		{"copy and snapshot", "      other: { type: string, copy_of: Thing.name, snapshot_of: Thing.name }\n", "copy_of and snapshot_of are exclusive"},
		{"accept without reason", "    accept: { unused-index: \"\" }\n", "give the reason the finding is acceptable"},
		{"volume without typical", "    volume: { max: 5 }\n", "give typical"},
		{"volume max under typical", "    volume: { per: Thing, typical: 5, max: 2 }\n", "need 0 <= typical <= max"},
		{"volume per unknown", "    volume: { per: Nope, typical: 5 }\n", "per: Nope is not an entity"},
		{"volume with nothing to count per", "    volume: { typical: 5 }\n", "doesn't nest in another entity's"},
		{"ref to unknown entity", "      owner: { type: string, ref: Nope }\n", "ref: Nope is not an entity"},
		{"ref that can't key", "      owner: { type: int, ref: Thing }\n", "Thing's key needs"},
		{"in with one value", "    writes:\n      W: { update: [name], when: { status: { in: [a] } } }\n", "one value is an equality"},
		{"in with every value", "    writes:\n      W: { update: [name], when: { status: { in: [a, b] } } }\n", "the condition always holds"},
		{"in with a value twice", "    writes:\n      W: { update: [count], when: { name: { in: [x, y, x] } } }\n", "x is listed twice"},
		{"in without a list", "    writes:\n      W: { update: [count], when: { name: { in: x } } }\n", "want a list of values"},
		{"not of a value the enum lacks", "    indexes:\n      ByName: { pk: \"N#{name}\", project: keys, where: { status: { not: c } } }\n", `"c" is not one of`},
		{"unknown condition form", "    writes:\n      W: { update: [name], when: { status: { neq: a } } }\n", `unknown form "neq"`},
		{"two condition forms", "    writes:\n      W: { update: [name], when: { name: { not: x, in: [y, z] } } }\n", "want a value, { not: <value> } or { in: [<value>, ...] }"},
		{"matches without where", "    indexes:\n      ByName: { pk: \"N#{name}\", project: keys, matches: 0.5 }\n", "this index has no where"},
		{"matches over one", "    indexes:\n      ByName: { pk: \"N#{name}\", project: keys, where: { status: a }, matches: 5 }\n", "more than 0, at most 1"},
		{"requires adds to a string", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, add: { name: 1 } } } }\n", "add applies to int fields"},
		{"requires adds nothing", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, add: { count: 0 } } } }\n", "a whole number other than 0"},
		{"requires adds a field of another type", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, add: { count: \"{name}\" } } } }\n", "Thing.name is a string, but Thing.count is a int"},
		{"requires patches a constant", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, patch: { name: x } } } }\n", "A constant goes in set"},
		{"requires changes a field twice", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, set: { count: 1 }, add: { count: 1 } } } }\n", "count is given more than once"},
		{"requires adds to a key field", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, patch: { thingId: \"{name}\" } } } }\n", "part of Thing's primary key"},
		{"requires adds and consumes", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, add: { count: 1 }, consume: true } } }\n", "exclusive with consume"},
		{"batch on a query", "    access:\n      L: { query: key, batch: 5 }\n", "batch applies to get: key"},
		{"batch without a number", "    access:\n      G: { get: key, batch: true }\n", "the typical number of keys a call reads"},
		{"all on a get", "    access:\n      G: { get: key, all: true }\n", "all applies to a counter read"},
		{"all of a counter with one item", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", values: { n: count } }\n    access:\n      A: { counter: C, all: true }\n", "has one item per partition key"},
		{"all of a sharded counter", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C#{name}\", shards: 2, values: { n: count } }\n    access:\n      A: { counter: C, all: true }\n", "is sharded"},
		{"all of a counter keyed by a time", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C#{at}\", values: { n: count } }\n    access:\n      A: { counter: C, all: true }\n", "needs string or enum fields"},
		{"all of a counter keyed by a lowered name", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C#{name|lower}\", values: { n: count } }\n    access:\n      A: { counter: C, all: true }\n", "can't be read back"},
		{"all with a range", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C#{name}\", values: { n: count } }\n    access:\n      A: { counter: C, all: true, order: desc }\n", "takes page and max_page"},
		{"partition read without of", "    access:\n      P: { query: partition }\n", "list the entities it returns"},
		{"of without a partition read", "    access:\n      P: { query: key, of: [Thing] }\n", "it goes with query: partition"},
		{"partition read of an unknown entity", "    access:\n      P: { query: partition, of: [Thing, Nope] }\n", "of: Nope is not an entity"},
		{"partition read of an entity twice", "    access:\n      P: { query: partition, of: [Thing, Thing] }\n", "Thing is listed twice"},
		{"partition read with a range", "    access:\n      P: { query: partition, of: [Thing], range: name }\n", "a partition read returns several kinds whole"},
		{"partition read of another partition", "    access:\n      P: { query: partition, of: [Thing, Other] }\n  Other:\n    fields: { tenantId: string, otherId: string }\n    key: { pk: \"O#{tenantId}\", sk: \"OTHER#{otherId}\" }\n", "so one Query can't read both"},
		{"ensure and optional", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, ensure: {}, optional: true } } }\n", "choose one"},
		{"ensure and consume", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, ensure: {}, consume: true } } }\n", "choose one"},
		{"ensure gives a key field", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, ensure: { thingId: x } } } }\n", "part of Thing's primary key"},
		{"ensure and set give one field", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, set: { count: 1 }, ensure: { count: 2 } } } }\n", "count is given more than once"},
		{"ensure of the wrong type", "    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, ensure: { count: many } } } }\n", "does not match field type"},
		{"ensure without a required field", "      must: { type: string, required: true }\n    writes:\n      W: { create: true, requires: { Thing: { key: { tenantId: tenantId, thingId: name }, ensure: { count: 2 } } } }\n", "Thing.must is required, so a Thing this write creates needs it"},
		{"ensure of a counter", "    counters:\n      C: { pk: \"C#{tenantId}\", sk: \"C\", values: { n: count } }\n    writes:\n      W: { delete: true, requires: { C: { key: { tenantId: tenantId }, ensure: {}, when: { n: 0 } } } }\n", "can only be checked with when"},
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

// `not` and `in` are conditions like any other, but they leave several values possible: a write
// whose when uses one can't know the state it starts from, so it reads the item if anything
// derived depends on the field.
func TestNotAndInConditions(t *testing.T) {
	m, err := Parse([]byte(strings.Replace(base, "values: [a, b]", "values: [a, b, c]", 1) + `    indexes:
      Open: { pk: "OPEN#{tenantId}", sk: "T#{thingId}", project: keys, where: { status: { not: c }, name: { in: [x, y] } } }
    counters:
      Counts:
        pk: "T#{tenantId}"
        sk: "COUNTS"
        values:
          live: { count: true, where: { status: { in: [a, b] } } }
    writes:
      Close: { set: { status: c }, when: { status: { not: c } } }
      Reopen: { set: { status: a }, when: { status: c } }
      Rename: { update: [at], when: { status: { in: [a, b] } } }
      Other:
        create: true
        requires:
          Thing:
            key: { tenantId: tenantId, thingId: name }
            when: { status: { not: c }, name: { not: "{name}" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	e := m.Entity("Thing")
	where := e.Indexes[0].Where
	if got := PredText(where, ""); got != `status != "c" and name in ["x", "y"]` {
		t.Errorf("where reads %q", got)
	}
	if !where[0].Matches("a") || where[0].Matches("c") || !where[1].Matches("y") || where[1].Matches("z") {
		t.Errorf("Matches is wrong for %s", PredText(where, ""))
	}
	if vs, ok := where[0].Allowed(); !ok || fmt.Sprint(vs) != "[a b]" {
		t.Errorf("status != c allows %v, %v", vs, ok)
	}
	if _, ok := where[1].Allowed(); ok {
		t.Errorf("a string field's values can't be listed")
	}
	writes := map[string]*Write{}
	for _, w := range e.Writes {
		writes[w.Name] = w
	}
	// Closing moves the index entry and the count, from a state the when doesn't pin: it reads.
	if w := writes["Close"]; !w.ReadFirst || w.Transition {
		t.Errorf("Close: ReadFirst %v, Transition %v; want a read", w.ReadFirst, w.Transition)
	}
	// Nothing derived depends on at: one conditional UpdateItem, whatever the when.
	if w := writes["Rename"]; w.ReadFirst {
		t.Errorf("Rename reads first")
	}
	rq := writes["Other"].Requires[0]
	if got := rq.Condition("Thing"); got != `the Thing to exist with status != "c" and name != Thing.name` {
		t.Errorf("the requirement reads %q", got)
	}
}

func TestGoName(t *testing.T) {
	for in, want := range map[string]string{
		"tenantId": "TenantID", "descriptionHtml": "DescriptionHTML", "in_person": "InPerson",
		"url": "URL", "HTMLBody": "HTMLBody", "a": "A",
		// The plural of an initialism keeps its capitals, as its singular does.
		"userIds": "UserIDs", "ids": "IDs", "imageUrls": "ImageURLs", "URLsSeen": "URLsSeen",
		"sms": "SMS", "https": "HTTPS", "kids": "Kids", "is": "Is",
	} {
		if got := GoName(in); got != want {
			t.Errorf("GoName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"Loan": "loan", "APIKey": "apiKey", "THING": "thing", "IDs": "ids", "URLsSeen": "urlsSeen", "IDsmith": "iDsmith"} {
		if got := unexported(in); got != want {
			t.Errorf("unexported(%q) = %q, want %q", in, got, want)
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

// A read needing immediate freshness makes an index without a strategy a copy; otherwise it is a
// GSI. Both are recorded as chosen, with the reason.
func TestFreshnessChoosesTheStrategy(t *testing.T) {
	src := base + `    indexes:
      Mine: { pk: "N#{tenantId}", sk: "MINE#{name}#{thingId}", project: keys }
      Theirs: { pk: "N#{tenantId}", sk: "{name}#{thingId}", project: keys }
    access:
      L: { query: Mine, freshness: immediate }
      M: { query: Theirs, freshness: eventual }
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	mine, theirs := m.Entities[0].Indexes[0], m.Entities[0].Indexes[1]
	if mine.Strategy != StrategyCopy || !mine.StrategyInferred || mine.StrategyReason != "L needs immediate freshness" {
		t.Errorf("Mine = %s (%s)", mine.Strategy, mine.StrategyReason)
	}
	if theirs.Strategy != StrategyGSI || !theirs.StrategyInferred {
		t.Errorf("Theirs = %s", theirs.Strategy)
	}
	if a := m.Entities[0].Access; !a[0].Consistent || a[1].Consistent {
		t.Error("immediate reads must be consistent, eventual ones not")
	}
}

// A ref field supplies the key field no same-named field can: with a generic id, the target's id;
// next to a same-named reference, the target's last key field.
func TestRefBesideSameNamedFields(t *testing.T) {
	m, err := Parse([]byte(`
dynago: 1
package: blog
table: { name: blog }
entities:
  User:
    fields: { id: string }
    key: { pk: "USER#{id}", sk: "USER" }
    volume: 100
  Post:
    fields: { id: string, userId: { type: string, ref: User } }
    key: { pk: "POST#{id}", sk: "POST" }
    volume: { per: User, typical: 5, max: 50 }
  Member:
    fields: { libraryId: string, memberId: string }
    key: { pk: "LIB#{libraryId}", sk: "MEMBER#{memberId}" }
  Loan:
    fields: { libraryId: string, loanId: string, memberId: string, stewardId: { type: string, ref: Member } }
    key: { pk: "LIB#{libraryId}", sk: "LOAN#{loanId}" }
`))
	if err != nil {
		t.Fatal(err)
	}
	post := m.Entity("Post")
	if p := post.Parent; p == nil || p.To.Name != "User" || p.Key[0].Source.Name != "userId" || p.OneToOne() {
		t.Fatalf("Post's parent = %+v", post.Parent)
	}
	var steward *Relation
	for _, r := range m.Relations {
		if r.From.Name == "Loan" && r.To.Name == "Member" && r.Is(RelRef) {
			steward = r
		}
	}
	if steward == nil || steward.Key[0].Source.Name != "libraryId" || steward.Key[1].Source.Name != "stewardId" {
		t.Fatalf("Loan → Member by stewardId = %+v", steward)
	}
}

// A loan holds two members' keys (its borrower's by name, its steward's through a ref), so a
// volume by Member must say which it counts by.
func TestVolumeViaPicksALink(t *testing.T) {
	src := `
dynago: 1
package: lib
table: { name: lib }
entities:
  Member:
    fields: { libraryId: string, memberId: string }
    key: { pk: "LIB#{libraryId}", sk: "MEMBER#{memberId}" }
    volume: 100
  Loan:
    fields: { libraryId: string, loanId: string, memberId: string, stewardId: { type: string, ref: Member } }
    key: { pk: "LIB#{libraryId}", sk: "LOAN#{loanId}" }
    volume: { per: Member, typical: 3%s, by: { Member: { typical: 10%s } } }
`
	for _, c := range []struct{ per, by, want, err string }{
		{"", "", "", "more than one way (via: memberId, or via: stewardId); say which with via"},
		{", via: memberId", ", via: stewardId", "stewardId", ""},
		{", via: stewardId", ", via: memberId", "memberId", ""},
		{", via: libraryId", "", "", "via: libraryId doesn't pick one way"},
		{", via: nope", "", "", "Loan has no field nope"},
	} {
		m, err := Parse([]byte(strings.Replace(strings.Replace(src, "%s", c.per, 1), "%s", c.by, 1)))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("per%q by%q: %v, want %q", c.per, c.by, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		v := m.Entity("Loan").Volume
		if got := v.By[0].Relation.Key[1].Source.Name; got != c.want {
			t.Errorf("per%q by%q: by Member through %s, want %s", c.per, c.by, got, c.want)
		}
		if got := v.Per.Key[1].Source.Name; got == c.want {
			t.Errorf("per%q by%q: per Member through %s too", c.per, c.by, got)
		}
	}
}

// Parents come from partition nesting, counts multiply down from totals, and ref links a field
// to another entity's key.
func TestRelationsAndVolumes(t *testing.T) {
	m, err := Parse([]byte(`
dynago: 1
package: shop
table: { name: shop }
entities:
  Shop:
    fields: { shopId: string }
    key: { pk: "SHOP#{shopId}", sk: "SHOP" }
    volume: 50
  Customer:
    fields: { shopId: string, customerId: string }
    key: { pk: "SHOP#{shopId}", sk: "CUST#{customerId}" }
    volume: { typical: 200, max: 10000 }
  Order:
    fields: { shopId: string, orderId: string, buyer: { type: string, ref: Customer } }
    key: { pk: "SHOP#{shopId}#ORDER#{orderId}", sk: "ORDER" }
    volume: { typical: 1000, by: { Customer: { typical: 5, max: 300 } } }
  Note:
    fields: { shopId: string, orderId: string }
    key: { pk: "SHOP#{shopId}#ORDER#{orderId}", sk: "NOTE" }
    volume: { typical: 0.1 }
`))
	if err != nil {
		t.Fatal(err)
	}
	parent := func(name string) string {
		if p := m.Entity(name).Parent; p != nil {
			return p.To.Name
		}
		return ""
	}
	for e, want := range map[string]string{"Shop": "", "Customer": "Shop", "Order": "Shop", "Note": "Order"} {
		if got := parent(e); got != want {
			t.Errorf("%s's parent = %q, want %q", e, got, want)
		}
	}
	if c := m.Entity("Order").Count; c != 50000 {
		t.Errorf("orders = %v", c)
	}
	if !m.Entity("Note").Parent.OneToOne() || m.Entity("Note").Volume.Max != 1 {
		t.Error("a note keyed like its order is at most one per order")
	}
	var ref *Relation
	for _, r := range m.Relations {
		if r.From.Name == "Order" && r.To.Name == "Customer" {
			ref = r
		}
	}
	if ref == nil || !ref.Is(RelRef) || !ref.Is(RelVolume) || ref.Key[1].Source.Name != "buyer" {
		t.Errorf("Order → Customer = %+v", ref)
	}
	// Workload peaks below 1 aren't peaks.
	if _, err := Parse([]byte(strings.Replace(base, "table: { name: things }", "table: { name: things }\nworkload: { peak: 0.5 }", 1))); err == nil || !strings.Contains(err.Error(), "must be 1 or more") {
		t.Errorf("peak 0.5: %v", err)
	}
}

func TestNumber(t *testing.T) {
	for f, want := range map[float64]string{
		0: "0", 2000000: "2,000,000", 1234.5: "1,234.5", 0.02: "0.02", 0.004: "0.004", 0.001: "0.001", -1500: "-1,500", 12.345: "12.35", 0.999: "1", 999.999: "1,000",
	} {
		if got := Number(f); got != want {
			t.Errorf("Number(%v) = %q, want %q", f, got, want)
		}
	}
}
