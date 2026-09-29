package vet

import (
	"sort"
	"strings"
	"testing"
)

func TestVet(t *testing.T) {
	if testing.Short() {
		t.Skip("loads packages with the go command")
	}
	res, err := Run(".", []string{"./testdata/app"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var unmarked, marked []string
	for _, c := range res.Unmarked {
		unmarked = append(unmarked, c.Name)
	}
	for _, c := range res.Marked {
		marked = append(marked, c.Name+": "+c.Reason)
	}
	sort.Strings(unmarked)
	sort.Strings(marked)
	got := strings.Join(unmarked, "\n")
	for _, w := range []string{"(*dynamodb.Client).Scan", "dynamodb.NewScanPaginator", "dynago.GetOne", ".Put", ".Run"} {
		if !strings.Contains(got, w) {
			t.Errorf("unmarked calls lack %s:\n%s", w, got)
		}
	}
	if strings.Contains(got, "Options") || strings.Contains(got, "Max") || strings.Contains(got, "dynamo.New") {
		t.Errorf("flagged a call that makes no request:\n%s", got)
	}
	// Raw's three calls, SDK's two, NoReason's mark without a reason, Runtime's one, Trailing's
	// PutItem and Wrapped's DeleteItem through an interface; nothing from the generated file.
	if len(res.Unmarked) != 9 || !strings.Contains(got, "(app.Items).DeleteItem") || !strings.Contains(got, "(*dynamodb.Client).PutItem") {
		t.Errorf("want 9 unmarked calls, got %d:\n%s", len(res.Unmarked), got)
	}
	all := strings.Join(marked, "\n")
	// Export's three, Marked's three, Chained's three (across three lines), Trailing's GetItem.
	if len(res.Marked) != 10 || !strings.Contains(all, "the nightly export reads everything") || !strings.Contains(all, "seeding a fixture") ||
		!strings.Contains(all, "(*dynamo.Query).All: the admin console lists a partition") || !strings.Contains(all, "GetItem: a health check") {
		t.Errorf("marked calls:\n%s", all)
	}
	for _, c := range res.Unmarked {
		if strings.HasSuffix(c.Pos.Filename, "app_dynago.go") {
			t.Errorf("flagged generated code: %v", c.Pos)
		}
	}
}
