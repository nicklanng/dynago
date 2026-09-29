package e2e_test

import (
	"testing"
	"time"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/examples/toollibrary"
)

// stamps reads an item's stored timestamps.
func (e env) stamps(t *testing.T, pk, sk string) (created, updated string) {
	t.Helper()
	it := e.raw(t, pk, sk)
	if it == nil {
		t.Fatalf("no item %s / %s", pk, sk)
	}
	c, _ := it[dynago.AttrCreated].(string)
	u, _ := it[dynago.AttrUpdated].(string)
	return c, u
}

// Every row dynago writes records when it was created and last changed, on every write path.
func TestTimestamps(t *testing.T) {
	e := setup(t)
	start := dynago.FmtTime(time.Now())
	later := func(t *testing.T, what, before, after string) {
		t.Helper()
		if after <= before {
			t.Fatalf("%s: updated %q is not after %q", what, after, before)
		}
	}

	// A create stamps both, and the entity knows them.
	tool := &toollibrary.Tool{LibraryID: "lib1", ToolID: "t1", Name: "Drill", Category: toollibrary.ToolCategoryPower, SerialNumber: "SN-1"}
	must(t, e.st.Tools.Add(ctx, tool))
	ts := tool.Timestamps()
	if ts.Created.IsZero() || !ts.Created.Equal(ts.Updated) {
		t.Fatalf("after Add: %+v", ts)
	}
	got, err := e.st.Tools.Get(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t1"})
	must(t, err)
	if got.Timestamps() != ts {
		t.Fatalf("read back %+v, created with %+v", got.Timestamps(), ts)
	}
	toolCreated, toolUpdated := e.stamps(t, "LIB#lib1#TOOL#t1", "TOOL")
	if toolCreated < start || toolCreated != toolUpdated {
		t.Fatalf("stored tool stamps %q %q", toolCreated, toolUpdated)
	}

	// A create that fails leaves the entity without timestamps: nothing was stored.
	dup := &toollibrary.Tool{LibraryID: "lib1", ToolID: "t1", Name: "Drill again", Category: toollibrary.ToolCategoryPower}
	wantErr(t, e.st.Tools.Add(ctx, dup), toollibrary.ErrToolExists)
	if ts := dup.Timestamps(); !ts.Created.IsZero() {
		t.Fatalf("a failed create left timestamps %+v", ts)
	}

	// Claims and counters are stamped too.
	if c, u := e.stamps(t, "UNIQUE#Tool.Serial#lib1#SN-1", "UNIQUE"); c == "" || u == "" {
		t.Fatalf("serial claim stamps %q %q", c, u)
	}
	countsCreated, countsUpdated := e.stamps(t, "LIB#lib1", "COUNTS#TOOLS")
	if countsCreated == "" || countsUpdated == "" {
		t.Fatalf("counter stamps %q %q", countsCreated, countsUpdated)
	}

	// A single UpdateItem (the shape with no derived items) moves updated, not created.
	e.join(t, "alice", toollibrary.MemberRoleSteward, 3)
	aliceCreated, aliceUpdated := e.stamps(t, "LIB#lib1", "MEMBER#alice")
	must(t, e.st.Members.SetLoanCap(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "alice"}, toollibrary.MemberSetLoanCap{MaxLoans: 5}))
	c, u := e.stamps(t, "LIB#lib1", "MEMBER#alice")
	later(t, "SetLoanCap", aliceUpdated, u)
	if c != aliceCreated {
		t.Fatalf("SetLoanCap changed created: %q → %q", aliceCreated, c)
	}

	// A read-first update rewrites the item and keeps created.
	aliceUpdated = u
	must(t, e.st.Members.UpdateProfile(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "alice"}, toollibrary.MemberUpdateProfile{Name: dynago.Ptr("Alice")}))
	c, u = e.stamps(t, "LIB#lib1", "MEMBER#alice")
	later(t, "UpdateProfile", aliceUpdated, u)
	if c != aliceCreated {
		t.Fatalf("UpdateProfile changed created: %q → %q", aliceCreated, c)
	}

	// A change made through requires (Borrow sets the tool on loan) stamps the tool, and the
	// counter it moves; the loan's copy carries the loan's creation time.
	must(t, e.borrow(t, "l1", "t1", "alice", 5))
	c, u = e.stamps(t, "LIB#lib1#TOOL#t1", "TOOL")
	later(t, "Borrow's change to the tool", toolUpdated, u)
	if c != toolCreated {
		t.Fatalf("Borrow changed the tool's created: %q → %q", toolCreated, c)
	}
	toolUpdated = u
	if cc, cu := e.stamps(t, "LIB#lib1", "COUNTS#TOOLS"); cc != countsCreated || cu <= countsUpdated {
		t.Fatalf("counter after Borrow: created %q (was %q), updated %q (was %q)", cc, countsCreated, cu, countsUpdated)
	}
	loanCreated, _ := e.stamps(t, "LIB#lib1#TOOL#t1", "LOAN#l1")
	var copies []map[string]any
	must(t, e.table.Get("PK", "LIB#lib1#MEMBER#alice").Range("SK", "BEGINS_WITH", "MYLOAN#").Consistent(true).All(ctx, &copies))
	if len(copies) != 1 || copies[0][dynago.AttrCreated] != loanCreated || copies[0][dynago.AttrUpdated] == nil {
		t.Fatalf("copy stamps: %+v (loan created %q)", copies, loanCreated)
	}

	// A read-free transition (Return, then Retire) stamps without reading.
	must(t, e.st.Loans.Return(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t1", LoanID: "l1"}, toollibrary.LoanReturn{ReturnedAt: time.Now()}))
	_, u = e.stamps(t, "LIB#lib1#TOOL#t1", "TOOL")
	later(t, "Return's change to the tool", toolUpdated, u)
	toolUpdated = u
	must(t, e.st.Tools.Retire(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t1"}))
	c, u = e.stamps(t, "LIB#lib1#TOOL#t1", "TOOL")
	later(t, "Retire", toolUpdated, u)
	if c != toolCreated {
		t.Fatalf("Retire changed created: %q → %q", toolCreated, c)
	}

	// An item written before dynago kept timestamps keeps an unknown creation time when updated.
	must(t, e.table.Put(map[string]any{"PK": "LIB#lib1", "SK": "MEMBER#old", "_t": "Member", "_v": 1, "_rev": 7,
		"libraryId": "lib1", "memberId": "old", "email": "old@example.org", "status": "active", "role": "member"}).Run(ctx))
	must(t, e.st.Members.UpdateProfile(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "old"}, toollibrary.MemberUpdateProfile{Name: dynago.Ptr("Old")}))
	old, err := e.st.Members.Get(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "old"})
	must(t, err)
	if ts := old.Timestamps(); !ts.Created.IsZero() || ts.Updated.IsZero() {
		t.Fatalf("an old item's timestamps after an update: %+v", ts)
	}
}
