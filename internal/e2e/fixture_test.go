package e2e_test

import (
	"testing"

	"github.com/nicklanng/dynago/dynagotest"
	"github.com/nicklanng/dynago/internal/e2e/fixture"
)

// HandOver closes an account and activates its successor in one transaction. Both feed the
// tenant's counter item, so their changes must be merged into one update (DynamoDB refuses two
// operations on one item), and the successor is read first, because its region counter is keyed
// by a field the requirement doesn't pin.
func TestHandOverChangesAnotherItemOfTheSameEntity(t *testing.T) {
	db, n := dynagotest.CountingDB(t)
	st := fixture.New(db, dynagotest.Table(t, db, fixture.TableSpec))
	open := func(id, region string) {
		must(t, st.Accounts.Open(ctx, &fixture.Account{TenantID: "t1", AccountID: id, Region: region, Status: fixture.AccountStatusPending}))
	}
	open("a1", "north")
	open("a2", "north")
	open("a3", "south")
	must(t, st.Accounts.Activate(ctx, fixture.AccountKey{TenantID: "t1", AccountID: "a1"}))
	counts := func() (fixture.TenantCounts, fixture.RegionCounts, fixture.RegionCounts) {
		tc, _ := st.Accounts.Tenant(ctx, fixture.TenantCountsKey{TenantID: "t1"})
		north, _ := st.Accounts.Region(ctx, fixture.RegionCountsKey{Region: "north"})
		south, _ := st.Accounts.Region(ctx, fixture.RegionCountsKey{Region: "south"})
		return tc, north, south
	}

	reads, txs := n.Reads(), n.TransactWriteItems.Load()
	must(t, st.Accounts.HandOver(ctx, fixture.AccountKey{TenantID: "t1", AccountID: "a1"}, fixture.AccountHandOver{SuccessorID: "a2"}))
	if r, x := n.Reads()-reads, n.TransactWriteItems.Load()-txs; r != 2 || x != 1 {
		t.Fatalf("HandOver made %d reads and %d transactions, want 2 (the account and its successor) and 1", r, x)
	}
	tc, north, south := counts()
	if tc != (fixture.TenantCounts{Active: 1, Closed: 1}) || north.Active != 1 || south.Active != 0 {
		t.Fatalf("after handing a1 to a2: tenant %+v, north %+v, south %+v", tc, north, south)
	}

	// The successor must be pending: a closed one refuses, and nothing changes.
	wantErr(t, st.Accounts.HandOver(ctx, fixture.AccountKey{TenantID: "t1", AccountID: "a2"}, fixture.AccountHandOver{SuccessorID: "a1"}),
		fixture.ErrAccountHandOverRequiresAccount)
	if a2, _ := st.Accounts.Get(ctx, fixture.AccountKey{TenantID: "t1", AccountID: "a2"}); a2.Status != fixture.AccountStatusActive {
		t.Fatalf("a refused hand-over changed a2: %+v", a2)
	}

	// Across regions: the tenant item is shared (merged), the region items are separate.
	must(t, st.Accounts.HandOver(ctx, fixture.AccountKey{TenantID: "t1", AccountID: "a2"}, fixture.AccountHandOver{SuccessorID: "a3"}))
	tc, north, south = counts()
	if tc != (fixture.TenantCounts{Active: 1, Closed: 2}) || north.Active != 0 || south.Active != 1 {
		t.Fatalf("after handing a2 to a3: tenant %+v, north %+v, south %+v", tc, north, south)
	}
}
