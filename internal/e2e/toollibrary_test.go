// Package e2e tests dynago end to end: the code generated for the example schema, run against
// DynamoDB Local. The example itself carries no tests, since its store is generated.
package e2e_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guregu/dynamo/v2"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/dynagotest"
	"github.com/nicklanng/dynago/examples/toollibrary"
)

var ctx = context.Background()

type env struct {
	st      *toollibrary.Store
	counter *dynagotest.Requests
	table   dynamo.Table
}

func setup(t *testing.T) env {
	t.Helper()
	db, reads := dynagotest.CountingDB(t)
	name := dynagotest.Table(t, db, toollibrary.TableSpec)
	return env{st: toollibrary.New(db, name), counter: reads, table: db.Table(name)}
}

// raw reads an item as stored.
func (e env) raw(t *testing.T, pk, sk string) map[string]any {
	t.Helper()
	var item map[string]any
	if err := e.table.Get("PK", pk).Range("SK", dynamo.Equal, sk).Consistent(true).One(ctx, &item); err != nil {
		return nil
	}
	return item
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want %v", err, target)
	}
}

func (e env) join(t *testing.T, id string, role toollibrary.MemberRole, maxLoans int64) {
	t.Helper()
	must(t, e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: id, Email: id + "@example.org",
		Name: "Member " + id, Role: role, Status: toollibrary.MemberStatusActive, MaxLoans: maxLoans, JoinedAt: time.Now()}))
}

func (e env) addTool(t *testing.T, id, name string, cat toollibrary.ToolCategory) {
	t.Helper()
	must(t, e.st.Tools.Add(ctx, &toollibrary.Tool{LibraryID: "lib1", ToolID: id, Name: name, Category: cat,
		Status: toollibrary.ToolStatusAvailable, Manual: "Wear goggles.", AddedAt: time.Now()}))
}

func (e env) memberCounts(t *testing.T) toollibrary.MemberCounts {
	t.Helper()
	c, err := e.st.Libraries.MemberStats(ctx, toollibrary.MemberCountsKey{LibraryID: "lib1"})
	must(t, err)
	return c
}

func (e env) toolCounts(t *testing.T) toollibrary.ToolCounts {
	t.Helper()
	c, err := e.st.Libraries.ToolStats(ctx, toollibrary.ToolCountsKey{LibraryID: "lib1"})
	must(t, err)
	return c
}

func (e env) borrow(t *testing.T, loanID, toolID, memberID string, maxLoans int64) error {
	t.Helper()
	return e.st.Loans.Borrow(ctx, &toollibrary.Loan{LibraryID: "lib1", ToolID: toolID, LoanID: loanID, MemberID: memberID,
		ToolName: "Drill", BorrowedAt: time.Now(), DueAt: time.Now().Add(7 * 24 * time.Hour)},
		toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(maxLoans)})
}

func (e env) toolStatus(t *testing.T, id string) toollibrary.ToolStatus {
	t.Helper()
	tool, err := e.st.Tools.Get(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: id})
	must(t, err)
	return tool.Status
}

// snapshot returns every item in the table, keyed by PK and SK.
func (e env) snapshot(t *testing.T) map[string]map[string]any {
	t.Helper()
	var items []map[string]any
	must(t, e.table.Scan().Consistent(true).All(ctx, &items))
	out := map[string]map[string]any{}
	for _, it := range items {
		out[fmt.Sprint(it["PK"], " | ", it["SK"])] = it
	}
	return out
}

// reads counts the read requests fn makes, of any kind.
func (e env) reads(fn func()) int64 {
	before := e.counter.Reads()
	fn()
	return e.counter.Reads() - before
}

func TestLibrarySlugAndVersionedEdits(t *testing.T) {
	e := setup(t)
	lib := &toollibrary.Library{LibraryID: "lib1", Name: "Greenwood Tool Library", Slug: "greenwood", OpenedAt: time.Now()}
	must(t, e.st.Libraries.Open(ctx, lib))
	// Open leaves the entity knowing its version: no read needed to hand out an ETag.
	if lib.Version() == "" {
		t.Fatal("no version after create")
	}
	wantErr(t, e.st.Libraries.Open(ctx, &toollibrary.Library{LibraryID: "lib2", Name: "Copy", Slug: "greenwood"}), toollibrary.ErrLibrarySlugTaken)

	got, err := e.st.Libraries.GetBySlug(ctx, "greenwood")
	must(t, err)
	if got.LibraryID != "lib1" || got.Version() != lib.Version() {
		t.Fatalf("by slug = %+v", got)
	}
	// Rename is a patch and requires a version.
	wantErr(t, e.st.Libraries.Rename(ctx, lib.Key(), toollibrary.LibraryRename{Name: dynago.Ptr("Greenwood")}), dynago.ErrVersionRequired)
	var newVersion string
	must(t, e.st.Libraries.Rename(ctx, lib.Key(), toollibrary.LibraryRename{Name: dynago.Ptr("Greenwood")},
		dynago.IfVersion(lib.Version()), dynago.ReturnVersion(&newVersion)))
	fresh, _ := e.st.Libraries.Get(ctx, lib.Key())
	if fresh.Name != "Greenwood" || newVersion == "" || newVersion != fresh.Version() {
		t.Fatalf("after rename: %+v, returned version %q", fresh, newVersion)
	}
	// The stale version (another steward's open form) is refused.
	wantErr(t, e.st.Libraries.Rename(ctx, lib.Key(), toollibrary.LibraryRename{Name: dynago.Ptr("Oops")}, dynago.From(lib)), dynago.ErrVersionMismatch)
	// A patch with nothing given writes nothing.
	must(t, e.st.Libraries.Rename(ctx, lib.Key(), toollibrary.LibraryRename{}, dynago.From(fresh)))
	// Moving the slug frees the old one.
	must(t, e.st.Libraries.ChangeSlug(ctx, lib.Key(), toollibrary.LibraryChangeSlug{Slug: "greenwood-tools"}, dynago.From(fresh)))
	wantErr(t, e.st.Libraries.Open(ctx, &toollibrary.Library{LibraryID: "lib2", Name: "New", Slug: "greenwood-tools"}), toollibrary.ErrLibrarySlugTaken)
	must(t, e.st.Libraries.Open(ctx, &toollibrary.Library{LibraryID: "lib2", Name: "New", Slug: "greenwood"}))
}

func TestMembers(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 2)
	e.join(t, "bob", toollibrary.MemberRoleMember, 2)
	if c := e.memberCounts(t); c != (toollibrary.MemberCounts{Active: 2, Stewards: 1}) {
		t.Fatalf("counts = %+v", c)
	}
	// Email is unique per library, not globally.
	wantErr(t, e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "carol", Email: "bob@example.org", Status: toollibrary.MemberStatusActive}), toollibrary.ErrMemberEmailTaken)
	must(t, e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib9", MemberID: "bob", Email: "bob@example.org", Role: toollibrary.MemberRoleSteward, Status: toollibrary.MemberStatusActive}))
	if m, err := e.st.Members.GetByEmail(ctx, "lib1", "bob@example.org"); err != nil || m.MemberID != "bob" {
		t.Fatalf("by email: %+v %v", m, err)
	}

	// Only active stewards count, so whether suspending someone changes the steward count depends
	// on their role, which Suspend doesn't pin: it reads the member first.
	alice, bob := toollibrary.MemberKey{LibraryID: "lib1", MemberID: "alice"}, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "bob"}
	if n := e.reads(func() { must(t, e.st.Members.Suspend(ctx, bob)) }); n != 1 {
		t.Fatalf("Suspend made %d reads, want 1", n)
	}
	if c := e.memberCounts(t); c != (toollibrary.MemberCounts{Active: 1, Suspended: 1, Stewards: 1}) {
		t.Fatalf("counts after suspend = %+v", c)
	}
	wantErr(t, e.st.Members.Suspend(ctx, bob), toollibrary.ErrMemberSuspendPrecondition)
	wantErr(t, e.st.Members.Suspend(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "nobody"}), toollibrary.ErrMemberNotFound)
	must(t, e.st.Members.Reinstate(ctx, bob))

	// The last active steward can't be suspended, step down or leave; with a second, they can.
	wantErr(t, e.st.Members.Suspend(ctx, alice), toollibrary.ErrMemberCountsStewardsMin)
	wantErr(t, e.st.Members.StepDown(ctx, alice), toollibrary.ErrMemberCountsStewardsMin)
	wantErr(t, e.st.Members.Leave(ctx, alice), toollibrary.ErrMemberCountsStewardsMin)
	must(t, e.st.Members.MakeSteward(ctx, bob))
	must(t, e.st.Members.Suspend(ctx, alice))
	if c := e.memberCounts(t); c != (toollibrary.MemberCounts{Active: 1, Suspended: 1, Stewards: 1}) {
		t.Fatalf("counts with a suspended steward = %+v", c)
	}
	// Bob is now the only active steward, even though alice is still a steward.
	wantErr(t, e.st.Members.StepDown(ctx, bob), toollibrary.ErrMemberCountsStewardsMin)
	must(t, e.st.Members.Reinstate(ctx, alice))
	must(t, e.st.Members.StepDown(ctx, alice))
	if c := e.memberCounts(t); c != (toollibrary.MemberCounts{Active: 2, Stewards: 1}) {
		t.Fatalf("counts after hand-over = %+v", c)
	}

	// A patch changes only what is given. Phone alone feeds nothing derived, so it is one
	// UpdateItem without a read; a new name moves the directory entry, so that call reads.
	updates, txs := e.counter.UpdateItem.Load(), e.counter.TransactWriteItems.Load()
	if n := e.reads(func() {
		must(t, e.st.Members.UpdateProfile(ctx, alice, toollibrary.MemberUpdateProfile{Phone: dynago.Ptr("555-0100")}))
	}); n != 0 {
		t.Fatalf("phone-only UpdateProfile made %d reads, want 0", n)
	}
	if u, x := e.counter.UpdateItem.Load()-updates, e.counter.TransactWriteItems.Load()-txs; u != 1 || x != 0 {
		t.Fatalf("phone-only UpdateProfile made %d UpdateItems and %d transactions, want one UpdateItem", u, x)
	}
	a, _ := e.st.Members.Get(ctx, alice)
	if a.Name != "Member alice" || a.Phone != "555-0100" {
		t.Fatalf("after phone patch: %+v", a)
	}
	if n := e.reads(func() {
		must(t, e.st.Members.UpdateProfile(ctx, alice, toollibrary.MemberUpdateProfile{Name: dynago.Ptr("Alice Aardvark")}))
	}); n != 1 {
		t.Fatalf("renaming UpdateProfile made %d reads, want 1", n)
	}
	dir, _, err := e.st.Members.Directory(ctx, toollibrary.MemberDirectoryQuery{LibraryID: "lib1"}, dynago.Page{})
	must(t, err)
	if len(dir) != 2 || dir[0].MemberID != "alice" || dir[1].MemberID != "bob" || dir[1].Role != toollibrary.MemberRoleSteward {
		t.Fatalf("directory = %+v", dir)
	}
	// Clearing a field is a pointer to its zero value.
	must(t, e.st.Members.UpdateProfile(ctx, alice, toollibrary.MemberUpdateProfile{Phone: dynago.Ptr("")}))
	if item := e.raw(t, "LIB#lib1", "MEMBER#alice"); item["phone"] != nil {
		t.Fatalf("phone not cleared: %v", item["phone"])
	}
}

func TestKeySeparators(t *testing.T) {
	e := setup(t)
	// libraryId is followed by "#" in keys that identify items: a value containing it could
	// render another library's keys.
	err := e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1#TOOL#t1", MemberID: "m1", Email: "e@example.org"})
	wantErr(t, err, dynago.ErrInvalidKey)
	_, err = e.st.Members.GetByEmail(ctx, "lib1#x", "e@example.org")
	wantErr(t, err, dynago.ErrInvalidKey)
	// Names only appear in GSI sort keys, which needn't be unique, and emails end their claim's
	// key: both may contain "#".
	must(t, e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "m1", Name: "Smith #2", Email: "odd#box@example.org",
		Role: toollibrary.MemberRoleSteward, Status: toollibrary.MemberStatusActive}))
	e.addTool(t, "t1", "Drill #2", toollibrary.ToolCategoryPower)
	// A tool id is last in the tool's own key, but not in a loan's MyLoans key: a "#" in it would
	// make the tool impossible to borrow, so the tool is refused when it is added.
	wantErr(t, e.st.Tools.Add(ctx, &toollibrary.Tool{LibraryID: "lib1", ToolID: "t#2", Name: "Saw", Category: toollibrary.ToolCategoryHand,
		Status: toollibrary.ToolStatusAvailable}), dynago.ErrInvalidKey)
	cat, _, err := e.st.Tools.Catalogue(ctx, toollibrary.ToolCatalogueQuery{LibraryID: "lib1", Category: toollibrary.ToolCategoryPower}, dynago.Page{})
	must(t, err)
	if len(cat) != 1 || cat[0].Name != "Drill #2" {
		t.Fatalf("catalogue = %+v", cat)
	}
}

func TestToolsCatalogueAndCounts(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 5)
	e.addTool(t, "t1", "Cordless drill", toollibrary.ToolCategoryPower)
	e.addTool(t, "t2", "Angle grinder", toollibrary.ToolCategoryPower)
	e.addTool(t, "t3", "Rake", toollibrary.ToolCategoryGarden)
	// Serial numbers are unique per library; tools without one make no claim.
	must(t, e.st.Tools.Add(ctx, &toollibrary.Tool{LibraryID: "lib1", ToolID: "t4", Name: "Saw", Category: toollibrary.ToolCategoryHand, Status: toollibrary.ToolStatusAvailable, SerialNumber: "SN-1"}))
	wantErr(t, e.st.Tools.Add(ctx, &toollibrary.Tool{LibraryID: "lib1", ToolID: "t5", Name: "Saw again", Category: toollibrary.ToolCategoryHand, Status: toollibrary.ToolStatusAvailable, SerialNumber: "SN-1"}), toollibrary.ErrToolSerialTaken)

	power, _, err := e.st.Tools.Catalogue(ctx, toollibrary.ToolCatalogueQuery{LibraryID: "lib1", Category: toollibrary.ToolCategoryPower}, dynago.Page{})
	must(t, err)
	if len(power) != 2 || power[0].Name != "Angle grinder" || power[1].Name != "Cordless drill" {
		t.Fatalf("catalogue = %+v", power)
	}

	// Retiring changes the counts by a known amount, so it runs without reading the tool.
	rake := toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t3"}
	if n := e.reads(func() { must(t, e.st.Tools.Retire(ctx, rake)) }); n != 0 {
		t.Fatalf("Retire made %d reads, want 0", n)
	}
	// When the assumed state doesn't hold, it falls back to reading, and reports why.
	wantErr(t, e.st.Tools.Retire(ctx, rake), toollibrary.ErrToolRetirePrecondition)
	wantErr(t, e.st.Tools.Retire(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "nope"}), toollibrary.ErrToolNotFound)

	// Status is projected into the catalogue, so lending updates the entry without moving it.
	must(t, e.borrow(t, "l1", "t1", "alice", 5))
	if c := e.toolCounts(t); c != (toollibrary.ToolCounts{Available: 2, OnLoan: 1, Retired: 1}) {
		t.Fatalf("tool counts = %+v", c)
	}
	power, _, _ = e.st.Tools.Catalogue(ctx, toollibrary.ToolCatalogueQuery{LibraryID: "lib1", Category: toollibrary.ToolCategoryPower}, dynago.Page{})
	if power[1].Status != toollibrary.ToolStatusOnLoan {
		t.Fatalf("catalogue status = %+v", power[1])
	}
	wantErr(t, e.st.Tools.Retire(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t1"}), toollibrary.ErrToolRetirePrecondition)

	// EditDetails is a versioned patch that renames (moving the catalogue entry).
	drill := toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t1"}
	tool, _ := e.st.Tools.Get(ctx, drill)
	must(t, e.st.Tools.EditDetails(ctx, drill, toollibrary.ToolEditDetails{Name: dynago.Ptr("Drill driver")}, dynago.From(tool)))
	wantErr(t, e.st.Tools.EditDetails(ctx, drill, toollibrary.ToolEditDetails{Tags: dynago.Ptr([]string{"18v"})}, dynago.From(tool)), dynago.ErrVersionMismatch)
	power, _, _ = e.st.Tools.Catalogue(ctx, toollibrary.ToolCatalogueQuery{LibraryID: "lib1", Category: toollibrary.ToolCategoryPower}, dynago.Page{})
	if len(power) != 2 || power[1].Name != "Drill driver" {
		t.Fatalf("catalogue after rename = %+v", power)
	}
	got, _ := e.st.Tools.Get(ctx, drill)
	if got.Manual != "Wear goggles." {
		t.Fatalf("patch cleared a field it wasn't given: %+v", got)
	}
}

func TestBorrowingRules(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 1)
	e.join(t, "bob", toollibrary.MemberRoleMember, 2)
	e.addTool(t, "t1", "Drill", toollibrary.ToolCategoryPower)
	e.addTool(t, "t2", "Tent", toollibrary.ToolCategoryCamping)

	// A limit must be given: the zero value is not "unlimited".
	err := e.st.Loans.Borrow(ctx, &toollibrary.Loan{LibraryID: "lib1", ToolID: "t1", LoanID: "l0", MemberID: "alice",
		BorrowedAt: time.Now(), DueAt: time.Now()}, toollibrary.LoanBorrowLimits{})
	wantErr(t, err, dynago.ErrLimitRequired)
	// A loan needs a due date.
	err = e.st.Loans.Borrow(ctx, &toollibrary.Loan{LibraryID: "lib1", ToolID: "t1", LoanID: "l0", MemberID: "alice", BorrowedAt: time.Now()},
		toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(1)})
	wantErr(t, err, dynago.ErrFieldRequired)

	// Borrow sets the loan's status itself, whatever the caller passes, and marks the tool on
	// loan in the same transaction. When everything is as required, it reads nothing.
	loan := &toollibrary.Loan{LibraryID: "lib1", ToolID: "t1", LoanID: "l1", MemberID: "alice", ToolName: "Drill",
		Status: toollibrary.LoanStatusReturned, BorrowedAt: time.Now(), DueAt: time.Now().Add(7 * 24 * time.Hour)}
	writes := e.counter.Writes()
	if n := e.reads(func() {
		must(t, e.st.Loans.Borrow(ctx, loan, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(1)}))
	}); n != 0 {
		t.Fatalf("Borrow made %d reads, want 0", n)
	}
	if w := e.counter.Writes() - writes; w != 1 || e.counter.TransactWriteItems.Load() == 0 {
		t.Fatalf("Borrow made %d write requests, want one transaction", w)
	}
	if got, _ := e.st.Loans.Get(ctx, loan.Key()); got.Status != toollibrary.LoanStatusActive {
		t.Fatalf("loan status = %q", got.Status)
	}
	if s := e.toolStatus(t, "t1"); s != toollibrary.ToolStatusOnLoan {
		t.Fatalf("tool status = %q", s)
	}
	if c := e.toolCounts(t); c != (toollibrary.ToolCounts{Available: 1, OnLoan: 1}) {
		t.Fatalf("tool counts = %+v", c)
	}

	// The tool is out, so nobody else can borrow it.
	wantErr(t, e.borrow(t, "l2", "t1", "bob", 2), toollibrary.ErrLoanBorrowRequiresTool)
	// Alice may have one tool out at a time.
	wantErr(t, e.borrow(t, "l3", "t2", "alice", 1), toollibrary.ErrMemberLoansActiveLimit)
	// Suspended members can't borrow; nor can members who don't exist, or anyone a missing tool.
	must(t, e.st.Members.Suspend(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "bob"}))
	wantErr(t, e.borrow(t, "l4", "t2", "bob", 2), toollibrary.ErrLoanBorrowRequiresMember)
	wantErr(t, e.borrow(t, "l5", "t2", "nobody", 2), toollibrary.ErrLoanBorrowRequiresMember)
	wantErr(t, e.borrow(t, "l6", "nope", "alice", 5), toollibrary.ErrLoanBorrowRequiresTool)
	must(t, e.st.Members.Reinstate(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "bob"}))
	// None of the refused borrows left anything behind.
	if act, _ := e.st.Loans.ActiveLoans(ctx, toollibrary.MemberLoansKey{LibraryID: "lib1", MemberID: "bob"}); act.Active != 0 {
		t.Fatalf("bob's active loans = %d", act.Active)
	}
	if tot, _ := e.st.Loans.Totals(ctx, toollibrary.LoanTotalsKey{LibraryID: "lib1"}); tot.Started != 1 {
		t.Fatalf("loans started = %d", tot.Started)
	}
	if s := e.toolStatus(t, "t2"); s != toollibrary.ToolStatusAvailable {
		t.Fatalf("tent status = %q", s)
	}

	// A member with no tools out may leave; one with tools out can't.
	must(t, e.st.Members.Leave(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "bob"}))
	e.join(t, "bob", toollibrary.MemberRoleMember, 2)
	must(t, e.borrow(t, "l7", "t2", "bob", 2))
	wantErr(t, e.st.Members.Leave(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "bob"}), toollibrary.ErrMemberLeaveRequiresMemberLoans)

	// Returning frees the tool and the member's slot, in one transaction.
	key := loan.Key()
	must(t, e.st.Loans.AddNote(ctx, key, toollibrary.LoanAddNote{Notes: dynago.Ptr("Chuck is stiff.")}))
	must(t, e.st.Loans.Return(ctx, key, toollibrary.LoanReturn{ReturnedAt: time.Now()}))
	wantErr(t, e.st.Loans.Return(ctx, key, toollibrary.LoanReturn{ReturnedAt: time.Now()}), toollibrary.ErrLoanReturnPrecondition)
	if s := e.toolStatus(t, "t1"); s != toollibrary.ToolStatusAvailable {
		t.Fatalf("tool status after return = %q", s)
	}
	if act, _ := e.st.Loans.ActiveLoans(ctx, toollibrary.MemberLoansKey{LibraryID: "lib1", MemberID: "alice"}); act.Active != 0 {
		t.Fatalf("alice's active loans after return = %d", act.Active)
	}
	must(t, e.borrow(t, "l8", "t1", "alice", 1)) // the tool and the slot are free again

	// The history lists a tool's loans newest first, without the notes.
	hist, _, err := e.st.Loans.History(ctx, toollibrary.LoanHistoryQuery{LibraryID: "lib1", ToolID: "t1"}, dynago.Page{})
	must(t, err)
	if len(hist) != 2 || hist[0].LoanID != "l8" || hist[1].Status != toollibrary.LoanStatusReturned || hist[1].ReturnedAt.IsZero() {
		t.Fatalf("history = %+v", hist)
	}
}

// A borrow is one transaction: whichever rule refuses it, nothing is written. The design this
// replaced borrowed and then marked the tool on loan in a second call, so a crash in between left
// a loan on an "available" tool; now there is no in between.
func TestBorrowIsAllOrNothing(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 1)
	e.join(t, "bob", toollibrary.MemberRoleSteward, 1) // so alice isn't the last active steward
	e.addTool(t, "t1", "Drill", toollibrary.ToolCategoryPower)
	e.addTool(t, "t2", "Tent", toollibrary.ToolCategoryCamping)
	e.addTool(t, "t3", "Ladder", toollibrary.ToolCategoryHand)
	must(t, e.borrow(t, "l1", "t2", "bob", 1)) // bob is at his cap
	must(t, e.st.Holds.Place(ctx, &toollibrary.Hold{LibraryID: "lib1", ToolID: "t3", MemberID: "alice", CodeHash: pickupCode("K3J-9QZ"),
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(48 * time.Hour)}))
	must(t, e.st.Members.Suspend(ctx, toollibrary.MemberKey{LibraryID: "lib1", MemberID: "alice"}))

	before := e.snapshot(t)
	cases := []struct {
		name         string
		tool, member string
		want         error
	}{
		{"member suspended", "t1", "alice", toollibrary.ErrLoanBorrowRequiresMember},
		{"tool on loan", "t2", "bob", toollibrary.ErrLoanBorrowRequiresTool},
		{"someone else's hold", "t3", "bob", toollibrary.ErrLoanBorrowRequiresHold},
		{"member at their cap", "t1", "bob", toollibrary.ErrMemberLoansActiveLimit},
		{"no member at all", "t1", "", dynago.ErrFieldRequired},
	}
	for _, c := range cases {
		wantErr(t, e.borrow(t, "l-"+c.name, c.tool, c.member, 1), c.want)
		if after := e.snapshot(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: a refused borrow changed the table", c.name)
		}
	}
}

// Many members race for one tool: exactly one borrow wins, and the rest are told the tool is out.
func TestConcurrentBorrowsOfOneTool(t *testing.T) {
	e := setup(t)
	e.addTool(t, "t1", "Pressure washer", toollibrary.ToolCategoryPower)
	for i := 0; i < 10; i++ {
		e.join(t, fmt.Sprintf("m%d", i), toollibrary.MemberRoleSteward, 1)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, out := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := e.borrow(t, fmt.Sprintf("l%d", i), "t1", fmt.Sprintf("m%d", i), 1)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, toollibrary.ErrLoanBorrowRequiresTool):
				out++
			default:
				t.Errorf("borrow %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || out != 9 {
		t.Fatalf("ok=%d out=%d", ok, out)
	}
	hist, _, err := e.st.Loans.History(ctx, toollibrary.LoanHistoryQuery{LibraryID: "lib1", ToolID: "t1"}, dynago.Page{})
	must(t, err)
	if len(hist) != 1 {
		t.Fatalf("%d loans of one tool", len(hist))
	}
}

// "My loans" lists current loans, soonest due first. It is a copy index, so it is readable
// consistently the moment a loan is taken, and a returned loan's copy is deleted with it.
func TestMyLoans(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 10)
	now := time.Now().UTC().Truncate(time.Second)
	for i, days := range []int{7, 2, 14} {
		id := fmt.Sprintf("t%d", i)
		e.addTool(t, id, "Tool "+id, toollibrary.ToolCategoryHand)
		must(t, e.st.Loans.Borrow(ctx, &toollibrary.Loan{LibraryID: "lib1", ToolID: id, LoanID: "l" + id, MemberID: "alice", ToolName: "Tool " + id,
			BorrowedAt: now, DueAt: now.Add(time.Duration(days) * 24 * time.Hour)}, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Unlimited()}))
	}
	myLoans := func() []string {
		mine, _, err := e.st.Loans.MyLoans(ctx, toollibrary.LoanMyLoansQuery{LibraryID: "lib1", MemberID: "alice"}, dynago.Page{})
		must(t, err)
		var ids []string
		for _, l := range mine {
			ids = append(ids, l.ToolName)
		}
		return ids
	}
	if got := strings.Join(myLoans(), ","); got != "Tool t1,Tool t0,Tool t2" {
		t.Fatalf("my loans = %s", got)
	}
	must(t, e.st.Loans.Return(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t1", LoanID: "lt1"}, toollibrary.LoanReturn{ReturnedAt: now}))
	must(t, e.st.Loans.Extend(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t2", LoanID: "lt2"}, toollibrary.LoanExtend{DueAt: now.Add(24 * time.Hour)}))
	if got := strings.Join(myLoans(), ","); got != "Tool t2,Tool t0" {
		t.Fatalf("my loans after a return and an extension = %s", got)
	}
}

func TestOverdueRange(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 10)
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("t%d", i)
		e.addTool(t, id, "Tool "+id, toollibrary.ToolCategoryHand)
		must(t, e.st.Loans.Borrow(ctx, &toollibrary.Loan{LibraryID: "lib1", ToolID: id, LoanID: "l" + id, MemberID: "alice",
			BorrowedAt: now, DueAt: now.Add(time.Duration(i-2) * 24 * time.Hour)}, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Unlimited()}))
	}
	// Due before now (bounds are inclusive, and one loan is due exactly now): the first two.
	before := now.Add(-time.Second)
	overdue, _, err := e.st.Loans.Overdue(ctx, toollibrary.LoanOverdueQuery{LibraryID: "lib1", To: &before}, dynago.Page{})
	must(t, err)
	if len(overdue) != 2 || overdue[0].LoanID != "lt0" || overdue[1].LoanID != "lt1" {
		t.Fatalf("overdue = %+v", overdue)
	}
	// A cursor belongs to its range: reused with other bounds it is refused, rather than sent to
	// DynamoDB, which would reject a start key outside the range.
	_, next, err := e.st.Loans.Overdue(ctx, toollibrary.LoanOverdueQuery{LibraryID: "lib1", To: &before}, dynago.Page{Size: 1})
	must(t, err)
	later := now.Add(48 * time.Hour)
	_, _, err = e.st.Loans.Overdue(ctx, toollibrary.LoanOverdueQuery{LibraryID: "lib1", To: &later}, dynago.Page{Size: 1, Cursor: next})
	wantErr(t, err, dynago.ErrInvalidCursor)

	// Extending moves a loan out of the overdue range.
	must(t, e.st.Loans.Extend(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t0", LoanID: "lt0"}, toollibrary.LoanExtend{DueAt: now.Add(30 * 24 * time.Hour)}))
	overdue, _, _ = e.st.Loans.Overdue(ctx, toollibrary.LoanOverdueQuery{LibraryID: "lib1", To: &before}, dynago.Page{})
	if len(overdue) != 1 || overdue[0].LoanID != "lt1" {
		t.Fatalf("overdue after extend = %+v", overdue)
	}
}

// pickupSecret stands in for a secret from the application's secret store.
var pickupSecret = []byte("not-a-real-secret")

// pickupCode is what the library stores for a pickup code: an HMAC under a server-side secret. A
// plain hash of a six-character code can be reversed by hashing every possible code.
func pickupCode(code string) string {
	mac := hmac.New(sha256.New, pickupSecret)
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHolds(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 2)
	e.join(t, "bob", toollibrary.MemberRoleMember, 2)
	e.addTool(t, "t1", "Drill", toollibrary.ToolCategoryPower)
	key := toollibrary.HoldKey{LibraryID: "lib1", ToolID: "t1"}
	hold := func(member, code string, expires time.Time) *toollibrary.Hold {
		return &toollibrary.Hold{LibraryID: "lib1", ToolID: "t1", MemberID: member, CodeHash: pickupCode(code), CreatedAt: time.Now(), ExpiresAt: expires}
	}

	// A hold must say when it lapses: one without an expiry would reserve the tool forever.
	wantErr(t, e.st.Holds.Place(ctx, hold("alice", "K3J-9QZ", time.Time{})), dynago.ErrFieldRequired)

	// A hold that has already lapsed: DynamoDB hasn't deleted it yet, but nobody sees it.
	must(t, e.st.Holds.Place(ctx, hold("alice", "OLD-CODE", time.Now().Add(-time.Hour))))
	if e.raw(t, "LIB#lib1#TOOL#t1", "HOLD") == nil {
		t.Fatal("the lapsed hold should still be stored")
	}
	_, err := e.st.Holds.Get(ctx, key)
	wantErr(t, err, toollibrary.ErrHoldNotFound)
	found, _, err := e.st.Holds.FindByCode(ctx, toollibrary.HoldFindByCodeQuery{CodeHash: pickupCode("OLD-CODE")}, dynago.Page{})
	must(t, err)
	if len(found) != 0 {
		t.Fatalf("expired hold found by code: %+v", found)
	}
	wantErr(t, e.st.Holds.Release(ctx, key), toollibrary.ErrHoldNotFound)

	// A new hold may replace the lapsed one; a live one can't be replaced.
	must(t, e.st.Holds.Place(ctx, hold("alice", "K3J-9QZ", time.Now().Add(48*time.Hour))))
	wantErr(t, e.st.Holds.Place(ctx, hold("bob", "OTHER", time.Now().Add(time.Hour))), toollibrary.ErrHoldExists)
	found, _, _ = e.st.Holds.FindByCode(ctx, toollibrary.HoldFindByCodeQuery{CodeHash: pickupCode("K3J-9QZ")}, dynago.Page{})
	if len(found) != 1 || found[0].MemberID != "alice" {
		t.Fatalf("by code = %+v", found)
	}

	// The hold reserves the tool: bob can't borrow it...
	wantErr(t, e.borrow(t, "l1", "t1", "bob", 2), toollibrary.ErrLoanBorrowRequiresHold)
	// ...and alice collects it: borrowing consumes her hold, in the same transaction.
	must(t, e.borrow(t, "l2", "t1", "alice", 2))
	if e.raw(t, "LIB#lib1#TOOL#t1", "HOLD") != nil {
		t.Fatal("the hold outlived the borrow that collected it")
	}
	// Holds need an available tool.
	wantErr(t, e.st.Holds.Place(ctx, hold("bob", "X", time.Now().Add(time.Hour))), toollibrary.ErrHoldPlaceRequiresTool)
	must(t, e.st.Loans.Return(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t1", LoanID: "l2"}, toollibrary.LoanReturn{ReturnedAt: time.Now()}))

	// A lapsed hold reserves nothing: bob borrows despite it, clearing it away.
	must(t, e.st.Holds.Place(ctx, hold("alice", "LATE", time.Now().Add(-time.Minute))))
	must(t, e.borrow(t, "l3", "t1", "bob", 2))
	if e.raw(t, "LIB#lib1#TOOL#t1", "HOLD") != nil {
		t.Fatal("the lapsed hold should have been cleared")
	}
	must(t, e.st.Loans.Return(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t1", LoanID: "l3"}, toollibrary.LoanReturn{ReturnedAt: time.Now()}))

	// Retiring a tool cancels its hold.
	must(t, e.st.Holds.Place(ctx, hold("alice", "K3J-9QZ", time.Now().Add(48*time.Hour))))
	must(t, e.st.Tools.Retire(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t1"}))
	if e.raw(t, "LIB#lib1#TOOL#t1", "HOLD") != nil {
		t.Fatal("retiring the tool left its hold")
	}
}

// Old code must not overwrite items that newer code wrote, on any write path.
func TestNewerSchemaIsNotOverwritten(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 2)
	e.addTool(t, "t1", "Drill", toollibrary.ToolCategoryPower)
	e.addTool(t, "t2", "Tent", toollibrary.ToolCategoryCamping)
	must(t, e.table.Update("PK", "LIB#lib1").Range("SK", "MEMBER#alice").Set("'_v'", 99).Run(ctx))
	must(t, e.table.Update("PK", "LIB#lib1#TOOL#t1").Range("SK", "TOOL").Set("'_v'", 99).Run(ctx))
	alice := toollibrary.MemberKey{LibraryID: "lib1", MemberID: "alice"}
	wantErr(t, e.st.Members.SetLoanCap(ctx, alice, toollibrary.MemberSetLoanCap{MaxLoans: 9}), dynago.ErrNewerSchema)                 // single UpdateItem
	wantErr(t, e.st.Members.UpdateProfile(ctx, alice, toollibrary.MemberUpdateProfile{Name: dynago.Ptr("x")}), dynago.ErrNewerSchema) // read-first
	wantErr(t, e.st.Tools.Retire(ctx, toollibrary.ToolKey{LibraryID: "lib1", ToolID: "t1"}), dynago.ErrNewerSchema)                   // read-free
	// Borrow changes the tool too: its read-free attempt fails, and the read reports why.
	must(t, e.table.Update("PK", "LIB#lib1").Range("SK", "MEMBER#alice").Set("'_v'", 1).Run(ctx))
	wantErr(t, e.borrow(t, "l1", "t1", "alice", 2), dynago.ErrNewerSchema)
	if c := e.toolCounts(t); c != (toollibrary.ToolCounts{Available: 2}) {
		t.Fatalf("counts changed by a refused write: %+v", c)
	}
}

// Concurrent borrowers race for a member's last slot: exactly the limit get through.
func TestConcurrentBorrowsRespectTheLimit(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 3)
	for i := 0; i < 10; i++ {
		e.addTool(t, fmt.Sprintf("t%d", i), "Tool", toollibrary.ToolCategoryHand)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, full := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := e.borrow(t, fmt.Sprintf("l%d", i), fmt.Sprintf("t%d", i), "alice", 3)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, toollibrary.ErrMemberLoansActiveLimit):
				full++
			default:
				t.Errorf("borrow %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 3 || full != 7 {
		t.Fatalf("ok=%d full=%d", ok, full)
	}
	if act, _ := e.st.Loans.ActiveLoans(ctx, toollibrary.MemberLoansKey{LibraryID: "lib1", MemberID: "alice"}); act.Active != 3 {
		t.Fatalf("active = %d", act.Active)
	}
}

func TestCursors(t *testing.T) {
	e := setup(t)
	for i := 0; i < 5; i++ {
		e.addTool(t, fmt.Sprintf("t%d", i), fmt.Sprintf("Spade %d", i), toollibrary.ToolCategoryGarden)
	}
	q := toollibrary.ToolCatalogueQuery{LibraryID: "lib1", Category: toollibrary.ToolCategoryGarden}
	var all []toollibrary.ToolByCategory
	page := dynago.Page{Size: 2}
	for {
		tools, next, err := e.st.Tools.Catalogue(ctx, q, page)
		must(t, err)
		all = append(all, tools...)
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if len(all) != 5 {
		t.Fatalf("paged %d tools", len(all))
	}
	_, next, _ := e.st.Tools.Catalogue(ctx, q, dynago.Page{Size: 2})
	_, _, err := e.st.Tools.Catalogue(ctx, toollibrary.ToolCatalogueQuery{LibraryID: "lib1", Category: toollibrary.ToolCategoryPower}, dynago.Page{Cursor: next})
	wantErr(t, err, dynago.ErrInvalidCursor)

	// With signing on, cursors can't be edited, and unsigned ones are refused.
	dynago.SignCursors([]byte("test key"))
	defer dynago.SignCursors(nil)
	_, signed, err := e.st.Tools.Catalogue(ctx, q, dynago.Page{Size: 2})
	must(t, err)
	if _, _, err := e.st.Tools.Catalogue(ctx, q, dynago.Page{Cursor: signed}); err != nil {
		t.Fatalf("signed cursor refused: %v", err)
	}
	_, _, err = e.st.Tools.Catalogue(ctx, q, dynago.Page{Cursor: next})
	wantErr(t, err, dynago.ErrInvalidCursor)
}

// {name|lower} in the directory's sort key orders names case-insensitively, and {email|lower} in the
// claim makes email unique and findable regardless of case.
func TestCaseInsensitiveKeys(t *testing.T) {
	e := setup(t)
	for id, name := range map[string]string{"m1": "Zoe", "m2": "adam", "m3": "Bea", "m4": "carl"} {
		must(t, e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: id, Name: name,
			Email: strings.ToUpper(id) + "@Example.org", Role: toollibrary.MemberRoleSteward, Status: toollibrary.MemberStatusActive}))
	}
	dir, _, err := e.st.Members.Directory(ctx, toollibrary.MemberDirectoryQuery{LibraryID: "lib1"}, dynago.Page{})
	must(t, err)
	var names []string
	for _, m := range dir {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "adam,Bea,carl,Zoe" {
		t.Fatalf("directory order = %v", names)
	}
	// The name keeps its case; only the key is lowered.
	if item := e.raw(t, "LIB#lib1", "MEMBER#m1"); item["name"] != "Zoe" {
		t.Fatalf("stored name = %v", item["name"])
	}
	m, err := e.st.Members.GetByEmail(ctx, "lib1", "m1@example.ORG")
	must(t, err)
	if m.MemberID != "m1" || m.Email != "M1@Example.org" {
		t.Fatalf("by email = %+v", m)
	}
	err = e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "m9", Email: "m1@example.org", Status: toollibrary.MemberStatusActive})
	wantErr(t, err, toollibrary.ErrMemberEmailTaken)

	// "é" as one code point and as "e" plus a combining accent look identical: they are one email.
	composed, decomposed := "\u00e9mile@example.org", "e\u0301mile@example.org"
	must(t, e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "m5", Name: "Émile", Email: composed,
		Role: toollibrary.MemberRoleSteward, Status: toollibrary.MemberStatusActive}))
	if m, err := e.st.Members.GetByEmail(ctx, "lib1", decomposed); err != nil || m.MemberID != "m5" {
		t.Fatalf("by decomposed email: %+v %v", m, err)
	}
	err = e.st.Members.Join(ctx, &toollibrary.Member{LibraryID: "lib1", MemberID: "m6", Email: decomposed, Status: toollibrary.MemberStatusActive})
	wantErr(t, err, toollibrary.ErrMemberEmailTaken)
}

// When the tool can't be changed without reading it (here it is stored at a schema version older
// than its counters, simulated with _v 0, so its contributions aren't the ones assumed), Borrow
// reads the tool and the hold, and writes the changes guarded by what it read.
func TestBorrowFallsBackToReading(t *testing.T) {
	e := setup(t)
	e.join(t, "alice", toollibrary.MemberRoleSteward, 2)
	e.addTool(t, "t1", "Drill", toollibrary.ToolCategoryPower)
	e.addTool(t, "t2", "Tent", toollibrary.ToolCategoryCamping)
	for _, id := range []string{"t1", "t2"} {
		must(t, e.table.Update("PK", "LIB#lib1#TOOL#"+id).Range("SK", "TOOL").Set("'_v'", 0).Run(ctx))
	}
	must(t, e.st.Holds.Place(ctx, &toollibrary.Hold{LibraryID: "lib1", ToolID: "t2", MemberID: "alice", CodeHash: pickupCode("K3J-9QZ"),
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(48 * time.Hour)}))

	// No hold on t1: the read finds none, and the write checks none appeared in the meantime.
	if n := e.reads(func() { must(t, e.borrow(t, "l1", "t1", "alice", 2)) }); n != 2 {
		t.Fatalf("Borrow made %d reads, want 2 (the tool and the hold)", n)
	}
	// alice's own hold on t2: the read finds it hers, and the write deletes it.
	must(t, e.borrow(t, "l2", "t2", "alice", 2))
	if e.raw(t, "LIB#lib1#TOOL#t2", "HOLD") != nil {
		t.Fatal("the hold outlived the borrow that collected it")
	}
	for _, id := range []string{"t1", "t2"} {
		if s := e.toolStatus(t, id); s != toollibrary.ToolStatusOnLoan {
			t.Fatalf("%s status = %q", id, s)
		}
	}
	if c := e.toolCounts(t); c != (toollibrary.ToolCounts{OnLoan: 2}) {
		t.Fatalf("tool counts = %+v", c)
	}
	// Returning reads the tool the same way, and the counts come back.
	must(t, e.st.Loans.Return(ctx, toollibrary.LoanKey{LibraryID: "lib1", ToolID: "t1", LoanID: "l1"}, toollibrary.LoanReturn{ReturnedAt: time.Now()}))
	if c := e.toolCounts(t); c != (toollibrary.ToolCounts{Available: 1, OnLoan: 1}) {
		t.Fatalf("tool counts after return = %+v", c)
	}
}
