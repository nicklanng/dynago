package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExamplesAreUpToDate fails when a change to the generator or an example schema was not
// followed by `go generate ./examples/...`.
func TestExamplesAreUpToDate(t *testing.T) {
	schemas, _ := filepath.Glob("../../examples/*/*.dynago.yaml")
	fixtures, _ := filepath.Glob("../../internal/e2e/fixture/*.dynago.yaml")
	schemas = append(schemas, fixtures...)
	if len(schemas) == 0 {
		t.Fatal("no example schemas")
	}
	var out, errs bytes.Buffer
	if code := run(append([]string{"generate", "-check"}, schemas...), &out, &errs); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errs.String())
	}
}

func TestCheckReport(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"check", "../../examples/toollibrary/toollibrary.dynago.yaml"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{"Loan (v1)", "Borrow", "tx 8 items", "estimated total", "copies Tool.name"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestGenerateWritesFilesAndRefusesBadDesigns(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "notes.dynago.yaml")
	writeFile(t, good, []byte(`
dynago: 1
package: notes
table: { name: notes }
entities:
  Note:
    fields:
      id: string
      body: string
    key: { pk: "NOTE#{id}", sk: "NOTE" }
    access:
      Get: get
    writes:
      Create: create
`), 0o644)
	var out, errs bytes.Buffer
	if code := run([]string{"generate", good}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, f := range []string{"notes_dynago.go", "notes.model.md", "notes.tf.json", "notes.table.json", "notes.dynago.lock"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	// Changing the stored shape without bumping the version is refused.
	src, _ := os.ReadFile(good)
	writeFile(t, good, bytes.Replace(src, []byte("      body: string\n"), []byte("      body: string\n      title: string\n"), 1), 0o644)
	errs.Reset()
	if code := run([]string{"generate", good}, &out, &errs); code == 0 || !strings.Contains(errs.String(), "version: 2") {
		t.Fatalf("want a version bump error, got %d: %s", code, errs.String())
	}

	bad := filepath.Join(dir, "bad.dynago.yaml")
	writeFile(t, bad, []byte("dynago: 1\npackage: bad\ntable: { name: bad }\nentities: {}\n"), 0o644)
	errs.Reset()
	if code := run([]string{"generate", bad}, &out, &errs); code == 0 {
		t.Fatal("invalid schema accepted")
	}
}

func writeFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatal(err)
	}
}

func TestFlagsAfterFiles(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"check", "../../examples/toollibrary/toollibrary.dynago.yaml", "-prices", "1,1,1"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if strings.Contains(errs.String(), "-prices") || !strings.Contains(out.String(), "Loan (v1)") {
		t.Fatalf("flag after the file was not taken as a flag:\n%s%s", out.String(), errs.String())
	}
}
