package infra

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicklanng/dynago/internal/schema"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("..", "testdata", name)
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
		t.Errorf("%s differs from the generated output; review it and run go test -update:\n%s", name, got)
	}
}

func TestGolden(t *testing.T) {
	m, err := schema.Load(filepath.Join("..", "testdata", "golden.dynago.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tf, err := Terraform(m, "golden.dynago.yaml")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "golden.tf.json", tf)
	table, err := CreateTableJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "golden.table.json", table)
}

func TestResourceName(t *testing.T) {
	for in, want := range map[string]string{"orders": "orders", "My-Table.v2": "my_table_v2", "2shop": "table_2shop", "--": "table_"} {
		if got := resourceName(in); got != want {
			t.Errorf("resourceName(%q) = %q, want %q", in, got, want)
		}
	}
}
