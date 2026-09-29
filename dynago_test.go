package dynago

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/guregu/dynamo/v2"
)

func TestFmtIntSortsNumerically(t *testing.T) {
	nums := []int64{-1 << 62, -1000, -1, 0, 1, 9, 10, 1000, 1 << 62}
	strs := make([]string, len(nums))
	for i, n := range nums {
		strs[i] = FmtInt(n)
	}
	if !sort.StringsAreSorted(strs) {
		t.Fatalf("not sorted: %v", strs)
	}
}

func TestFmtTimeIsFixedWidthUTC(t *testing.T) {
	a := FmtTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	b := FmtTime(time.Date(2026, 1, 2, 4, 4, 5, 120, time.FixedZone("CET", 3600)))
	if a != "2026-01-02T03:04:05.000000000Z" || b != "2026-01-02T03:04:05.000000120Z" {
		t.Fatalf("%q, %q", a, b)
	}
}

func TestPath(t *testing.T) {
	for in, want := range map[string]string{"name": "name", "_rev": "'_rev'", "a1": "a1", "x-y": "'x-y'", "1a": "'1a'"} {
		if got := Path(in); got != want {
			t.Errorf("Path(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	lek := dynamo.PagingKey{"PK": &types.AttributeValueMemberS{Value: "A"}, "SK": &types.AttributeValueMemberS{Value: "B#1"}}
	c, err := encodeCursor("scope", lek)
	if err != nil || c == "" {
		t.Fatal(err)
	}
	got, err := decodeCursor("scope", c)
	if err != nil || got["SK"].(*types.AttributeValueMemberS).Value != "B#1" {
		t.Fatalf("decode: %v %v", got, err)
	}
	if _, err := decodeCursor("other", c); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("scope check: %v", err)
	}
	if empty, _ := encodeCursor("scope", nil); empty != "" {
		t.Fatal("no LEK should mean no cursor")
	}
}

func TestShards(t *testing.T) {
	pks := ShardPKs("C", 4)
	if len(pks) != 4 || pks[3] != "C#S3" {
		t.Fatalf("%v", pks)
	}
	got := ShardPK("C", Key{"a", "b"}, 4)
	found := false
	for _, pk := range pks {
		found = found || pk == got
	}
	if !found || ShardPK("C", Key{"a", "b"}, 1) != "C" {
		t.Fatalf("ShardPK = %q", got)
	}
}

func TestDiff(t *testing.T) {
	table := dynamo.Table{}
	owner := Key{"O", "1"}
	stats := Key{"S", "STATS"}
	limitErr := errors.New("full")
	counter := func(attr string, n int64, lim Limit) Derived {
		return Derived{Kind: KindCounter, Key: stats, Type: "Stats", Attr: attr, Amount: n, Limit: lim, LimitErr: limitErr}
	}
	claim := func(v string) Derived { return Derived{Kind: KindClaim, Key: Key{"U#" + v, "U"}, Type: "U"} }

	// Nothing changed: no writes.
	same := []Derived{counter("n", 1, Limit{}), claim("x")}
	if ops, err := Diff(table, owner, same, same); err != nil || len(ops) != 0 {
		t.Fatalf("unchanged: %d ops, %v", len(ops), err)
	}
	// Two attributes of one counter item become one update; a moved claim is a put and a delete.
	ops, err := Diff(table, owner,
		[]Derived{counter("n", 1, Limit{}), claim("x")},
		[]Derived{counter("n", 2, Limit{}), counter("m", 1, Limit{}), claim("y")})
	if err != nil || len(ops) != 3 {
		t.Fatalf("changed: %d ops, %v", len(ops), err)
	}
	// A single contribution larger than the limit fails before touching DynamoDB.
	if _, err := Diff(table, owner, nil, []Derived{counter("n", 5, Max(3))}); !errors.Is(err, limitErr) {
		t.Fatalf("over limit: %v", err)
	}
	// A lower bound turns a decrement into a conditional one, with its own error.
	minErr := errors.New("too few")
	floor := func(n int64) Derived {
		return Derived{Kind: KindCounter, Key: stats, Type: "Stats", Attr: "owners", Amount: n, Min: Max(1), MinErr: minErr}
	}
	if ops, err := Diff(table, owner, []Derived{floor(1)}, nil); err != nil || len(ops) != 1 || !errors.Is(ops[0].err, minErr) {
		t.Fatalf("decrement with min: %v %v", ops, err)
	}
	if ops, err := Diff(table, owner, nil, []Derived{floor(1)}); err != nil || len(ops) != 1 || ops[0].err != nil {
		t.Fatalf("increment ignores min: %v %v", ops, err)
	}
	// Shrinking never checks the limit.
	if ops, err := Diff(table, owner, []Derived{counter("n", 5, Limit{})}, []Derived{counter("n", 1, Max(3))}); err != nil || len(ops) != 1 || ops[0].err != nil {
		t.Fatalf("shrink: %v %v", ops, err)
	}
}

func TestNewRevIsPositiveAndVaries(t *testing.T) {
	seen := map[int64]bool{}
	for i := 0; i < 100; i++ {
		r := NewRev()
		if r <= 0 || seen[r] {
			t.Fatalf("rev %d", r)
		}
		seen[r] = true
	}
}

func TestSignedCursors(t *testing.T) {
	lek := dynamo.PagingKey{"PK": &types.AttributeValueMemberS{Value: "A"}, "SK": &types.AttributeValueMemberS{Value: "B"}}
	plain, _ := encodeCursor("scope", lek)
	SignCursors([]byte("k1"))
	defer SignCursors(nil)
	signed, _ := encodeCursor("scope", lek)
	if _, err := decodeCursor("scope", signed); err != nil {
		t.Fatalf("signed: %v", err)
	}
	if _, err := decodeCursor("scope", plain); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unsigned accepted: %v", err)
	}
	SignCursors([]byte("k2"))
	if _, err := decodeCursor("scope", signed); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("wrong key accepted: %v", err)
	}
}

func TestLimits(t *testing.T) {
	if err := RequireLimit("X", Limit{}); !errors.Is(err, ErrLimitRequired) {
		t.Fatalf("zero limit: %v", err)
	}
	if RequireLimit("X", Unlimited()) != nil || RequireLimit("X", Max(3)) != nil {
		t.Fatal("given limits rejected")
	}
	table := dynamo.Table{}
	d := func(n int64, lim Limit) Derived {
		return Derived{Kind: KindCounter, Key: Key{"C", "C"}, Type: "C", Attr: "n", Amount: n, Limit: lim, LimitRequired: true}
	}
	// Growing a value that needs a caller limit without one fails before anything is written.
	if _, err := Diff(table, Key{"O", "O"}, nil, []Derived{d(1, Limit{})}); !errors.Is(err, ErrLimitRequired) {
		t.Fatalf("missing limit: %v", err)
	}
	// Unlimited grows without a condition.
	if ops, err := Diff(table, Key{"O", "O"}, nil, []Derived{d(1, Unlimited())}); err != nil || len(ops) != 1 || ops[0].err != nil {
		t.Fatalf("unlimited: %v %v", ops, err)
	}
	// Shrinking never needs the limit.
	if _, err := Diff(table, Key{"O", "O"}, []Derived{d(1, Limit{})}, nil); err != nil {
		t.Fatalf("shrink: %v", err)
	}
}

func TestCheckKeyPart(t *testing.T) {
	if err := CheckKeyPart("id", "a#b", "#"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("separator accepted: %v", err)
	}
	if err := CheckKeyPart("id", "a-b", "#"); err != nil {
		t.Fatal(err)
	}
}

func TestLowerNormalises(t *testing.T) {
	composed, decomposed := "Émile", "Émile"
	if composed == decomposed || Lower(composed) != Lower(decomposed) || Lower(composed) != "émile" {
		t.Fatalf("Lower(%q) = %q, Lower(%q) = %q", composed, Lower(composed), decomposed, Lower(decomposed))
	}
}

// DynamoDB rejects a transaction touching one item twice; DynamoDB Local sometimes cancels it with
// a misleading ConditionalCheckFailed instead. Run refuses it before sending, so the error is the
// same everywhere and names the item.
func TestRunRefusesTheSameItemTwice(t *testing.T) {
	tbl := dynamo.NewFromIface(nil).Table("t")
	k := Key{PK: "COUNTS", SK: "C"}
	ops := []Op{
		UpdateOp(k, tbl.Update(AttrPK, k.PK).Range(AttrSK, k.SK).Add("n", 1), nil),
		CheckOp(k, tbl.Check(AttrPK, k.PK).Range(AttrSK, k.SK).If("attribute_exists($)", AttrPK), nil),
	}
	err := Run(context.Background(), nil, ops)
	if !errors.Is(err, ErrSameItemTwice) {
		t.Fatalf("got %v, want ErrSameItemTwice", err)
	}
}

// Changes from two owners to one counter item are merged into one update.
func TestDiffAllMergesCounterItems(t *testing.T) {
	tbl := dynamo.NewFromIface(nil).Table("t")
	counter := Key{PK: "TENANT#1", SK: "COUNTS"}
	d := func(attr string) []Derived {
		return []Derived{{Kind: KindCounter, Key: counter, Type: "Counts", Attr: attr, Amount: 1}}
	}
	ops, err := DiffAll(tbl, []Change{
		{Owner: Key{PK: "A#1", SK: "A"}, Before: d("open"), After: d("closed")},
		{Owner: Key{PK: "A#2", SK: "A"}, Before: d("closed"), After: d("open")},
		{Owner: Key{PK: "A#3", SK: "A"}, After: d("open")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].key != counter {
		t.Fatalf("got %d ops, want one update of the counter item", len(ops))
	}
}

func TestCheckKeyPartSeesLoweredValues(t *testing.T) {
	for _, v := range []string{"a#b", "AX", "\u212a"} {
		if err := CheckKeyPart("f", v, "#xk"); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("CheckKeyPart(%q) = %v, want ErrInvalidKey", v, err)
		}
	}
	if err := CheckKeyPart("f", "Drill 2", "#"); err != nil {
		t.Error(err)
	}
}

// A policy with a huge base delay still waits at most MaxDelay between attempts.
func TestRetryDelaysAreCapped(t *testing.T) {
	SetRetries(RetryPolicy{Attempts: 3, BaseDelay: time.Hour, MaxDelay: time.Millisecond})
	defer SetRetries(DefaultRetries)
	start := time.Now()
	err := Retry(context.Background(), func() error { return ErrStale })
	if !errors.Is(err, ErrConflict) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

// Cursors signed with a previous key stay valid while it is listed, so keys can be rotated.
func TestCursorKeyRotation(t *testing.T) {
	defer SignCursors(nil)
	lek := dynamo.PagingKey{"PK": &types.AttributeValueMemberS{Value: "A"}, "SK": &types.AttributeValueMemberS{Value: "B"}}
	SignCursors([]byte("old"))
	c, err := encodeCursor("scope", lek)
	if err != nil {
		t.Fatal(err)
	}
	SignCursors([]byte("new"), []byte("old"))
	if _, err := decodeCursor("scope", c); err != nil {
		t.Fatalf("old cursor during rotation: %v", err)
	}
	SignCursors([]byte("new"))
	if _, err := decodeCursor("scope", c); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("old cursor after rotation: %v", err)
	}
}

// A cursor is tied to the range bounds of the query that produced it.
func TestCursorScopeIncludesBounds(t *testing.T) {
	a, b := "2026-01-01", "2026-02-01"
	if cursorScope(QuerySpec{Scope: "s", To: &a}) == cursorScope(QuerySpec{Scope: "s", To: &b}) ||
		cursorScope(QuerySpec{Scope: "s", From: &a}) == cursorScope(QuerySpec{Scope: "s", To: &a}) {
		t.Fatal("different bounds give the same cursor scope")
	}
}

func TestDistinct(t *testing.T) {
	if got := Distinct([]string{"b", "a", "b", "c", "a"}); !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Fatalf("got %q", got)
	}
}

// Conflict records have sort keys DynamoDB accepts, one per source, whatever the source key.
func TestConflictKeys(t *testing.T) {
	j := &job{Migration: &Migration{From: "things-g1"}}
	long := strings.Repeat("x", 1100)
	keys := map[string]bool{}
	for _, src := range []Key{{"a|b", "c"}, {"a", "b|c"}, {long, "1"}, {long, "2"}} {
		k := j.conflictKey(src)
		if len(k.SK) > 1024 || keys[k.SK] {
			t.Fatalf("%v: sort key %q", src, k.SK)
		}
		keys[k.SK] = true
	}
}
