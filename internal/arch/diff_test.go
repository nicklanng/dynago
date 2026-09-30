package arch

import (
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/analysis"
	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/schema"
)

const before = `
dynago: 1
package: shop
table: { name: shop }
workload: { peak: 3 }
entities:
  Shop:
    fields: { shopId: string }
    key: { pk: "SHOP#{shopId}", sk: "SHOP" }
    volume: 100
  Order:
    fields:
      shopId: string
      orderId: string
      customerId: { type: string, required: true }
      status: { type: enum, values: [open, closed] }
      placedAt: time
    key: { pk: "SHOP#{shopId}", sk: "ORDER#{orderId}" }
    indexes:
      Recent:
        pk: "SHOP#{shopId}#RECENT"
        sk: "AT#{placedAt}#{orderId}"
        project: [status]
    access:
      Get: get
      Recent: { query: Recent, freshness: eventual, rate: 5 }
    writes:
      Place: { create: true, set: { status: open }, rate: 2 }
      Close: { set: { status: closed }, when: { status: open } }
    volume: { typical: 1000, max: 50000 }
`

func snapshot(t *testing.T, src string) *Snapshot {
	t.Helper()
	m, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return Build(analysis.Analyze(m, cost.DefaultPrices, nil))
}

// diff compares two schemas as `dynago diff` does, with the lock history started at before.
func diff(t *testing.T, before, after string) string {
	t.Helper()
	parse := func(src string) *schema.Model {
		m, err := schema.Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	return Diff(snapshot(t, before), snapshot(t, after), CheckStorage(parse(before), nil, parse(after)), "main", "branch")
}

func TestNoChange(t *testing.T) {
	if out := diff(t, before, before); out != "" {
		t.Errorf("identical schemas differ:\n%s", out)
	}
	// Documentation isn't architecture.
	docs := strings.Replace(before, "    fields: { shopId: string }", "    doc: A shop.\n    fields: { shopId: string }", 1)
	if out := diff(t, before, docs); out != "" {
		t.Errorf("a doc change is an architecture change:\n%s", out)
	}
}

// Making the recent orders read-your-writes turns the index into a copy: the diff shows the index,
// the read, the writes it now makes transactions, and that existing items need a migration.
func TestFreshnessChange(t *testing.T) {
	after := strings.Replace(before, "Recent: { query: Recent, freshness: eventual, rate: 5 }", "Recent: { query: Recent, freshness: immediate, rate: 5 }", 1)
	out := diff(t, before, after)
	for _, want := range []string{
		"### `shop`: architecture changes (main → branch)",
		"**`dynago generate` will refuse this change:**",
		"Set `version: 2` so stored items record which shape wrote them, and start a new table generation (`table.generation: 2`",
		"index Recent",
		"**~** `Order.Recent`: GSI (maintained by DynamoDB, eventually consistent) → copy (written in the write's transaction, read-your-writes) (Recent needs immediate freshness)",
		"**~** `Order.Recent`: Query of GSI Recent → Query of copy Recent; eventual → immediate",
		"**~** `Order.Place`: single item → transaction of 2 items",
		"#### Partitions",
		"`SHOP#{shopId}#RECENT` (base table)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
}

// A new generation estimates the copy from the old table's declared volumes.
func TestNewGeneration(t *testing.T) {
	after := strings.Replace(before, "table: { name: shop }", "table: { name: shop, generation: 2, retain: [1] }", 1)
	after = strings.Replace(after, "      placedAt: time\n", "      placedAt: time\n      total: { type: int, required: true }\n", 1)
	after = strings.Replace(after, "  Order:\n", "  Order:\n    version: 2\n", 1)
	out := diff(t, before, after)
	for _, want := range []string{
		"**Needs a new table generation (1 → 2):**",
		"about 100,100 items",
		"field total added as required",
		"**+** `Order.total` (int, required)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
}

// A new rule or a new risk shows up as a finding; an accepted one says so.
func TestFindingChanges(t *testing.T) {
	after := strings.Replace(before, "      placedAt: time\n", "      placedAt: time\n      note: string\n", 1)
	after = strings.Replace(after, "    access:\n      Get: get\n", `      ByNote:
        pk: "SHOP#{shopId}#NOTE"
        sk: "{note}#{orderId}"
        project: keys
        accept: { sparse-index: "orders without a note needn't be found by one" }
    access:
      Get: get
`, 1)
	out := diff(t, before, after)
	for _, want := range []string{
		"**+** `Order.ByNote`: GSI",
		"**+** warning `unused-index`, index Order.ByNote",
		"**+** warning `sparse-index`, index Order.ByNote, accepted: orders without a note needn't be found by one",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
}

func TestNewSchema(t *testing.T) {
	out := Diff(nil, snapshot(t, before), Storage{}, "main", "branch")
	if !strings.Contains(out, "A new table, `shop-g1`.") || !strings.Contains(out, "**+** `Order.Recent`") {
		t.Errorf("new schema diff:\n%s", out)
	}
}

// A change only the lock sees (a generation bump, a stored attribute renamed) still leads the diff,
// even when no section has anything to show.
func TestStorageOnlyChanges(t *testing.T) {
	gen := strings.Replace(before, "table: { name: shop }", "table: { name: shop, generation: 2, retain: [1] }", 1)
	if out := diff(t, before, gen); !strings.Contains(out, "**Needs a new table generation (1 → 2):**") {
		t.Errorf("generation bump:\n%s", out)
	}
	attr := strings.Replace(before, "      customerId: { type: string, required: true }", "      customerId: { type: string, required: true, attr: cust }", 1)
	if out := diff(t, before, attr); !strings.Contains(out, "**`dynago generate` will refuse this change:**") || !strings.Contains(out, "table.generation: 2") {
		t.Errorf("attribute renamed:\n%s", out)
	}
}

// The diff agrees with the lock: a compatible change still needs a version bump.
func TestInPlaceChangeNeedsAVersion(t *testing.T) {
	added := strings.Replace(before, "      placedAt: time\n", "      placedAt: time\n      note: string\n", 1)
	if out := diff(t, before, added); !strings.Contains(out, "will refuse this change") || !strings.Contains(out, "Set `version: 2`") {
		t.Errorf("unversioned:\n%s", out)
	}
	versioned := strings.Replace(added, "  Order:\n", "  Order:\n    version: 2\n", 1)
	if out := diff(t, before, versioned); !strings.Contains(out, "Existing items fit these storage changes, so they happen in place:\n\n- Order: v1 → v2: field note added") {
		t.Errorf("versioned:\n%s", out)
	}
}

// The workload scales every partition's load, so a change to it is architectural.
func TestWorkloadChange(t *testing.T) {
	after := strings.Replace(before, "workload: { peak: 3 }", "workload: { peak: 30, horizon: 5 years }", 1)
	out := diff(t, before, after)
	for _, want := range []string{"#### Workload", "**~** peak: 3× → 30× the average rate", "**~** horizon: unknown → 5 years"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
}
