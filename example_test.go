package dynago_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/guregu/dynamo/v2"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/examples/toollibrary"
)

// These examples use the store generated from examples/toollibrary/toollibrary.dynago.yaml.
var (
	ctx = context.Background()
	st  *toollibrary.Store
)

// A store is created from a guregu/dynamo DB and a table name; its fields hold one store per entity.
func Example() {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		panic(err)
	}
	st := toollibrary.New(dynamo.New(cfg), "toollibrary-prod")

	tool, err := st.Tools.Get(ctx, toollibrary.ToolKey{LibraryID: "lib_1", ToolID: "t_42"})
	switch {
	case errors.Is(err, toollibrary.ErrToolNotFound):
		fmt.Println("no such tool")
	case err != nil:
		panic(err)
	default:
		fmt.Println(tool.Name, tool.Status)
	}
}

// Max caps a counter value declared with `limit: arg`, here how many tools a member may have out.
// The counter update is conditioned on the limit, so concurrent borrows can never exceed it. A
// limit must always be given: pass Unlimited to switch it off on purpose.
func ExampleMax() {
	member, err := st.Members.Get(ctx, toollibrary.MemberKey{LibraryID: "lib_1", MemberID: "m_7"})
	if err != nil {
		panic(err)
	}
	err = st.Loans.Borrow(ctx, &toollibrary.Loan{
		LibraryID: "lib_1", ToolID: "t_42", LoanID: "ln_9", MemberID: "m_7",
		BorrowedAt: time.Now(), DueAt: time.Now().Add(7 * 24 * time.Hour),
	}, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(member.MaxLoans)})
	switch {
	case errors.Is(err, toollibrary.ErrMemberLoansActiveLimit):
		fmt.Println("return something first")
	case errors.Is(err, toollibrary.ErrLoanBorrowRequiresTool):
		fmt.Println("that tool is out")
	case errors.Is(err, toollibrary.ErrLoanBorrowRequiresHold):
		fmt.Println("that tool is reserved for someone else")
	}
}

// From starts a write from an entity the store returned: the write skips its own read, and fails
// with ErrVersionMismatch if the item changed since, so decisions made on the entity still hold.
func ExampleFrom() {
	key := toollibrary.LoanKey{LibraryID: "lib_1", ToolID: "t_42", LoanID: "ln_9"}
	loan, err := st.Loans.Get(ctx, key)
	if err != nil {
		panic(err)
	}
	if loan.DueAt.Sub(loan.BorrowedAt) > 28*24*time.Hour {
		return // business rule decided on the loaded entity: no more extensions
	}
	err = st.Loans.Extend(ctx, key, toollibrary.LoanExtend{DueAt: loan.DueAt.Add(7 * 24 * time.Hour)}, dynago.From(loan))
	if errors.Is(err, dynago.ErrVersionMismatch) {
		fmt.Println("the loan changed; reload and decide again")
	}
}

// IfVersion carries a version across requests, typically as an ETag, so a stale edit form cannot
// overwrite a newer change. ReturnVersion hands back the new version for the response.
func ExampleIfVersion() {
	key := toollibrary.ToolKey{LibraryID: "lib_1", ToolID: "t_42"}
	get := func(w http.ResponseWriter, r *http.Request) {
		tool, _ := st.Tools.Get(r.Context(), key)
		w.Header().Set("ETag", strconv.Quote(tool.Version())) // ETags are quoted (RFC 9110)
	}
	put := func(w http.ResponseWriter, r *http.Request) {
		var version string
		err := st.Tools.EditDetails(r.Context(), key, toollibrary.ToolEditDetails{Name: dynago.Ptr(r.FormValue("name"))},
			dynago.IfVersion(strings.Trim(strings.TrimPrefix(r.Header.Get("If-Match"), "W/"), `"`)), dynago.ReturnVersion(&version))
		if errors.Is(err, dynago.ErrVersionMismatch) {
			http.Error(w, "changed by someone else; reload", http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("ETag", strconv.Quote(version))
	}
	_, _ = get, put
}

// Ptr makes the pointers that optional (patch) fields take: nil leaves a field unchanged.
func ExamplePtr() {
	key := toollibrary.MemberKey{LibraryID: "lib_1", MemberID: "m_7"}
	// Only the phone number changes; the name is left as it is.
	_ = st.Members.UpdateProfile(ctx, key, toollibrary.MemberUpdateProfile{Phone: dynago.Ptr("555-0100")})
}

// Page selects one page of a query. Every query method makes exactly one request per call and
// returns the cursor for the next page, "" at the end.
func ExamplePage() {
	q := toollibrary.ToolCatalogueQuery{LibraryID: "lib_1", Category: toollibrary.ToolCategoryPower}
	page := dynago.Page{Size: 25}
	for {
		tools, next, err := st.Tools.Catalogue(ctx, q, page)
		if err != nil {
			panic(err)
		}
		for _, t := range tools {
			fmt.Println(t.Name, t.Status)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
}

// SignCursors makes page cursors tamper-proof. Call it once at startup with a secret shared by
// every instance.
func ExampleSignCursors() {
	dynago.SignCursors([]byte("a secret from your secret store"))
}
