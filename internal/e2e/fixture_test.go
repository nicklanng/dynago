package e2e_test

import (
	"sort"
	"testing"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/internal/e2e/fixture"
	"github.com/nicklanng/dynago/internal/testdb"
)

// HandOver closes an account and activates its successor in one transaction. Both feed the
// tenant's counter item, so their changes must be merged into one update (DynamoDB refuses two
// operations on one item), and the successor is read first, because its region counter is keyed
// by a field the requirement doesn't pin.
func TestHandOverChangesAnotherItemOfTheSameEntity(t *testing.T) {
	t.Parallel()
	db, n := testdb.CountingDB(t)
	st := fixture.New(db, testdb.Table(t, db, fixture.TableSpec))
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

// A declared scan pages through the whole table and returns only the entity's items: the counter
// items it also reads are left out, so pages can be short.
func TestScanReturnsOnlyTheEntity(t *testing.T) {
	t.Parallel()
	db := testdb.DB(t)
	st := fixture.New(db, testdb.Table(t, db, fixture.TableSpec))
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		must(t, st.Accounts.Open(ctx, &fixture.Account{TenantID: "t" + id[1:], AccountID: id, Region: "north", Status: fixture.AccountStatusActive}))
	}
	var ids []string
	page := dynago.Page{}
	for calls := 0; ; calls++ {
		if calls > 20 {
			t.Fatal("the scan never ended")
		}
		accounts, next, err := st.Accounts.Export(ctx, page)
		must(t, err)
		if len(accounts) > 2 {
			t.Fatalf("a page of %d accounts, over its size", len(accounts))
		}
		for _, a := range accounts {
			ids = append(ids, a.AccountID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	sort.Strings(ids)
	if len(ids) != 5 || ids[0] != "a1" || ids[4] != "a5" {
		t.Fatalf("scanned %v", ids)
	}
	// A cursor from another read is refused.
	if _, _, err := st.Accounts.Export(ctx, dynago.Page{Cursor: "bm9wZQ"}); err == nil {
		t.Fatal("a forged cursor was accepted")
	}
}
