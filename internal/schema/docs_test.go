package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const (
	jsonSchemaPath = "../../schema/dynago.schema.json"
	referencePath  = "../../docs/schema.md"
)

func compileJSONSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	data, err := os.ReadFile(jsonSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("dynago.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("dynago.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// yamlToJSON converts YAML to the value shapes the validator expects.
func yamlToJSON(t *testing.T, src []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(src, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExamplesMatchJSONSchema(t *testing.T) {
	s := compileJSONSchema(t)
	paths, _ := filepath.Glob("../../examples/*/*.dynago.yaml")
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(yamlToJSON(t, src)); err != nil {
			t.Errorf("%s does not match the JSON Schema: %v", p, err)
		}
	}
}

func TestJSONSchemaRejectsMistakes(t *testing.T) {
	s := compileJSONSchema(t)
	for name, extra := range map[string]string{
		"typo":             "    acess: {}\n",
		"bad type":         "      bad: widget\n",
		"enum no values":   "      e: { type: enum }\n",
		"sk no prefix":     "    counters:\n      C: { pk: \"C\", sk: \"{thingId}\", values: { n: count } }\n",
		"two kinds":        "    writes:\n      W: { create: true, delete: true }\n",
		"access no kind":   "    access:\n      A: { order: asc }\n",
		"bad limit":        "    counters:\n      C: { pk: \"C\", sk: \"C\", values: { n: { count: true, limit: lots } } }\n",
		"create versioned": "    writes:\n      W: { create: true, versioned: required }\n",
	} {
		t.Run(name, func(t *testing.T) {
			src := base
			if strings.HasPrefix(extra, "      ") {
				src = strings.Replace(src, "      tags: string_set\n", "      tags: string_set\n"+extra, 1)
			} else {
				src += extra
			}
			if err := s.Validate(yamlToJSON(t, []byte(src))); err == nil {
				t.Errorf("accepted:\n%s", extra)
			}
		})
	}
	if err := s.Validate(yamlToJSON(t, []byte(base))); err != nil {
		t.Errorf("rejected the valid base schema: %v", err)
	}
}

// TestSchemaKeysAreDocumented keeps the parser, the JSON Schema and the reference in step: every
// key the parser accepts must be declared in the JSON Schema and mentioned in docs/schema.md, and
// the JSON Schema must not declare keys the parser rejects.
func TestSchemaKeysAreDocumented(t *testing.T) {
	raw, err := os.ReadFile(jsonSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var js map[string]any
	if err := json.Unmarshal(raw, &js); err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	reference := string(ref)

	cases := []struct {
		typ  any
		path string
	}{
		{RawFile{}, ""},
		{RawTable{}, "table"},
		{RawOutput{}, "output"},
		{RawEntity{}, "entity"},
		{RawKey{}, "key"},
		{RawVolume{}, "volume"},
		{RawVolumeBy{}, "volumeBy"},
		{RawWorkload{}, "workload"},
		{RawField{}, "field"},
		{RawIndex{}, "index"},
		{RawUnique{}, "unique"},
		{RawCounter{}, "counter"},
		{RawCounterValue{}, "counterValue"},
		{RawAccess{}, "access"},
		{RawWrite{}, "write"},
		{RawRequire{}, "require"},
	}
	for _, c := range cases {
		parser := yamlKeys(reflect.TypeOf(c.typ))
		declared := objectProperties(t, js, c.path)
		name := reflect.TypeOf(c.typ).Name()
		for k := range parser {
			if !declared[k] {
				t.Errorf("%s: parser accepts %q but the JSON Schema does not declare it", name, k)
			}
			if !strings.Contains(reference, "`"+k+"`") {
				t.Errorf("%s: key %q is not documented in docs/schema.md", name, k)
			}
		}
		for k := range declared {
			if !parser[k] {
				t.Errorf("%s: the JSON Schema declares %q but the parser rejects it", name, k)
			}
		}
	}
	// Every field type is documented.
	for _, typ := range []FieldType{TypeString, TypeEnum, TypeInt, TypeFloat, TypeBool, TypeTime, TypeBytes, TypeList, TypeStringSet, TypeMap} {
		if !strings.Contains(reference, "| `"+string(typ)+"` |") {
			t.Errorf("field type %s missing from the type table in docs/schema.md", typ)
		}
	}
}

// objectProperties returns the property names of the object form of a $defs entry ("" is the
// root). Definitions with a short scalar form use oneOf; the object branch is the one with
// properties.
func objectProperties(t *testing.T, js map[string]any, def string) map[string]bool {
	t.Helper()
	node := js
	if def != "" {
		defs := js["$defs"].(map[string]any)
		n, ok := defs[def].(map[string]any)
		if !ok {
			t.Fatalf("JSON Schema has no $defs/%s", def)
		}
		node = n
	}
	if _, ok := node["properties"]; !ok {
		for _, branch := range node["oneOf"].([]any) {
			if b, ok := branch.(map[string]any); ok {
				if _, ok := b["properties"]; ok {
					node = b
					break
				}
			}
		}
	}
	props, ok := node["properties"].(map[string]any)
	if !ok {
		t.Fatalf("$defs/%s has no object properties", def)
	}
	out := map[string]bool{}
	for k := range props {
		out[k] = true
	}
	return out
}
