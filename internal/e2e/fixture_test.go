package e2e_test

import (
	"errors"
	"fmt"
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

// A parcel's index, counter values and preconditions exclude a state (not) or list several (in).
// Each holds for exactly the states it says: in the copies and counts every write maintains, in
// the conditions a single UpdateItem carries, and in what another entity's write requires.
func TestPredicatesNotAndIn(t *testing.T) {
	t.Parallel()
	db, n := testdb.CountingDB(t)
	st := fixture.New(db, testdb.Table(t, db, fixture.TableSpec))
	key := func(id string) fixture.ParcelKey { return fixture.ParcelKey{DepotID: "d1", ParcelID: id} }
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		must(t, st.Parcels.Receive(ctx, &fixture.Parcel{DepotID: "d1", ParcelID: id}))
	}
	check := func(when string, onSite, known int64, ids ...string) {
		t.Helper()
		c, err := st.Parcels.Counts(ctx, fixture.DepotParcelsKey{DepotID: "d1"})
		must(t, err)
		if c != (fixture.DepotParcels{OnSite: onSite, Known: known}) {
			t.Fatalf("%s: counts %+v, want %d on site and %d known", when, c, onSite, known)
		}
		list, _, err := st.Parcels.OnSite(ctx, fixture.ParcelOnSiteQuery{DepotID: "d1"}, dynago.Page{})
		must(t, err)
		var got []string
		for _, p := range list {
			got = append(got, p.ParcelID)
		}
		if fmt.Sprint(got) != fmt.Sprint(ids) {
			t.Fatalf("%s: on site %v, want %v", when, got, ids)
		}
	}
	check("received", 5, 5, "p1", "p2", "p3", "p4", "p5")

	// in: sending out takes a received or shelved parcel, and no other.
	must(t, st.Parcels.SendOut(ctx, key("p1")))
	wantErr(t, st.Parcels.SendOut(ctx, key("p1")), fixture.ErrParcelSendOutPrecondition)
	must(t, st.Parcels.Shelve(ctx, key("p2")))
	must(t, st.Parcels.SendOut(ctx, key("p2")))
	check("two sent out", 3, 5, "p3", "p4", "p5")

	// not: anything but a lost parcel can be lost, once.
	must(t, st.Parcels.Lose(ctx, key("p1")))
	must(t, st.Parcels.Lose(ctx, key("p3")))
	wantErr(t, st.Parcels.Lose(ctx, key("p3")), fixture.ErrParcelLosePrecondition)
	check("two lost", 2, 3, "p4", "p5")
	testdb.Eventually(t, "the index of parcels not lost", func() error {
		list, _, err := st.Parcels.Known(ctx, fixture.ParcelKnownQuery{DepotID: "d1"}, dynago.Page{})
		if err != nil {
			return err
		}
		var got []string
		for _, p := range list {
			got = append(got, p.ParcelID)
		}
		return testdb.Check(fmt.Sprint(got) == "[p2 p4 p5]", "known parcels %v, want p2, p4 and p5", got)
	})

	// The same condition on a single UpdateItem, with no read.
	reads := n.Reads()
	must(t, st.Parcels.Annotate(ctx, key("p4"), fixture.ParcelAnnotate{Note: "fragile"}))
	if r := n.Reads() - reads; r != 0 {
		t.Fatalf("Annotate made %d reads, want none", r)
	}
	wantErr(t, st.Parcels.Annotate(ctx, key("p3"), fixture.ParcelAnnotate{Note: "found?"}), fixture.ErrParcelAnnotatePrecondition)

	// Another entity's write requires the parcel to be in one of several states, or not in one.
	file := func(parcel string) error {
		return st.Damages.File(ctx, &fixture.Damage{DepotID: "d1", ParcelID: parcel, DamageID: "dmg1", Detail: "dented"})
	}
	wantErr(t, file("p2"), fixture.ErrDamageFileRequiresParcel) // out
	must(t, file("p4"))
	must(t, file("p5"))
	dispute := func(parcel string) error {
		return st.Damages.Dispute(ctx, fixture.DamageKey{DepotID: "d1", ParcelID: parcel, DamageID: "dmg1"}, fixture.DamageDispute{Detail: "was dented on arrival"})
	}
	must(t, dispute("p4"))
	if p, err := st.Parcels.Get(ctx, key("p4")); err != nil || p.Note != "disputed" {
		t.Fatalf("a dispute should mark its parcel: %+v, %v", p, err)
	}
	must(t, st.Parcels.Lose(ctx, key("p5")))
	wantErr(t, dispute("p5"), fixture.ErrDamageDisputeRequiresParcel)
	if d, err := st.Damages.Get(ctx, fixture.DamageKey{DepotID: "d1", ParcelID: "p5", DamageID: "dmg1"}); err != nil || d.Detail != "dented" {
		t.Fatalf("a refused dispute changed the report: %+v, %v", d, err)
	}
}

// A requirement can add to a number on its item and set a field only when the writer has a value
// for it. Filing a damage report counts on the parcel, which is read first because the count is
// listed in a copy; disputing one changes nothing derived, so its addition is an atomic ADD on an
// update made without reading the parcel. Either way, concurrent writes all count.
func TestRequiresAddAndPatch(t *testing.T) {
	t.Parallel()
	db, n := testdb.CountingDB(t)
	st := fixture.New(db, testdb.Table(t, db, fixture.TableSpec))
	pk := fixture.ParcelKey{DepotID: "d1", ParcelID: "p1"}
	must(t, st.Parcels.Receive(ctx, &fixture.Parcel{DepotID: "d1", ParcelID: "p1"}))
	parcel := func() *fixture.Parcel {
		t.Helper()
		p, err := st.Parcels.Get(ctx, pk)
		must(t, err)
		return p
	}
	file := func(id string, fragile bool) error {
		return st.Damages.File(ctx, &fixture.Damage{DepotID: "d1", ParcelID: "p1", DamageID: id, Detail: "dented", Fragile: fragile})
	}

	must(t, file("a", false))
	if p := parcel(); p.Damages != 1 || p.Fragile {
		t.Fatalf("after one report: %+v", p)
	}
	must(t, file("b", true))
	must(t, file("c", false)) // says nothing about fragility: the mark stays
	if p := parcel(); p.Damages != 3 || !p.Fragile {
		t.Fatalf("after three reports, one of a fragile parcel: %+v", p)
	}
	// The copy that lists the count is kept in the same transaction.
	list, _, err := st.Parcels.OnSite(ctx, fixture.ParcelOnSiteQuery{DepotID: "d1"}, dynago.Page{})
	must(t, err)
	if len(list) != 1 || list[0].Damages != 3 {
		t.Fatalf("the on-site list shows %+v, want one parcel with 3 damages", list)
	}

	dispute := func(id, detail string) error {
		return st.Damages.Dispute(ctx, fixture.DamageKey{DepotID: "d1", ParcelID: "p1", DamageID: id}, fixture.DamageDispute{Detail: detail})
	}
	reads := n.Reads()
	must(t, dispute("a", "was dented on arrival"))
	if r := n.Reads() - reads; r != 0 {
		t.Fatalf("a dispute made %d reads, want none: its changes are known from the call", r)
	}
	must(t, dispute("b", "")) // no detail: the last one given stays
	if p := parcel(); p.Disputes != 2 || p.LastDispute != "was dented on arrival" || p.Note != "disputed" || p.Damages != 3 {
		t.Fatalf("after two disputes: %+v", p)
	}

	// Concurrent writes: every addition counts, whichever path makes it.
	const each = 6
	errs := make(chan error, 2*each)
	for i := range each {
		go func() { errs <- file(fmt.Sprint("x", i), false) }()
		go func() { errs <- dispute("c", fmt.Sprint("again ", i)) }()
	}
	for range 2 * each {
		must(t, <-errs)
	}
	if p := parcel(); p.Damages != 3+each || p.Disputes != 2+each {
		t.Fatalf("after %d concurrent reports and disputes: %d damages and %d disputes, want %d and %d", each, p.Damages, p.Disputes, 3+each, 2+each)
	}
}

// Several items are read by key in one BatchGetItem: those that exist, in the order asked for.
// And every item of a counter in a partition is read with one Query, each with the key its sort
// key holds, a page at a time.
func TestBatchGetAndAllCounters(t *testing.T) {
	t.Parallel()
	db, n := testdb.CountingDB(t)
	st := fixture.New(db, testdb.Table(t, db, fixture.TableSpec))
	key := func(id string) fixture.ParcelKey { return fixture.ParcelKey{DepotID: "d1", ParcelID: id} }
	for _, id := range []string{"p1", "p2", "p3", "p4"} {
		must(t, st.Parcels.Receive(ctx, &fixture.Parcel{DepotID: "d1", ParcelID: id, Note: "note " + id}))
	}
	must(t, st.Parcels.Receive(ctx, &fixture.Parcel{DepotID: "d2", ParcelID: "p1"}))

	reads, batches := n.Reads(), n.BatchGetItem.Load()
	got, err := st.Parcels.GetSeveral(ctx, []fixture.ParcelKey{key("p3"), key("nope"), key("p1"), key("p3"), key("p4")})
	must(t, err)
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ParcelID+"="+p.Note)
	}
	if fmt.Sprint(ids) != "[p3=note p3 p1=note p1 p4=note p4]" {
		t.Fatalf("GetSeveral returned %v; want p3, p1 and p4, as asked, without the missing key or the repeat", ids)
	}
	if r, b := n.Reads()-reads, n.BatchGetItem.Load()-batches; r != 1 || b != 1 {
		t.Fatalf("GetSeveral made %d reads, %d of them BatchGetItem; want one BatchGetItem", r, b)
	}
	// What it returns can be written back like any entity read from the store.
	if got[0].Version() == "" {
		t.Fatalf("an entity from GetSeveral has no version")
	}
	must(t, st.Parcels.Shelve(ctx, key("p3"), dynago.From(&got[0])))
	if none, err := st.Parcels.GetSeveral(ctx, nil); err != nil || len(none) != 0 {
		t.Fatalf("no keys: %v, %v", none, err)
	}
	if _, err := st.Parcels.GetSeveral(ctx, []fixture.ParcelKey{{DepotID: "d1"}}); !errors.Is(err, dynago.ErrInvalidKey) {
		t.Fatalf("a key without a parcel id: %v", err)
	}

	// One counter item per state, read together. A state every parcel has left reads as zero.
	must(t, st.Parcels.SendOut(ctx, key("p3")))
	must(t, st.Parcels.Lose(ctx, key("p4")))
	queries := n.Query.Load()
	states, next, err := st.Parcels.States(ctx, fixture.ParcelStatesQuery{DepotID: "d1"}, dynago.Page{})
	must(t, err)
	byState := map[fixture.ParcelState]int64{}
	for _, s := range states {
		if s.Key.DepotID != "d1" {
			t.Fatalf("a counter of another depot: %+v", s)
		}
		byState[s.Key.State] = s.Parcels
	}
	want := map[fixture.ParcelState]int64{fixture.ParcelStateReceived: 2, fixture.ParcelStateShelved: 0, fixture.ParcelStateOut: 1, fixture.ParcelStateLost: 1}
	if next != "" || fmt.Sprint(byState) != fmt.Sprint(want) {
		t.Fatalf("states %v (next %q), want %v", byState, next, want)
	}
	if q := n.Query.Load() - queries; q != 1 {
		t.Fatalf("States made %d queries, want 1", q)
	}

	// Paged: three parcels with damage reports, two counter items to a page.
	for _, id := range []string{"p1", "p1", "p2"} {
		must(t, st.Damages.File(ctx, &fixture.Damage{DepotID: "d1", ParcelID: id, DamageID: fmt.Sprint("x", len(ids)), Detail: "dented"}))
		ids = append(ids, id)
	}
	must(t, st.Damages.File(ctx, &fixture.Damage{DepotID: "d2", ParcelID: "p1", DamageID: "y", Detail: "dented"}))
	var seen []string
	page := dynago.Page{}
	for calls := 0; ; calls++ {
		if calls > 5 {
			t.Fatal("the counters never ended")
		}
		entries, next, err := st.Damages.Damaged(ctx, fixture.DamageDamagedQuery{DepotID: "d1"}, page)
		must(t, err)
		if len(entries) > 2 {
			t.Fatalf("a page of %d counters, over its size", len(entries))
		}
		for _, e := range entries {
			seen = append(seen, fmt.Sprint(e.Key.ParcelID, ":", e.Reports))
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if fmt.Sprint(seen) != "[p1:2 p2:1]" {
		t.Fatalf("damaged parcels %v, want p1 with 2 reports and p2 with 1", seen)
	}
}

// A depot, its parcels and their damage reports share a partition, and one Query reads them
// together: each page's items sorted by kind, the counters that live there too left out, and the
// partition's one depot wherever its sort key falls.
func TestPartitionRead(t *testing.T) {
	t.Parallel()
	db, n := testdb.CountingDB(t)
	st := fixture.New(db, testdb.Table(t, db, fixture.TableSpec))
	for _, depot := range []string{"d1", "d2"} {
		must(t, st.Depots.Open(ctx, &fixture.Depot{DepotID: depot, Name: "Depot " + depot}))
		for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
			must(t, st.Parcels.Receive(ctx, &fixture.Parcel{DepotID: depot, ParcelID: id}))
		}
		for _, id := range []string{"p1", "p2", "p2"} {
			must(t, st.Damages.File(ctx, &fixture.Damage{DepotID: depot, ParcelID: id, DamageID: testdb.UniqueName(t, "dmg"), Detail: "dented"}))
		}
	}

	var depots, parcels, damages []string
	queries := n.Query.Load()
	page := dynago.Page{}
	for calls := 0; ; calls++ {
		if calls > 20 {
			t.Fatal("the partition never ended")
		}
		got, next, err := st.Depots.Everything(ctx, fixture.DepotEverythingQuery{DepotID: "d1"}, page)
		must(t, err)
		if n := len(got.Parcels) + len(got.Damages); n > 4 {
			t.Fatalf("a page of %d items, over its size", n)
		}
		if got.Depot != nil {
			depots = append(depots, got.Depot.DepotID+" "+got.Depot.Name)
		}
		for _, p := range got.Parcels {
			parcels = append(parcels, p.DepotID+"/"+p.ParcelID)
		}
		for _, d := range got.Damages {
			damages = append(damages, d.DepotID+"/"+d.ParcelID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if fmt.Sprint(depots) != "[d1 Depot d1]" || fmt.Sprint(parcels) != "[d1/p1 d1/p2 d1/p3 d1/p4 d1/p5]" || fmt.Sprint(damages) != "[d1/p1 d1/p2 d1/p2]" {
		t.Fatalf("read depots %v, parcels %v, damages %v", depots, parcels, damages)
	}
	// 9 items of these kinds and the partition's counter items, 4 evaluated to a page.
	if q := n.Query.Load() - queries; q < 3 {
		t.Fatalf("Everything took %d queries; pages of 4 can't hold the partition in fewer than 3", q)
	}

	// Another read of the same partition returns other kinds, newest sort key first, in one page.
	got, next, err := st.Depots.Newest(ctx, fixture.DepotNewestQuery{DepotID: "d2"}, dynago.Page{})
	must(t, err)
	var ids []string
	for _, p := range got.Parcels {
		ids = append(ids, p.ParcelID)
	}
	if next != "" || fmt.Sprint(ids) != "[p5 p4 p3 p2 p1]" || len(got.Damages) != 3 {
		t.Fatalf("Newest read parcels %v and %d damages (next %q)", ids, len(got.Damages), next)
	}
	// What it returns can be written back like any entity read from the store.
	must(t, st.Parcels.Shelve(ctx, fixture.ParcelKey{DepotID: "d2", ParcelID: "p5"}, dynago.From(&got.Parcels[0])))
	if _, _, err := st.Depots.Everything(ctx, fixture.DepotEverythingQuery{}, dynago.Page{}); !errors.Is(err, dynago.ErrInvalidKey) {
		t.Fatalf("no depot id: %v", err)
	}
}

// A requirement with ensure creates its item when there is none, in the write's transaction, with
// everything a create of that entity writes; one that is there is checked and changed as without
// it. A parcel arriving at an unopened depot opens it, whoever gets there first.
func TestRequiresEnsureCreatesItsTarget(t *testing.T) {
	db, n := testdb.CountingDB(t)
	st := fixture.New(db, testdb.Table(t, db, fixture.TableSpec))
	arrive := func(depot, id string) error {
		return st.Parcels.Arrive(ctx, &fixture.Parcel{DepotID: depot, ParcelID: id})
	}
	depot := func(id string) *fixture.Depot {
		t.Helper()
		d, err := st.Depots.Get(ctx, fixture.DepotKey{DepotID: id})
		must(t, err)
		return d
	}
	depots := func() int64 {
		t.Helper()
		c, err := st.Depots.Totals(ctx, fixture.DepotTotalsKey{})
		must(t, err)
		return c.Depots
	}

	// The first parcel creates the depot, with the fields ensure gives and the requirement's own
	// change applied, and counts it.
	must(t, arrive("dx", "p1"))
	if d := depot("dx"); d.Name != "unnamed" || d.Parcels != 1 || d.Version() == "" || d.Timestamps().Created.IsZero() {
		t.Fatalf("the depot the first parcel created: %+v", d)
	}
	if got := depots(); got != 1 {
		t.Fatalf("%d depots counted, want 1", got)
	}
	// The second finds it: the change alone, without reading it.
	reads := n.Reads()
	must(t, arrive("dx", "p2"))
	if r := n.Reads() - reads; r != 0 {
		t.Fatalf("arriving at an open depot made %d reads, want none", r)
	}
	if d := depot("dx"); d.Name != "unnamed" || d.Parcels != 2 || depots() != 1 {
		t.Fatalf("after a second parcel: %+v, %d depots", d, depots())
	}

	// when still applies to a depot that is there: a closed one refuses, and nothing is written.
	must(t, st.Depots.Open(ctx, &fixture.Depot{DepotID: "dy", Name: "Yard"}))
	must(t, st.Depots.Close(ctx, fixture.DepotKey{DepotID: "dy"}))
	wantErr(t, arrive("dy", "p1"), fixture.ErrParcelArriveRequiresDepot)
	if _, err := st.Parcels.Get(ctx, fixture.ParcelKey{DepotID: "dy", ParcelID: "p1"}); !errors.Is(err, fixture.ErrParcelNotFound) {
		t.Fatalf("a refused arrival left a parcel: %v", err)
	}
	if d := depot("dy"); d.Name != "Yard" || d.Parcels != 0 {
		t.Fatalf("a refused arrival changed the depot: %+v", d)
	}

	// ensure alone: created if absent, otherwise left as it is.
	must(t, st.Parcels.Leave(ctx, &fixture.Parcel{DepotID: "dz", ParcelID: "p1"}))
	must(t, st.Parcels.Leave(ctx, &fixture.Parcel{DepotID: "dz", ParcelID: "p2"}))
	must(t, st.Parcels.Leave(ctx, &fixture.Parcel{DepotID: "dy", ParcelID: "p9"})) // closed, but nothing is required of it
	if d := depot("dz"); d.Name != "" || d.Parcels != 0 || depots() != 3 {
		t.Fatalf("after parcels left two depots: %+v, %d depots", d, depots())
	}

	// Many parcels arriving at once at a depot nobody has opened: one of them creates it, the
	// others find it, and every one is counted.
	const each = 8
	errs := make(chan error, each)
	for i := range each {
		go func() { errs <- arrive("dc", fmt.Sprint("p", i)) }()
	}
	for range each {
		must(t, <-errs)
	}
	if d := depot("dc"); d.Parcels != each || depots() != 4 {
		t.Fatalf("after %d parcels arrived at once: %d counted on the depot, %d depots; want %d and 4", each, d.Parcels, depots(), each)
	}
}
