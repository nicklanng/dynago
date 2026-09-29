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
	var msgs []string
	for _, f := range r.Findings {
		msgs = append(msgs, f.Message)
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"copies Tool.name", "manual (p99"} {
		if !strings.Contains(all, want) {
			t.Errorf("findings lack %q:\n%s", want, all)
		}
	}
}

func TestFindings(t *testing.T) {
	src := `
dynago: 1
package: big
table: { name: big }
entities:
  Doc:
    fields:
      id: string
      body: { type: string, size: 1000/500000 }
    key: { pk: "DOC#{id}", sk: "DOC" }
    counters:
      Total:
        pk: "TOTAL"
        sk: "TOTAL"
        values:
          n: count
    writes:
      Put: { create: true, hot_key_rate: 800 }
`
	m, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	r := Analyze(m, DefaultPrices)
	var msgs []string
	for _, f := range r.Findings {
		msgs = append(msgs, string(f.Severity)+": "+f.Message)
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"error: p99 item size", "error: at 800 writes/s to one Total key"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if !r.HasErrors() {
		t.Error("HasErrors = false")
	}
}
