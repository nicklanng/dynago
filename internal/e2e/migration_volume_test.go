package e2e_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nicklanng/dynago/examples/toollibrary"
	"github.com/nicklanng/dynago/internal/testdb"
)

// TestMigrationAtVolume migrates 50,000 tools across 500 libraries between two real tables: enough
// for every scan segment to page through many 1 MB pages, and for the job's workers to run
// concurrently for minutes. The old table changes between passes. It runs against AWS only
// (DYNAGO_TEST_AWS): DynamoDB Local has no pages worth the name, and it takes a few minutes and
// well under a dollar.
func TestMigrationAtVolume(t *testing.T) {
	t.Parallel()
	if !testdb.AWS() {
		t.Skip("runs against AWS only: set " + testdb.EnvAWS)
	}
	const libraries, toolsPer, workers = 500, 100, 16
	g := setupGenerations(t)
	lib := func(l int) string { return fmt.Sprintf("lib%03d", l) }
	categories := []toollibrary.ToolCategory{toollibrary.ToolCategoryPower, toollibrary.ToolCategoryGarden,
		toollibrary.ToolCategoryHand, toollibrary.ToolCategoryKitchen, toollibrary.ToolCategoryCamping}

	// Seed the old generation through its store, a library per worker at a time: tools of one
	// library share its counter item, so writing them one after another avoids conflicts.
	start := time.Now()
	parallel(t, libraries, 32, func(l int) error {
		id := lib(l)
		if err := g.old.Libraries.Open(ctx, &toollibrary.Library{LibraryID: id, Name: "Library " + id, Slug: "slug-" + id, OpenedAt: time.Now()}); err != nil {
			return err
		}
		for i := 0; i < toolsPer; i++ {
			tid := fmt.Sprintf("t%03d", i)
			if err := g.old.Tools.Add(ctx, &toollibrary.Tool{LibraryID: id, ToolID: tid, Name: "Tool " + tid, Category: categories[i%len(categories)],
				SerialNumber: id + "-SN-" + tid, AddedAt: time.Now()}); err != nil {
				return fmt.Errorf("%s/%s: %w", id, tid, err)
			}
		}
		return nil
	})
	t.Logf("seeded %d tools in %s", libraries*toolsPer, time.Since(start).Round(time.Second))

	// A conversion of our own labels each tool with its serial number: a claim per tool.
	toollibrary.MigrateTool = func(o toollibrary.ToolG2) (toollibrary.Tool, error) {
		tool := toollibrary.AutoMigrateTool(o)
		tool.Barcodes = []string{"LBL-" + o.SerialNumber}
		return tool, nil
	}
	defer func() { toollibrary.MigrateTool = nil }()
	run := func(command string) {
		t.Helper()
		m := toollibrary.NewMigration(g.db, g.base)
		m.Workers, m.Out = workers, &bytes.Buffer{}
		start := time.Now()
		if err := m.Run(ctx, command); err != nil {
			t.Fatalf("%s: %v\n%s", command, err, m.Out)
		}
		t.Logf("%s took %s", command, time.Since(start).Round(time.Second))
	}

	run("copy")
	checkCounts(t, g.new, libraries, func(int) toollibrary.ToolCounts { return toollibrary.ToolCounts{Available: toolsPer} })
	for _, l := range []int{0, 250, libraries - 1} {
		id := lib(l)
		if tool, err := g.new.Tools.GetByBarcode(ctx, id, "LBL-"+id+"-SN-t042"); err != nil || tool.ToolID != "t042" {
			t.Fatalf("barcode claim for %s: %+v %v", id, tool, err)
		}
	}

	// The old generation keeps serving: every tenth library retires ten tools and adds one.
	changed := func(l int) bool { return l%10 == 0 }
	parallel(t, libraries, 32, func(l int) error {
		if !changed(l) {
			return nil
		}
		id := lib(l)
		for i := 0; i < 10; i++ {
			if err := g.old.Tools.Retire(ctx, toollibrary.ToolKey{LibraryID: id, ToolID: fmt.Sprintf("t%03d", i)}); err != nil {
				return err
			}
		}
		return g.old.Tools.Add(ctx, &toollibrary.Tool{LibraryID: id, ToolID: "new", Name: "New tool", Category: toollibrary.ToolCategoryHand,
			SerialNumber: id + "-SN-new", AddedAt: time.Now()})
	})
	run("copy")
	run("finish")
	checkCounts(t, g.new, libraries, func(l int) toollibrary.ToolCounts {
		if changed(l) {
			return toollibrary.ToolCounts{Available: toolsPer - 10 + 1, Retired: 10}
		}
		return toollibrary.ToolCounts{Available: toolsPer}
	})
	if tool, err := g.new.Tools.GetByBarcode(ctx, lib(10), "LBL-"+lib(10)+"-SN-new"); err != nil || tool.ToolID != "new" {
		t.Fatalf("tool added between passes: %+v %v", tool, err)
	}
}

// checkCounts reads every library's tool counts from the new generation: the migration rebuilds
// counters from the entities it copies.
func checkCounts(t *testing.T, st *toollibrary.Store, libraries int, want func(int) toollibrary.ToolCounts) {
	t.Helper()
	parallel(t, libraries, 32, func(l int) error {
		id := fmt.Sprintf("lib%03d", l)
		got, err := st.Libraries.ToolStats(ctx, toollibrary.ToolCountsKey{LibraryID: id})
		if err != nil {
			return err
		}
		if got != want(l) {
			return fmt.Errorf("%s: tool counts %+v, want %+v", id, got, want(l))
		}
		return nil
	})
}

// parallel runs fn(0..n-1) on up to workers goroutines and fails the test with every error.
func parallel(t *testing.T, n, workers int, fn func(int) error) {
	t.Helper()
	jobs := make(chan int)
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := fn(i); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatal(errors.Join(errs[:min(len(errs), 10)]...))
	}
}
