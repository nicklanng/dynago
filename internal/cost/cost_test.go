package cost

import (
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/schema"
)

func load(t *testing.T, path string) *schema.Model {
	t.Helper()
	m, err := schema.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func find(r *Report, entity, write string) WriteCost {
	for _, er := range r.Entities {
		if er.Entity.Name != entity {
			continue
		}
		for _, wc := range er.Writes {
			if wc.Write.Name == write {
				return wc
			}
		}
	}
	return WriteCost{}
}

func TestWriteCosts(t *testing.T) {
	r := Analyze(load(t, "../../examples/toollibrary/toollibrary.dynago.yaml"), DefaultPrices)
	// Borrow: the loan, its copy and two counters; a check of the member; the tool and its counter
	// (changed without reading it); and the hold (deleted if there is one). One transaction.
	b := find(r, "Loan", "Borrow")
	items := strings.Join(b.Items, ",")
	if !b.Transactional || b.MaxTxItems != 8 || b.ReadFirst || b.RRU != 0 ||
		!strings.Contains(items, "Tool's counter ToolCounts") || !strings.Contains(items, "Hold (deletes the Hold if there is one)") {
		t.Errorf("Borrow = %+v", b)
	}
	// Suspend reads first (the steward count depends on the role it doesn't pin): the member and
	// its counter in one transaction.
	if s := find(r, "Member", "Suspend"); !s.ReadFirst || s.RRU != 1 || !s.Transactional || s.MaxTxItems != 2 {
		t.Errorf("Suspend = %+v", s)
	}
	// Retire is read-free, cancelling any hold in the same transaction.
	if s := find(r, "Tool", "Retire"); s.ReadFirst || s.RRU != 0 || s.MaxTxItems != 3 {
		t.Errorf("Retire = %+v", s)
	}
	// Return moves the loan out of the copy index and hands the tool back.
	ret := find(r, "Loan", "Return")
	if !strings.Contains(strings.Join(ret.Items, ","), `Tool (sets its status to "available")`) || ret.MaxTxItems != 6 {
		t.Errorf("Return = %+v", ret)
	}
	// Leave checks the member's loan counter: a condition check billed as a counter-sized write.
	if l := find(r, "Member", "Leave"); !strings.Contains(strings.Join(l.Items, ","), "check counter MemberLoans") {
		t.Errorf("Leave = %+v", l)
	}
	// SetLoanCap feeds nothing derived: a single UpdateItem.
	if s := find(r, "Member", "SetLoanCap"); s.Transactional || s.ReadFirst {
		t.Errorf("SetLoanCap = %+v", s)
	}
	// Every touch names the family of items it writes, for the partition analysis.
	for _, tc := range b.Touches {
		if tc.Target.Entity == nil || tc.Label == "" {
			t.Errorf("Borrow touch without a target: %+v", tc)
		}
	}
}

// Storage follows the declared volumes: a total, or a number per parent multiplied down.
func TestStorageFollowsVolumes(t *testing.T) {
	r := Analyze(load(t, "../../examples/toollibrary/toollibrary.dynago.yaml"), DefaultPrices)
	for _, er := range r.Entities {
		if er.Entity.Name == "Loan" && (er.Entity.Count != 2000*60*20 || er.StorageGB < 1) {
			t.Errorf("Loan: count %v, storage %.2f GB", er.Entity.Count, er.StorageGB)
		}
	}
	if r.StorageBytes <= 0 {
		t.Error("no table storage")
	}
}

// A scan's cost per page follows the page size, and its full pass the whole table.
func TestScanCost(t *testing.T) {
	m, err := schema.Parse([]byte(`
dynago: 1
package: things
table: { name: things }
entities:
  Thing:
    fields:
      id: string
      body: { type: string, size: 1000 }
    key: { pk: "T#{id}", sk: "THING" }
    access:
      Export: { scan: true, page: 100, reason: nightly export to the warehouse }
    volume: 1000000
`))
	if err != nil {
		t.Fatal(err)
	}
	r := Analyze(m, DefaultPrices)
	rc := r.Entities[0].Reads[0]
	if rc.Requests != "Scan (one page)" || rc.FullPassRRU < 100000 {
		t.Errorf("scan cost = %+v", rc)
	}
}
