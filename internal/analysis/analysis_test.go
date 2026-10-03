package analysis

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/schema"
)

func example(t *testing.T) *Result {
	t.Helper()
	m, err := schema.Load("../../examples/toollibrary/toollibrary.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return Analyze(m, cost.DefaultPrices, examplePolicy(t))
}

// examplePolicy reads the policy beside the example schema.
func examplePolicy(t *testing.T) *Policy {
	t.Helper()
	data, err := os.ReadFile("../../examples/toollibrary/dynago.policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func parse(t *testing.T, src string) *schema.Model {
	t.Helper()
	m, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// has reports whether a finding of the rule, severity and subject exists, with text containing want.
func has(r *Result, rule string, sev Severity, subject, want string) bool {
	for _, f := range r.Findings {
		if f.Rule == rule && f.Severity == sev && f.Subject.String() == subject && strings.Contains(f.Message, want) {
			return true
		}
	}
	return false
}

func dump(r *Result) string {
	var b strings.Builder
	for _, f := range r.Findings {
		acc := ""
		if f.Accepted != nil {
			acc = " (accepted)"
		}
		b.WriteString(string(f.Severity) + " " + f.Rule + " " + f.Subject.String() + ": " + f.Message + acc + "\n")
	}
	return b.String()
}

func TestExampleFindings(t *testing.T) {
	r := example(t)
	if len(r.Failing()) != 0 {
		t.Fatalf("the example fails its policy:\n%s", dump(r))
	}
	if !has(r, "large-field", Warning, "Tool", "manual (p99 19.5 KB)") {
		t.Errorf("no large-field finding:\n%s", dump(r))
	}
	if !has(r, "scan", Note, "Loan.Export", "The yearly lending report") {
		t.Errorf("no note for the declared scan:\n%s", dump(r))
	}
	accepted := 0
	for _, f := range r.Findings {
		if f.Accepted != nil {
			accepted++
		}
	}
	if accepted != 4 {
		t.Errorf("want 4 accepted findings (two sparse indexes, the manual, one lookup by code):\n%s", dump(r))
	}
}

func TestExamplePartitions(t *testing.T) {
	r := example(t)
	find := func(space, pk string) *Partition {
		for _, p := range r.Partitions {
			if p.Space() == space && p.PK == pk {
				return p
			}
		}
		t.Fatalf("no partition %s %s", space, pk)
		return nil
	}
	// A library's partition holds the library, its members and its counters.
	lib := find("base table", "LIB#{libraryId}")
	if len(lib.Members) != 4 || lib.Count.Typical != 2000 {
		t.Errorf("library partition = %+v", lib)
	}
	// A tool's partition holds the tool, its loans (which accumulate) and at most one hold.
	tool := find("base table", "LIB#{libraryId}#TOOL#{toolId}")
	var hold, loan *Member
	for _, mb := range tool.Members {
		switch mb.Label {
		case "Hold":
			hold = mb
		case "Loan":
			loan = mb
		}
	}
	if hold == nil || hold.Count.Typical != 0.02 || hold.Count.Max != 1 {
		t.Errorf("hold count = %+v", hold)
	}
	if loan == nil || !loan.Grows || loan.Count.Typical != 20 || loan.Count.Max != 500 || !tool.Grows {
		t.Errorf("loan member = %+v", loan)
	}
	// Overdue gathers a library's active loans in one GSI partition: of 60 tools × 20 loans
	// typically, and the biggest skew (5,000 tools) at most, the 2% its where matches.
	due := find("GSI Overdue", "LIB#{libraryId}#DUE")
	if c := due.Members[0].Count; c.Typical != 24 || c.Max != 2000 || due.Bound() {
		t.Errorf("overdue count = %+v", c)
	}
	// An enum in the key divides the typical count, not the largest.
	cat := find("GSI ByCategory", "LIB#{libraryId}#CAT#{category}")
	if c := cat.Members[0].Count; c.Typical != 12 || c.Max != 5000 {
		t.Errorf("catalogue count = %+v", c)
	}
	// Nothing says how many holds share a code hash: the count is unknown, not guessed.
	code := find("GSI ByCode", "HOLDCODE#{codeHash}")
	if len(code.Undeclared) != 1 || code.Undeclared[0] != "codeHash" || code.Members[0].Count.Known {
		t.Errorf("code partition = %+v", code)
	}
	// A member's partition holds their loan copies, counted by the declared spread over members.
	mine := find("base table", "LIB#{libraryId}#MEMBER#{memberId}")
	// Only the active ones are copied there: 2% of the 60 typical and 400 at most.
	if c := mine.Members[0].Count; math.Abs(c.Typical-1.2) > 1e-9 || c.Max != 8 {
		t.Errorf("my loans count = %+v", c)
	}
	for _, p := range r.Partitions {
		if p.Rated && p.Risk != RiskLow {
			t.Errorf("%s %s: risk %s at the example's rates", p.Space(), p.PK, p.Risk)
		}
	}
	if st := r.Reads[findAccess(r.Model, "Loan", "History")]; st.Pages.Typical != 1 || st.Pages.Max != 25 {
		t.Errorf("history pages = %+v", st.Pages)
	}
}

func findAccess(m *schema.Model, entity, name string) *schema.Access {
	for _, a := range m.Entity(entity).Access {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func TestExampleLifecyclesAndGuarantees(t *testing.T) {
	r := example(t)
	var status *Lifecycle
	for _, lc := range r.Lifecycles {
		if lc.Entity.Name == "Tool" && lc.Field.Name == "status" {
			status = lc
		}
	}
	if status == nil {
		t.Fatal("no lifecycle for Tool.status")
	}
	want := map[string]bool{"→available by Add": true, "available→retired by Retire": true, "available→onLoan by Loan.Borrow": true, "onLoan→available by Loan.Return": true}
	for _, tr := range status.Transitions {
		delete(want, tr.From+"→"+tr.To+" by "+tr.By)
	}
	if len(want) > 0 || len(status.Final) != 1 || status.Final[0] != "retired" {
		t.Errorf("Tool.status: missing %v; final %v", want, status.Final)
	}
	var texts []string
	for _, g := range r.Guarantees {
		texts = append(texts, g.Text)
	}
	all := strings.Join(texts, "\n")
	for _, w := range []string{
		"No two Members have the same libraryId and email (ignoring case).",
		`never goes below 1`,
		"Retire deletes the Hold if there is one, in one transaction.",
	} {
		if !strings.Contains(all, w) {
			t.Errorf("guarantees lack %q:\n%s", w, all)
		}
	}
}

func TestExampleAlternatives(t *testing.T) {
	r := example(t)
	ix := r.Model.Entity("Loan").Indexes[0]
	alt := r.Alternatives[ix]
	if ix.Name != "ByMember" || alt.Strategy != schema.StrategyGSI || !alt.Feasible || len(alt.Breaks) != 1 || len(alt.Writes) == 0 {
		t.Fatalf("ByMember alternative = %+v", alt)
	}
	// Switching the index back is complete: the model is unchanged.
	if ix.Strategy != schema.StrategyCopy || ix.PKAttr != schema.AttrPK {
		t.Error("the alternative left the index changed")
	}
	if !strings.Contains(alt.WriteSummary(), "Loan.Extend 8 → 5 WRU, no longer a transaction") {
		t.Errorf("summary = %s", alt.WriteSummary())
	}
}

const base = `
dynago: 1
package: things
table: { name: things }
entities:
  Org:
    fields:
      orgId: string
    key: { pk: "ORG#{orgId}", sk: "ORG" }
    writes:
      Create: create
    volume: 100
  Thing:
    fields:
      orgId: string
      thingId: string
      name: string
      kind: { type: enum, values: [a, b] }
      body: { type: string, size: 100 }
    key: { pk: "ORG#{orgId}", sk: "THING#{thingId}" }
    writes:
      Add: create
    volume: { typical: 10, max: 1000 }
`

func TestRules(t *testing.T) {
	cases := []struct {
		name, extra   string
		rule          string
		sev           Severity
		subject, want string
	}{
		{"low-cardinality GSI", "    indexes:\n      ByKind: { pk: \"KIND#{kind}\", sk: \"T#{thingId}\", project: keys }\n    access:\n      K: { query: ByKind }\n",
			"low-cardinality-key", Warning, "Thing.ByKind", "at most 2 partitions"},
		{"constant partition key", "    indexes:\n      All: { pk: \"ALL\", sk: \"T#{thingId}\", project: keys }\n    access:\n      L: { query: All }\n",
			"low-cardinality-key", Warning, "Thing.All", "one partition, however many there are. Add an id (a tenant, a parent) to the key. Declare the volume and rates"},
		{"small, quiet constant partition key", "    indexes:\n      All: { pk: \"ALL\", sk: \"T#{thingId}\", project: keys }\n    access:\n      L: { query: All, rate: 1 }\n",
			"low-cardinality-key", Note, "Thing.All", "At the declared volumes and rates that's fine: the largest holds"},
		{"sparse index", "    indexes:\n      ByName: { pk: \"ORG#{orgId}#N\", sk: \"{name}#{thingId}\", project: keys }\n    access:\n      L: { query: ByName }\n",
			"sparse-index", Warning, "Thing.ByName", "a Thing without one is missing from ByName and from L"},
		{"unused index", "    indexes:\n      ByName: { pk: \"ORG#{orgId}#N\", sk: \"N#{thingId}\", project: keys }\n",
			"unused-index", Warning, "Thing.ByName", "no declared read uses ByName"},
		{"lookup without uniqueness", "    indexes:\n      ByName: { pk: \"NAME#{name}\", project: keys }\n    access:\n      Find: { query: ByName, page: 1, max_page: 1 }\n",
			"unenforced-unique", Warning, "Thing.ByName", "Find reads one entry of ByName"},
		{"copy nobody needs", "    indexes:\n      Mine: { strategy: copy, pk: \"ORG#{orgId}#M\", sk: \"M#{thingId}\", project: keys }\n    access:\n      L: { query: Mine, freshness: eventual }\n",
			"copy-not-needed", Note, "Thing.Mine", "A GSI would do"},
		{"scan", "    access:\n      Export: { scan: true, reason: the nightly export }\n",
			"scan", Note, "Thing.Export", "the nightly export"},
		{"hot counter", "    counters:\n      Total: { pk: \"TOTAL\", sk: \"TOTAL\", values: { n: count } }\n    writes:\n      Put: { create: true, hot_key_rate: 800 }\n",
			"hot-counter", Error, "Thing.Total", "at 800 writes/s to one Total key"},
		{"item too large", "      blob: { type: string, size: 1000/500000 }\n",
			"item-too-large", Error, "Thing", "over DynamoDB's 400 KB limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := base
			if strings.HasPrefix(c.extra, "      ") {
				src = strings.Replace(src, "      body: { type: string, size: 100 }\n", "      body: { type: string, size: 100 }\n"+c.extra, 1)
			} else {
				src = strings.Replace(src, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", c.extra+"    volume: { typical: 10, max: 1000 }\n", 1)
				if !strings.Contains(c.extra, "writes:") {
					src = strings.Replace(src, "    volume: { typical: 10, max: 1000 }\n", "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", 1)
				}
			}
			r := Analyze(parse(t, src), cost.DefaultPrices, nil)
			if !has(r, c.rule, c.sev, c.subject, c.want) {
				t.Errorf("want %s %s on %s containing %q:\n%s", c.sev, c.rule, c.subject, c.want, dump(r))
			}
		})
	}
}

// copy_of says how far a change to the source fans out, and whether one transaction can carry it.
func TestCopyDriftFanOut(t *testing.T) {
	src := base + `  Note:
    fields:
      orgId: string
      thingId: string
      noteId: string
      thingName: { type: string, copy_of: Thing.name }
    key: { pk: "ORG#{orgId}#T#{thingId}", sk: "NOTE#{noteId}" }
    writes:
      Add: create
    volume: { per: Thing, typical: 5, max: 5000 }
`
	r := Analyze(parse(t, src), cost.DefaultPrices, nil)
	if !has(r, "copy-drift", Warning, "Note.thingName", "Each Thing is copied into up to 5,000 Notes: more than one transaction holds") {
		t.Errorf("copy drift:\n%s", dump(r))
	}
	// A snapshot is a decision, not a risk.
	r = Analyze(parse(t, strings.Replace(src, "copy_of: Thing.name", "snapshot_of: Thing.name", 1)), cost.DefaultPrices, nil)
	if len(r.Findings) != 0 {
		t.Errorf("a snapshot raised findings:\n%s", dump(r))
	}
}

// A spread declared with `by` over a grandparent is what its partitions hold. Worked out up the
// chain, an org's notes would peak at the busiest thing's 50 times the typical 10 things; the
// schema says no org has more than 40.
func TestDeclaredSpreadBeatsTheChain(t *testing.T) {
	note := func(volume string) string {
		return base + `  Note:
    fields:
      orgId: string
      thingId: string
      noteId: string
      at: time
    key: { pk: "ORG#{orgId}#T#{thingId}", sk: "NOTE#{noteId}" }
    indexes:
      Recent: { pk: "ORG#{orgId}#NOTES", sk: "{at}#{thingId}#{noteId}", project: keys }
    access:
      Latest: { query: Recent }
    writes:
      Add: create
    volume: ` + volume + "\n"
	}
	recent := func(src string) Estimate {
		t.Helper()
		r := Analyze(parse(t, src), cost.DefaultPrices, nil)
		for _, p := range r.Partitions {
			if p.PK == "ORG#{orgId}#NOTES" {
				return p.Members[0].Count
			}
		}
		t.Fatalf("no partition for the Recent index")
		return Estimate{}
	}
	if got := recent(note("{ per: Thing, typical: 0.2, max: 50 }")); got.Typical != 2 || got.Max != 500 {
		t.Errorf("from the chain: %+v, want 2 typically and 500 at most", got)
	}
	if got := recent(note("{ per: Thing, typical: 0.2, max: 50, by: { Org: { typical: 2, max: 40 } } }")); got.Typical != 2 || got.Max != 40 {
		t.Errorf("declared by Org: %+v, want 2 typically and 40 at most", got)
	}
	// Saying the parent's numbers twice is refused, not picked between.
	_, err := schema.Parse([]byte(note("{ per: Thing, typical: 0.2, max: 50, by: { Thing: { typical: 1, max: 9 } } }")))
	if err == nil || !strings.Contains(err.Error(), "the volume already counts per Thing") {
		t.Errorf("by repeating per: %v", err)
	}
}

// A sparse index holds the items its where matches. With the share declared (matches), its
// partitions, their traffic and the writes that maintain it are sized by it; without, every item
// is counted and the result is marked as the most it can be.
func TestSparseIndexIsSizedByItsShare(t *testing.T) {
	src := func(matches string) string {
		return strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    indexes:
      Binned:
        pk: "BIN"
        sk: "T#{orgId}#{thingId}"
        where: { kind: b }
        project: keys
`+matches+`    access:
      Binned: { query: Binned, rate: 1 }
    writes:
      Add: { create: true, rate: 10 }
      AddA: { create: true, set: { kind: a }, rate: 10 }
      Bin: { set: { kind: b }, when: { kind: a }, rate: 1 }
      Rename: { update: [name], rate: 10 }
    volume: { typical: 10, max: 1000 }
`, 1)
	}
	binned := func(r *Result) (*Partition, *Member) {
		t.Helper()
		for _, p := range r.Partitions {
			if p.PK == "BIN" {
				return p, p.Members[0]
			}
		}
		t.Fatal("no partition for the Binned index")
		return nil, nil
	}
	units := func(r *Result, write string) float64 {
		t.Helper()
		for _, er := range r.Cost.Entities {
			for _, wc := range er.Writes {
				if wc.Write.Entity.Name != "Thing" || wc.Write.Name != write {
					continue
				}
				for _, tc := range wc.Touches {
					if tc.Target.Index != nil {
						return tc.Units.P50
					}
				}
				return 0
			}
		}
		t.Fatalf("no write %s", write)
		return 0
	}

	// Not declared: all 1,000 things are counted, as a bound.
	r := Analyze(parse(t, src("")), cost.DefaultPrices, nil)
	p, mb := binned(r)
	if !mb.Bound || !p.Bound() || mb.Count.Typical != 1000 {
		t.Errorf("without matches: bound %v, count %+v; want a bound of 1,000", mb.Bound, mb.Count)
	}
	if !has(r, "low-cardinality-key", Note, "Thing.Binned", `every Thing with kind = "b" lands in one partition`) ||
		!has(r, "low-cardinality-key", Note, "Thing.Binned", "at most (if every Thing matched") {
		t.Errorf("the finding should speak of matching things, and of a bound:\n%s", dump(r))
	}
	if st := r.Reads[r.Model.Entity("Thing").Access[0]]; !st.Filtered {
		t.Errorf("the read should say fewer items qualify than counted")
	}
	loose := p.PeakWRU

	// One thing in a hundred is binned.
	r = Analyze(parse(t, src("        matches: 0.01\n")), cost.DefaultPrices, nil)
	p, mb = binned(r)
	if mb.Bound || mb.Count.Typical != 10 || p.Count.Typical != 1 {
		t.Errorf("with matches: bound %v, count %+v, %v partitions; want 10 things in one partition", mb.Bound, mb.Count, p.Count.Typical)
	}
	if has(r, "low-cardinality-key", Note, "Thing.Binned", "at most (if every") {
		t.Errorf("a sized index isn't a bound:\n%s", dump(r))
	}
	if st := r.Reads[r.Model.Entity("Thing").Access[0]]; st.Filtered || st.Items.Typical != 10 {
		t.Errorf("the read returns %+v, want 10 items and no caveat", st.Items)
	}
	// What a write pays for the index follows what it says about the item: a create that may or
	// may not match pays the share; one that sets another kind pays nothing; binning always adds
	// the entry; a rename touches no index key, and the index projects nothing.
	for write, want := range map[string]float64{"Add": 0.01, "AddA": 0, "Bin": 1, "Rename": 0} {
		if got := units(r, write); got != want {
			t.Errorf("%s pays %v WRU for the index typically, want %v", write, got, want)
		}
	}
	// 10 creates/s at 1% and one binning a second, against 21 writes/s counted in full.
	if p.PeakWRU >= loose || p.PeakWRU < 1 || p.PeakWRU > 1.2 {
		t.Errorf("peak on the index partition: %v WRU/s (was %v as a bound), want about 1.1", p.PeakWRU, loose)
	}
}

// A Query reads what its partition holds, however large its page: one sized to return a whole
// partition in a call is costed at the partition, not at a page that is never full. At worst the
// page is what bounds it.
func TestQueryCostIsWhatThePartitionHolds(t *testing.T) {
	src := strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    access:
      List: { query: key, page: 500, max_page: 1000, rate: 4 }
      Whole: { query: partition, of: [Org, Thing], page: 500, max_page: 1000, consistent: true }
    writes:
      Add: create
    volume: { typical: 10, max: 1000 }
`, 1)
	m := parse(t, src)
	full := cost.Analyze(m, cost.DefaultPrices)
	r := Analyze(m, cost.DefaultPrices, nil)
	read := func(rep *cost.Report, name string) cost.ReadCost {
		t.Helper()
		for _, rc := range rep.Entity(m.Entity("Thing")).Reads {
			if rc.Access.Name == name {
				return rc
			}
		}
		t.Fatalf("no read %s", name)
		return cost.ReadCost{}
	}
	// Ten things of about 230 bytes are one 4 KB unit, read eventually: half an RRU, where a full
	// page of 500 would be many. The largest org's thousand things fill the page.
	was, now := read(full, "List"), read(r.Cost, "List")
	if now.RRU.P50 != 0.5 || was.RRU.P50 < 10 || now.RRU.P99 != was.RRU.P99 {
		t.Errorf("List: %v RRU (a full page: %v), want 0.5 typically and the page at worst", now.RRU, was.RRU)
	}
	if now.Monthly >= was.Monthly/10 || r.Cost.MonthlyUSD >= full.MonthlyUSD {
		t.Errorf("the monthly cost should follow: $%.2f a month (a full page: $%.2f); total $%.2f (was $%.2f)", now.Monthly, was.Monthly, r.Cost.MonthlyUSD, full.MonthlyUSD)
	}
	// A partition read is everything under the key: the org and its ten things, one unit.
	if whole := read(r.Cost, "Whole"); whole.RRU.P50 != 1 {
		t.Errorf("Whole: %v RRU, want 1 typically", whole.RRU)
	}
	// Without volumes nothing says the partition is smaller than a page.
	bare := parse(t, strings.Replace(strings.Replace(src, "    volume: { typical: 10, max: 1000 }\n", "", 1), "    volume: 100\n", "", 1))
	if got, want := Analyze(bare, cost.DefaultPrices, nil).Cost.Entity(bare.Entity("Thing")).Reads[0].RRU, cost.Analyze(bare, cost.DefaultPrices).Entity(bare.Entity("Thing")).Reads[0].RRU; got != want {
		t.Errorf("without volumes: %v RRU, want the full page's %v", got, want)
	}
}

// A busy partition key: every Thing write lands on its org's partition, and the counter keyed by
// the org takes every write of the org's things.
func TestHotPartition(t *testing.T) {
	src := strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    counters:
      Things: { pk: "ORG#{orgId}", sk: "COUNT", values: { n: count } }
    writes:
      Add: { create: true, rate: 400 }
    volume: { typical: 10, max: 1000 }
`, 1)
	src = strings.Replace(src, "table: { name: things }", "table: { name: things }\nworkload: { peak: 2 }", 1)
	r := Analyze(parse(t, src), cost.DefaultPrices, nil)
	// The biggest org has 1,000 of 1,000 things: all 800 peak writes/s land on it.
	if !has(r, "hot-counter", Error, "Thing.Things", "at 800 writes/s to one Things key") {
		t.Errorf("hot counter:\n%s", dump(r))
	}
	if !has(r, "hot-partition", Warning, "Thing", "the busiest base table key (ORG#{orgId})") {
		t.Errorf("hot partition:\n%s", dump(r))
	}
}

// Every write to a counter item contends with the others: three writes of 15 transactions/s each
// are 45 on the item, over the ~20 a second where conflicts become routine.
func TestCounterContentionSumsWrites(t *testing.T) {
	src := strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    counters:
      Kinds: { pk: "ORG#{orgId}", sk: "KINDS", values: { a: { count: true, where: { kind: a } } } }
    writes:
      Add: { create: true, hot_key_rate: 15 }
      MakeA: { set: { kind: a }, when: { kind: b }, hot_key_rate: 15 }
      MakeB: { set: { kind: b }, when: { kind: a }, hot_key_rate: 15 }
    volume: { typical: 10, max: 1000 }
`, 1)
	r := Analyze(parse(t, src), cost.DefaultPrices, nil)
	if !has(r, "counter-contention", Warning, "Thing.Kinds", "at 45 writes/s to one Kinds key (Thing.Add 15/s, declared hot_key_rate; Thing.MakeA 15/s") {
		t.Errorf("summed contention:\n%s", dump(r))
	}
}

func TestAcceptance(t *testing.T) {
	withIndex := strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n",
		"    indexes:\n      ByName: { pk: \"ORG#{orgId}#N\", sk: \"N#{thingId}\", project: keys, accept: { %s: \"%s\" } }\n    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", 1)
	for _, c := range []struct {
		name, rule, reason, want string
	}{
		{"accepted", "unused-index", "read by the reporting job through the export", ""},
		{"unknown rule", "nope", "because", "there is no rule \"nope\""},
		{"nothing to accept", "sparse-index", "because", "has no sparse-index finding to accept"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := strings.ReplaceAll(strings.Replace(withIndex, "%s: \"%s\"", c.rule+": \""+c.reason+"\"", 1), "%s", "")
			r := Analyze(parse(t, src), cost.DefaultPrices, nil)
			if c.want == "" {
				if len(r.Open()) != 0 || len(r.Findings) != 1 || r.Findings[0].Accepted == nil {
					t.Errorf("not accepted:\n%s", dump(r))
				}
				return
			}
			if !has(r, "accept-invalid", Error, "Thing.ByName", c.want) {
				t.Errorf("want accept-invalid %q:\n%s", c.want, dump(r))
			}
		})
	}
	// Errors can't be accepted.
	src := strings.Replace(base, "      body: { type: string, size: 100 }\n", "      body: { type: string, size: 1000/500000 }\n", 1)
	src = strings.Replace(src, "    volume: { typical: 10, max: 1000 }\n", "    volume: { typical: 10, max: 1000 }\n    accept: { item-too-large: \"we will see\" }\n", 1)
	r := Analyze(parse(t, src), cost.DefaultPrices, nil)
	if !has(r, "accept-invalid", Error, "Thing", "is an error, which can't be accepted") || !has(r, "item-too-large", Error, "Thing", "") {
		t.Errorf("accepting an error:\n%s", dump(r))
	}
}

// An acceptance on an entity covers the rule's findings about its fields (and whatever else it
// declares), so a reason shared by all of them is given once. One on the field itself wins, and
// an entity-wide acceptance with nothing under it to accept is still an error.
func TestAcceptanceForAWholeEntity(t *testing.T) {
	note := func(fieldAccept, entityAccept string) string {
		return base + `  Note:
    fields:
      orgId: string
      thingId: string
      noteId: string
      thingName: { type: string, copy_of: Thing.name` + fieldAccept + ` }
      thingBody: { type: string, copy_of: Thing.body }
    key: { pk: "ORG#{orgId}#T#{thingId}", sk: "NOTE#{noteId}" }
    writes:
      Add: create
    volume: { per: Thing, typical: 5, max: 50 }
` + entityAccept
	}
	reasons := func(r *Result) map[string]string {
		out := map[string]string{}
		for _, f := range r.Findings {
			if f.Rule == "copy-drift" && f.Accepted != nil {
				out[f.Subject.String()] = f.Accepted.Reason
			}
		}
		return out
	}
	r := Analyze(parse(t, note("", "    accept: { copy-drift: \"one pass rewrites them\" }\n")), cost.DefaultPrices, nil)
	if got := reasons(r); len(r.Open()) != 0 || got["Note.thingName"] != "one pass rewrites them" || got["Note.thingBody"] != "one pass rewrites them" {
		t.Errorf("accepted on the entity: %v\n%s", got, dump(r))
	}
	r = Analyze(parse(t, note(", accept: { copy-drift: \"names never change\" }", "    accept: { copy-drift: \"one pass rewrites them\" }\n")), cost.DefaultPrices, nil)
	if got := reasons(r); len(r.Open()) != 0 || got["Note.thingName"] != "names never change" || got["Note.thingBody"] != "one pass rewrites them" {
		t.Errorf("the field's own acceptance wins: %v\n%s", got, dump(r))
	}
	r = Analyze(parse(t, note("", "    accept: { sparse-index: \"because\" }\n")), cost.DefaultPrices, nil)
	if !has(r, "accept-invalid", Error, "Note", "neither entity Note nor anything it declares has a sparse-index finding") {
		t.Errorf("nothing under the entity to accept:\n%s", dump(r))
	}
	// It doesn't reach another entity's findings.
	r = Analyze(parse(t, strings.Replace(note("", ""), "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n",
		"    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n    accept: { copy-drift: \"because\" }\n", 1)), cost.DefaultPrices, nil)
	if !has(r, "accept-invalid", Error, "Thing", "") || len(reasons(r)) != 0 {
		t.Errorf("accepted on another entity:\n%s", dump(r))
	}
}

func TestPolicy(t *testing.T) {
	p, err := ParsePolicy([]byte(`
fail_on: warning
rules: { unused-index: off, sparse-index: error }
limits: { item_size: 1KB, transaction_items: 1, indexes_per_entity: 1, partition_size: 1KB }
require: { volumes: true, rates: true, freshness: true }
`))
	if err != nil {
		t.Fatal(err)
	}
	src := strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    indexes:
      ByName: { pk: "ORG#{orgId}#N", sk: "{name}#{thingId}", project: keys }
      Other: { pk: "ORG#{orgId}#O", sk: "O#{thingId}", project: keys }
    counters:
      Count: { pk: "ORG#{orgId}", sk: "COUNT", values: { n: count } }
    access:
      L: { query: ByName }
    writes:
      Add: create
    volume: { typical: 10, max: 1000 }
`, 1)
	r := Analyze(parse(t, src), cost.DefaultPrices, p)
	for _, w := range []struct {
		rule    string
		sev     Severity
		subject string
	}{
		{"sparse-index", Error, "Thing.ByName"},
		{"transaction-items-limit", Warning, "Thing.Add"},
		{"index-limit", Warning, "Thing"},
		{"partition-size-limit", Warning, "Thing"},
		{"freshness-unstated", Warning, "Thing.L"},
		{"rate-missing", Warning, "Thing.Add"},
	} {
		if !has(r, w.rule, w.sev, w.subject, "") {
			t.Errorf("want %s %s on %s:\n%s", w.sev, w.rule, w.subject, dump(r))
		}
	}
	if has(r, "unused-index", Warning, "Thing.Other", "") {
		t.Error("a rule the policy turned off still reported")
	}
	// Exceeding the index limit doesn't stop the other index rules: Other is still sparse-checked.
	src2 := strings.Replace(src, `Other: { pk: "ORG#{orgId}#O", sk: "O#{thingId}", project: keys }`, `Other: { pk: "ORG#{orgId}#O", sk: "{name}#{thingId}", project: keys }`, 1)
	if r2 := Analyze(parse(t, src2), cost.DefaultPrices, p); !has(r2, "sparse-index", Error, "Thing.Other", "") {
		t.Errorf("the index limit hid later index findings:\n%s", dump(r2))
	}
	if len(r.Failing()) == 0 {
		t.Error("fail_on: warning doesn't fail on warnings")
	}
	for src, want := range map[string]string{
		"fail_on: sometimes":             "must be error, warning or note",
		"rules: { nope: off }":           "there is no rule",
		"rules: { item-too-large: off }": "which a policy can't change",
		"limits: { item_size: big }":     "is not a size",
		"requires: { volumes: true }":    `unknown key "requires"`,
		"limits: { gsi: 3 }":             `unknown key "gsi" in limits`,
	} {
		if _, err := ParsePolicy([]byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", src, want, err)
		}
	}
}

func TestRulesAreDocumented(t *testing.T) {
	guide, err := os.ReadFile("../../docs/guides/analysis.md")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range Rules {
		if seen[r.ID] {
			t.Errorf("rule %s listed twice", r.ID)
		}
		seen[r.ID] = true
		if r.Summary == "" {
			t.Errorf("rule %s has no summary", r.ID)
		}
		if !strings.Contains(string(guide), "| `"+r.ID+"` |") {
			t.Errorf("rule %s is missing from the table in docs/guides/analysis.md", r.ID)
		}
	}
}

// A scan's pages evaluate the whole base table, and its full pass reads the base table without
// GSI entries.
func TestScanReadsTheWholeTable(t *testing.T) {
	src := strings.Replace(base, "    writes:\n      Create: create\n    volume: 100\n", `    access:
      Export: { scan: true, page: 100, reason: the nightly export }
    writes:
      Create: create
    volume: 100
`, 1)
	src = strings.Replace(src, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    indexes:
      ByKind: { pk: "ORG#{orgId}#K#{kind}", sk: "T#{thingId}", project: all }
    access:
      K: { query: ByKind }
    writes:
      Add: create
    volume: { typical: 1000 }
`, 1)
	r := Analyze(parse(t, src), cost.DefaultPrices, nil)
	st := r.Reads[findAccess(r.Model, "Org", "Export")]
	// 100 orgs and 100,000 things: 1,001 pages of 100, though only 100 items are orgs.
	if st.Items.Typical != 100 || st.Pages.Typical != 1001 {
		t.Errorf("scan stats = %+v", st)
	}
	if r.Cost.BaseBytes >= r.Cost.StorageBytes/1.5 {
		t.Errorf("base bytes %v include the GSI (storage %v)", r.Cost.BaseBytes, r.Cost.StorageBytes)
	}
}

// A partition hot from reads is blamed on what is read, not on whichever entity is listed first.
func TestHotPartitionFromReads(t *testing.T) {
	src := strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    access:
      List: { query: key, rate: 5000 }
    writes:
      Add: create
    volume: { typical: 10, max: 1000 }
`, 1)
	r := Analyze(parse(t, src), cost.DefaultPrices, nil)
	if !has(r, "hot-partition", Warning, "Thing", "the busiest base table key (ORG#{orgId})") {
		t.Errorf("read-hot partition:\n%s", dump(r))
	}
}

// A GSI entry whose partition key moves is a delete on one key and a put on another: each key
// takes one of them, not both.
func TestMovedKeyLoadsEachKeyOnce(t *testing.T) {
	src := strings.Replace(base, "    writes:\n      Add: create\n    volume: { typical: 10, max: 1000 }\n", `    indexes:
      ByKind: { pk: "ORG#{orgId}#K#{kind}", sk: "T#{thingId}", project: keys }
    access:
      K: { query: ByKind }
    writes:
      Add: create
      Flip: { set: { kind: b }, when: { kind: a }, hot_key_rate: 100 }
    volume: { typical: 10, max: 1000 }
`, 1)
	r := Analyze(parse(t, src), cost.DefaultPrices, nil)
	for _, p := range r.Partitions {
		if p.PK == "ORG#{orgId}#K#{kind}" {
			if p.PeakWRU != 100 {
				t.Errorf("ByKind's busiest key takes %v WRU/s, want 100 (one 1 KB entry per flip)", p.PeakWRU)
			}
			return
		}
	}
	t.Fatalf("no ByKind partition:\n%s", dump(r))
}
