package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExamplesAreUpToDate fails when a change to the generator or an example schema was not
// followed by `go generate ./examples/...`.
func TestExamplesAreUpToDate(t *testing.T) {
	schemas, _ := filepath.Glob("../../examples/*/*.dynago.yaml")
	fixtures, _ := filepath.Glob("../../internal/e2e/*/*.dynago.yaml")
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
	for _, want := range []string{"Loan (v2)", "Borrow", "tx 8 items", "estimated total", "[large-field] entity Tool", "partitions", "LIB#{libraryId}#TOOL#{toolId}", "accepted [sparse-index]"} {
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
	if strings.Contains(errs.String(), "-prices") || !strings.Contains(out.String(), "Loan (v2)") {
		t.Fatalf("flag after the file was not taken as a flag:\n%s%s", out.String(), errs.String())
	}
}

const notes = `
dynago: 1
package: notes
table: { name: notes }
entities:
  Note:
    fields:
      id: string
      body: string
      tag: string
    key: { pk: "NOTE#{id}", sk: "NOTE" }
    access:
      Get: get
    writes:
      Create: create
`

func TestDiff(t *testing.T) {
	dir := t.TempDir()
	old, next := filepath.Join(dir, "old.dynago.yaml"), filepath.Join(dir, "notes.dynago.yaml")
	writeFile(t, old, []byte(notes), 0o644)
	writeFile(t, next, []byte(notes), 0o644)
	var out, errs bytes.Buffer
	if code := run([]string{"diff", "-from", old, next}, &out, &errs); code != 0 || !strings.Contains(out.String(), "no architectural changes") {
		t.Fatalf("exit %d: %s%s", code, out.String(), errs.String())
	}
	writeFile(t, next, []byte(strings.Replace(notes, "      Get: get\n", "      Get: get\n      All: { scan: true, reason: the weekly export }\n", 1)), 0o644)
	out.Reset()
	if code := run([]string{"diff", next, "-from", old}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{"### `notes`: architecture changes", "**+** `Note.All`: Scan", "**+** note `scan`, access Note.All"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("diff lacks %q:\n%s", want, out.String())
		}
	}
}

// A policy next to the schema decides which findings fail the build.
func TestPolicyFailsGenerate(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "notes.dynago.yaml")
	writeFile(t, schemaPath, []byte(strings.Replace(notes, "    access:\n", "    indexes:\n      ByTag: { pk: \"TAG#{tag}\", sk: \"N#{id}\", project: keys }\n    access:\n", 1)), 0o644)
	var out, errs bytes.Buffer
	if code := run([]string{"generate", schemaPath}, &out, &errs); code != 0 {
		t.Fatalf("warnings failed the default policy: %s", errs.String())
	}
	writeFile(t, filepath.Join(dir, "dynago.policy.yaml"), []byte("fail_on: warning\n"), 0o644)
	errs.Reset()
	if code := run([]string{"generate", schemaPath}, &out, &errs); code == 0 || !strings.Contains(errs.String(), "which dynago.policy.yaml fails on") || !strings.Contains(errs.String(), "[unused-index]") {
		t.Fatalf("want a policy failure, got %d: %s", code, errs.String())
	}
	errs.Reset()
	if code := run([]string{"check", "-json", schemaPath}, &out, &errs); code == 0 {
		t.Fatal("check -json passed a failing design")
	}
}

func TestCheckJSON(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"check", "-json", "../../examples/toollibrary/toollibrary.dynago.yaml"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var snap struct {
		Table      string `json:"table"`
		Partitions []struct {
			PK string `json:"pk"`
		} `json:"partitions"`
		Findings []struct {
			Rule, Accepted string
		} `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Table != "toollibrary" || len(snap.Partitions) < 5 || len(snap.Findings) != 6 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestVetCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("loads packages with the go command")
	}
	var out, errs bytes.Buffer
	code := run([]string{"vet", "-list", "../../internal/vet/testdata/app"}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "DynamoDB call outside generated code: (*dynamodb.Client).Scan") ||
		!strings.Contains(errs.String(), "the //dynago:raw mark needs a reason") || !strings.Contains(out.String(), "seeding a fixture") {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errs.String())
	}
}

// The policy is found up to the repository's root, never above it, and the model document names it
// relative to the schema, so the document is the same on every checkout.
func TestPolicyDiscovery(t *testing.T) {
	outer := t.TempDir()
	repo := filepath.Join(outer, "repo")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "tables")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	schemaPath := filepath.Join("tables", "notes.dynago.yaml")
	writeFile(t, filepath.Join(repo, schemaPath), []byte(strings.Replace(notes, "    access:\n", "    indexes:\n      ByTag: { pk: \"TAG#{tag}\", sk: \"N#{id}\", project: keys }\n    access:\n", 1)), 0o644)
	// A policy above the repository doesn't apply.
	writeFile(t, filepath.Join(outer, "dynago.policy.yaml"), []byte("fail_on: note\n"), 0o644)
	t.Chdir(repo)
	var out, errs bytes.Buffer
	if code := run([]string{"generate", schemaPath}, &out, &errs); code != 0 {
		t.Fatalf("a policy outside the repository applied: %s", errs.String())
	}
	writeFile(t, filepath.Join(repo, "dynago.policy.yaml"), []byte("rules: { unused-index: note }\n"), 0o644)
	if code := run([]string{"generate", schemaPath}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	doc, err := os.ReadFile(filepath.Join(repo, "tables", "notes.model.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "Checked against the policy in `../dynago.policy.yaml`") {
		t.Errorf("the model document doesn't name the policy relative to the schema:\n%s", doc)
	}
}

// check analyses a schema anywhere: it renders nothing, so it needs no Go module even when the
// schema generates a migration command.
func TestCheckOutsideAModule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.dynago.yaml")
	writeFile(t, path, []byte(strings.Replace(notes, "table: { name: notes }", "table: { name: notes }\noutput: { migrate_cmd: cmd/migrate }", 1)), 0o644)
	var out, errs bytes.Buffer
	if code := run([]string{"check", path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}
