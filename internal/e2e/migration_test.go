package e2e_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guregu/dynamo/v2"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/examples/toollibrary"
	"github.com/nicklanng/dynago/internal/testdb"
)

// generations sets up a migration: the previous generation's table (written with the current
// store, which stores the same shapes apart from the tenant list's index keys, which generation 2
// items lack) and the current generation's empty table.
type generations struct {
	db       *dynamo.DB
	base     string
	old, new *toollibrary.Store
	oldTable dynamo.Table
	out      bytes.Buffer
}

func setupGenerations(t *testing.T) *generations {
	t.Helper()
	db := testdb.DB(t)
	g := &generations{db: db, base: testdb.UniqueName(t, "migrate")}
	testdb.TableNamed(t, db, g.base+"-g2", toollibrary.TableSpec)
	testdb.TableNamed(t, db, toollibrary.TableName(g.base), toollibrary.TableSpec)
	g.old = toollibrary.New(db, g.base+"-g2")
	g.new = toollibrary.New(db, toollibrary.TableName(g.base))
	g.oldTable = db.Table(g.base + "-g2")
	return g
}

func (g *generations) run(command string) error {
	m := toollibrary.NewMigration(g.db, g.base)
	m.Workers, m.Out = 3, &g.out
	return m.Run(ctx, command)
}

// sameCounts checks the new table's counters match the old table's: derived items are rebuilt
// from the entities, not copied.
func (g *generations) sameCounts(t *testing.T) {
	t.Helper()
	lib := toollibrary.MemberCountsKey{LibraryID: "lib1"}
	om, _ := g.old.Libraries.MemberStats(ctx, lib)
	nm, _ := g.new.Libraries.MemberStats(ctx, lib)
	ot, _ := g.old.Libraries.ToolStats(ctx, toollibrary.ToolCountsKey{LibraryID: "lib1"})
	nt, _ := g.new.Libraries.ToolStats(ctx, toollibrary.ToolCountsKey{LibraryID: "lib1"})
	oa, _ := g.old.Loans.ActiveLoans(ctx, toollibrary.MemberLoansKey{LibraryID: "lib1", MemberID: "alice"})
	na, _ := g.new.Loans.ActiveLoans(ctx, toollibrary.MemberLoansKey{LibraryID: "lib1", MemberID: "alice"})
	oTot, _ := g.old.Loans.Totals(ctx, toollibrary.LoanTotalsKey{LibraryID: "lib1"})
	nTot, _ := g.new.Loans.Totals(ctx, toollibrary.LoanTotalsKey{LibraryID: "lib1"})
	if om != nm || ot != nt || oa != na || oTot != nTot {
		t.Fatalf("counters differ: members %+v vs %+v, tools %+v vs %+v, alice's loans %+v vs %+v, totals %+v vs %+v", om, nm, ot, nt, oa, na, oTot, nTot)
	}
}

func TestMigrationCopiesAndCatchesUp(t *testing.T) {
	g := setupGenerations(t)
	old := g.old
	must(t, old.Libraries.Open(ctx, &toollibrary.Library{LibraryID: "lib1", Name: "Greenwood", Slug: "greenwood", OpenedAt: time.Now()}))
	// Generation 2 allowed a library without a name; MigrateLibrary names it after its slug.
	must(t, g.oldTable.Put(map[string]any{"PK": "LIB#lib2", "SK": "LIBRARY", "_t": "Library", "_v": 1, "_rev": 1,
		"libraryId": "lib2", "slug": "oak"}).Run(ctx))
	for id, role := range map[string]toollibrary.MemberRole{"alice": toollibrary.MemberRoleSteward, "bob": toollibrary.MemberRoleSteward} {
		must(t, old.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: id, Email: id + "@example.org", Name: id,
			Role: role, Status: toollibrary.MemberStatusActive, MaxLoans: 3, JoinedAt: time.Now()}))
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		must(t, old.Tools.Add(ctx, &toollibrary.Tool{LibraryID: "lib1", ToolID: id, Name: "Tool " + id, Category: toollibrary.ToolCategoryHand,
			Status: toollibrary.ToolStatusAvailable, SerialNumber: "SN-" + id}))
	}
	must(t, old.Loans.Borrow(ctx, &toollibrary.Loan{LibraryID: "lib1", ToolID: "t1", LoanID: "l1", MemberID: "alice", ToolName: "Tool t1",
		BorrowedAt: time.Now(), DueAt: time.Now().Add(72 * time.Hour)}, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(3)}))
	must(t, old.Holds.Place(ctx, &toollibrary.Hold{LibraryID: "lib1", ToolID: "t2", MemberID: "bob", CodeHash: pickupCode("K3J"),
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(48 * time.Hour)}))

	// A conversion of our own: it labels each tool with its serial number.
	toollibrary.MigrateTool = func(o toollibrary.ToolG2) (toollibrary.Tool, error) {
		tool := toollibrary.AutoMigrateTool(o)
		if o.SerialNumber != "" {
			tool.Barcodes = []string{"LBL-" + o.SerialNumber}
		}
		return tool, nil
	}
	defer func() { toollibrary.MigrateTool = nil }()

	// The bulk copy, while the old generation serves.
	must(t, g.run("copy"))
	nu := g.new
	if l, err := nu.Libraries.GetBySlug(ctx, "greenwood"); err != nil || l.LibraryID != "lib1" {
		t.Fatalf("slug claim in the new table: %+v %v", l, err)
	}
	if m, err := nu.Members.GetByEmail(ctx, "lib1", "alice@example.org"); err != nil || m.MemberID != "alice" {
		t.Fatalf("email claim in the new table: %+v %v", m, err)
	}
	if tool, err := nu.Tools.GetByBarcode(ctx, "lib1", "LBL-SN-t2"); err != nil || tool.ToolID != "t2" {
		t.Fatalf("barcode from the conversion: %+v %v", tool, err)
	}
	// Every library is in the new generation's tenant list, the unnamed one under its slug.
	testdb.Eventually(t, "tenant list", func() error {
		libs, _, err := nu.Libraries.List(ctx, toollibrary.LibraryListQuery{}, dynago.Page{})
		if err != nil {
			return err
		}
		return testdb.Check(len(libs) == 2 && libs[0].Name == "Greenwood" && libs[1].Name == "oak", "libraries %+v", libs)
	})
	mine, _, err := nu.Loans.MyLoans(ctx, toollibrary.LoanMyLoansQuery{LibraryID: "lib1", MemberID: "alice"}, dynago.Page{})
	if err != nil || len(mine) != 1 {
		t.Fatalf("copy index in the new table: %+v %v", mine, err)
	}
	if _, err := nu.Holds.Get(ctx, toollibrary.HoldKey{LibraryID: "lib1", ToolID: "t2"}); err != nil {
		t.Fatalf("hold: %v", err)
	}
	g.sameCounts(t)
	if c, _ := nu.Libraries.ToolStats(ctx, toollibrary.ToolCountsKey{LibraryID: "lib1"}); c != (toollibrary.ToolCounts{Available: 2, OnLoan: 1}) {
		t.Fatalf("tool counts rebuilt as %+v", c)
	}
	// Copies keep their source's timestamps: moving tables doesn't change an item.
	oldT3, err := old.Tools.Get(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t3"})
	must(t, err)
	newT3, err := nu.Tools.Get(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t3"})
	must(t, err)
	if oldT3.Timestamps().Created.IsZero() || newT3.Timestamps() != oldT3.Timestamps() {
		t.Fatalf("migrated timestamps %+v, source %+v", newT3.Timestamps(), oldT3.Timestamps())
	}
	// Each copy records where it came from.
	if it := testdb.RawItem(t, g.db.Table(toollibrary.TableName(g.base)), "LIB#lib1#TOOL#t1", "TOOL"); it["_msrcPK"] != "LIB#lib1#TOOL#t1" {
		t.Fatalf("copied tool = %v", it)
	}

	// The old generation keeps serving: changes, a new tool, a new member, a deletion.
	must(t, old.Loans.Return(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t1", LoanID: "l1"}, toollibrary.LoanReturn{ReturnedAt: time.Now()}))
	must(t, old.Members.Suspend(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "bob"}))
	must(t, old.Tools.Add(ctx, &toollibrary.Tool{LibraryID: "lib1", ToolID: "t4", Name: "Tool t4", Category: toollibrary.ToolCategoryGarden, Status: toollibrary.ToolStatusAvailable}))
	must(t, old.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "carol", Email: "carol@example.org", Status: toollibrary.MemberStatusActive}))
	must(t, old.Holds.Release(ctx, toollibrary.HoldKey{LibraryID: "lib1", ToolID: "t2"}))

	// Catching up copies what changed and removes what's gone.
	must(t, g.run("copy"))
	if tool, _ := nu.Tools.Get(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t1"}); tool.Status != toollibrary.ToolStatusAvailable {
		t.Fatalf("returned tool in the new table: %+v", tool)
	}
	if _, err := nu.Holds.Get(ctx, toollibrary.HoldKey{LibraryID: "lib1", ToolID: "t2"}); !errors.Is(err, toollibrary.ErrHoldNotFound) {
		t.Fatalf("released hold still in the new table: %v", err)
	}
	if m, err := nu.Members.GetByEmail(ctx, "lib1", "carol@example.org"); err != nil || m.MemberID != "carol" {
		t.Fatalf("new member: %+v %v", m, err)
	}
	mine, _, _ = nu.Loans.MyLoans(ctx, toollibrary.LoanMyLoansQuery{LibraryID: "lib1", MemberID: "alice"}, dynago.Page{})
	if len(mine) != 0 {
		t.Fatalf("returned loan still listed: %+v", mine)
	}
	g.sameCounts(t)
	if c, _ := nu.Libraries.MemberStats(ctx, toollibrary.MemberCountsKey{LibraryID: "lib1"}); c != (toollibrary.MemberCounts{Active: 2, Suspended: 1, Stewards: 1}) {
		t.Fatalf("member counts after catching up: %+v", c)
	}

	// Carol leaves and Dave joins with her email. Whichever order the last pass reaches them in,
	// Dave's copy must not conflict with Carol's: conflicts are retried after removals.
	must(t, old.Members.Leave(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "carol"}))
	must(t, old.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "dave", Email: "carol@example.org", Status: toollibrary.MemberStatusActive}))

	// Writes stop; finish makes the last pass. Every new pod can run it: one does the work.
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := toollibrary.NewMigration(g.db, g.base)
			m.Workers, m.Out = 3, &bytes.Buffer{}
			errs[i] = m.Run(ctx, "finish")
		}(i)
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	g.out.Reset()
	must(t, g.run("copy"))
	if !strings.Contains(g.out.String(), "is finished") {
		t.Fatalf("copy after finish: %s", g.out.String())
	}
	if m, err := nu.Members.GetByEmail(ctx, "lib1", "carol@example.org"); err != nil || m.MemberID != "dave" {
		t.Fatalf("reused email after finish: %+v %v", m, err)
	}
	// The new generation serves.
	must(t, nu.Loans.Borrow(ctx, &toollibrary.Loan{LibraryID: "lib1", ToolID: "t4", LoanID: "l2", MemberID: "dave", BorrowedAt: time.Now(),
		DueAt: time.Now().Add(time.Hour)}, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(1)}))

	// The command line, as the generated job runs it.
	var out bytes.Buffer
	must(t, toollibrary.RunMigration(ctx, g.db, []string{"-table", g.base, "status"}, &out))
	if !strings.Contains(out.String(), "finished") {
		t.Fatalf("status: %s", out.String())
	}
}

// Two old members with one email (possible if the old generation didn't enforce it) can't both
// be copied: the job reports the conflict and fails, so a rollout gated on it waits. Once the data
// is fixed in the old table, the job carries on.
func TestMigrationConflictsGateTheRollout(t *testing.T) {
	g := setupGenerations(t)
	must(t, g.old.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "alice", Email: "alice@example.org", Status: toollibrary.MemberStatusActive}))
	// A duplicate written without its claim, as a generation without the rule would have.
	must(t, g.oldTable.Put(map[string]any{"PK": "LIB#lib1", "SK": "MEMBER#mallory", "_t": "Member", "_v": 1, "_rev": 7,
		"libraryId": "lib1", "memberId": "mallory", "email": "ALICE@example.org", "status": "active"}).Run(ctx))

	err := g.run("copy")
	if !errors.Is(err, dynago.ErrMigrationConflict) || strings.Count(g.out.String(), "conflict:") != 1 {
		t.Fatalf("got %v\n%s", err, g.out.String())
	}
	// Whichever of the two was copied first keeps the email; fix the data by removing mallory.
	must(t, g.oldTable.Delete("PK", "LIB#lib1").Range("SK", "MEMBER#mallory").Run(ctx))
	g.out.Reset()
	must(t, g.run("copy"))
	if m, err := g.new.Members.GetByEmail(ctx, "lib1", "alice@example.org"); err != nil || m.MemberID != "alice" {
		t.Fatalf("after the fix: %+v %v\n%s", m, err, g.out.String())
	}
}
