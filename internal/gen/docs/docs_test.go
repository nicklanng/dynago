package docs

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicklanng/dynago/internal/analysis"
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
	got := Generate(m, analysis.Analyze(m, cost.DefaultPrices, nil), "golden.dynago.yaml")
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

// An entity's acceptance that covers several findings is one row, with its reason once.
func TestAcceptedForAWholeEntityIsOneRow(t *testing.T) {
	m, err := schema.Parse([]byte(`
dynago: 1
package: things
table: { name: things }
entities:
  Thing:
    fields:
      thingId: string
      name: string
      body: string
    key: { pk: "THING#{thingId}", sk: "THING" }
    writes:
      Add: create
    volume: 100
  Note:
    accept: { copy-drift: "One pass rewrites them." }
    fields:
      thingId: string
      noteId: string
      thingName: { type: string, copy_of: Thing.name }
      thingBody: { type: string, copy_of: Thing.body }
    key: { pk: "THING#{thingId}", sk: "NOTE#{noteId}" }
    writes:
      Add: create
    volume: { typical: 5, max: 50 }
`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(Generate(m, analysis.Analyze(m, cost.DefaultPrices, nil), "things.dynago.yaml"))
	if n := strings.Count(got, "One pass rewrites them."); n != 1 {
		t.Errorf("the reason appears %d times, want once", n)
	}
	for _, want := range []string{"| `copy-drift` | entity Note | **field Note.thingName** copies Thing.name", "<br>**field Note.thingBody** copies Thing.body"} {
		if !strings.Contains(got, want) {
			t.Errorf("the model document lacks %q", want)
		}
	}
}
