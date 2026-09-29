package docs

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/lock"
	"github.com/nicklanng/dynago/internal/schema"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestGolden(t *testing.T) {
	m, err := schema.Load(filepath.Join("..", "testdata", "golden.dynago.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lock.Apply(m, &lock.File{Dynago: 1, Entities: map[string]*lock.History{}}, lock.Options{}); err != nil {
		t.Fatal(err)
	}
	got := Generate(m, cost.Analyze(m, cost.DefaultPrices), "golden.dynago.yaml")
	path := filepath.Join("..", "testdata", "golden.model.md")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden.model.md differs from the generated output; review it and run go test -update")
	}
}
