package schema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Ordered is a YAML mapping that keeps the author's key order, so generated code and docs follow
// the schema file rather than map iteration order.
type Ordered[T any] []Entry[T]

// Entry is one key/value pair of an Ordered mapping.
type Entry[T any] struct {
	Key   string
	Value T
	Line  int
}

// UnmarshalYAML decodes a mapping node, rejecting duplicate keys.
func (o *Ordered[T]) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping", n.Line)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if seen[k.Value] {
			return fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
		}
		seen[k.Value] = true
		var val T
		if err := v.Decode(&val); err != nil {
			return fmt.Errorf("%s: %w", k.Value, err)
		}
		*o = append(*o, Entry[T]{Key: k.Value, Value: val, Line: k.Line})
	}
	return nil
}

// RawFile is the schema file as written.
type RawFile struct {
	Dynago   int                `yaml:"dynago"`
	Package  string             `yaml:"package"`
	Table    RawTable           `yaml:"table"`
	Output   RawOutput          `yaml:"output"`
	Workload RawWorkload        `yaml:"workload"`
	Entities Ordered[RawEntity] `yaml:"entities"`
}

// RawWorkload holds table-wide workload assumptions for the analysis.
type RawWorkload struct {
	// Peak is the ratio of peak to average traffic, applied to every declared rate.
	Peak float64 `yaml:"peak"`
	// Horizon says when the declared volumes are expected, for the reader ("3 years").
	Horizon string `yaml:"horizon"`
}

// RawOutput overrides where generated files are written, relative to the schema file.
type RawOutput struct {
	Go        string `yaml:"go"`
	Docs      string `yaml:"docs"`
	Terraform string `yaml:"terraform"`
	TableJSON string `yaml:"table_json"`
	Lock      string `yaml:"lock"`
	// MigrateCmd is the directory of the generated migration job's main package, if wanted.
	MigrateCmd string `yaml:"migrate_cmd"`
}

// RawTable describes the physical table.
type RawTable struct {
	Name         string `yaml:"name"`
	Doc          string `yaml:"doc"`
	TTLAttribute string `yaml:"ttl_attribute"`
	// Generation numbers the physical table: a change existing items don't fit starts a new one.
	Generation int `yaml:"generation"`
	// Retain lists older generations whose tables are kept (declared in Terraform) for rollback.
	Retain []int `yaml:"retain"`
	// Accept records findings about the table as deliberate: rule id → reason.
	Accept Ordered[string] `yaml:"accept"`
}

// RawEntity describes one entity and everything derived from it.
type RawEntity struct {
	Doc      string              `yaml:"doc"`
	Version  int                 `yaml:"version"`
	Fields   Ordered[RawField]   `yaml:"fields"`
	Key      RawKey              `yaml:"key"`
	TTL      string              `yaml:"ttl"`
	Indexes  Ordered[RawIndex]   `yaml:"indexes"`
	Unique   Ordered[RawUnique]  `yaml:"unique"`
	Counters Ordered[RawCounter] `yaml:"counters"`
	Access   Ordered[RawAccess]  `yaml:"access"`
	Writes   Ordered[RawWrite]   `yaml:"writes"`
	Volume   RawVolume           `yaml:"volume"`
	Accept   Ordered[string]     `yaml:"accept"`
}

// RawVolume is how many items of an entity to expect: a total (`volume: 2000`), or a number per
// parent entity (`volume: { typical: 20, max: 40000 }`).
type RawVolume struct {
	// Total is the short form: the number of items in the table.
	Total *float64 `yaml:"-"`
	// Per names the parent the numbers are per; inferred from the partition key when absent.
	Per string `yaml:"per"`
	// Via names the field linking to Per, when the item holds Per's key more than one way.
	Via     string   `yaml:"via"`
	Typical *float64 `yaml:"typical"`
	Max     *float64 `yaml:"max"`
	// By says how the items spread over other entities they refer to.
	By  Ordered[RawVolumeBy] `yaml:"by"`
	Set bool                 `yaml:"-"`
}

// UnmarshalYAML accepts a number (the total) or a mapping.
func (v *RawVolume) UnmarshalYAML(n *yaml.Node) error {
	v.Set = true
	if n.Kind == yaml.ScalarNode {
		var total float64
		if err := n.Decode(&total); err != nil {
			return fmt.Errorf("line %d: volume must be a number of items or {typical, max}", n.Line)
		}
		v.Total = &total
		return nil
	}
	type plain RawVolume
	if err := strictDecode(n, (*plain)(v)); err != nil {
		return err
	}
	v.Set = true
	return nil
}

// RawVolumeBy is how many items of an entity each item of another entity has.
type RawVolumeBy struct {
	Typical *float64 `yaml:"typical"`
	Max     *float64 `yaml:"max"`
	// Via names the field linking to the entity, when the item holds its key more than one way.
	Via string `yaml:"via"`
}

// UnmarshalYAML decodes a RawVolumeBy, rejecting unknown keys.
func (v *RawVolumeBy) UnmarshalYAML(n *yaml.Node) error {
	type plain RawVolumeBy
	return strictDecode(n, (*plain)(v))
}

// UnmarshalYAML decodes a RawWorkload, rejecting unknown keys.
func (w *RawWorkload) UnmarshalYAML(n *yaml.Node) error {
	type plain RawWorkload
	return strictDecode(n, (*plain)(w))
}

// RawKey is a pair of key templates.
type RawKey struct {
	PK string `yaml:"pk"`
	SK string `yaml:"sk"`
}

// RawField is a field declaration. The short form is just the type: `name: string`.
type RawField struct {
	Type    string   `yaml:"type"`
	Attr    string   `yaml:"attr"`
	Doc     string   `yaml:"doc"`
	Size    string   `yaml:"size"`
	Example string   `yaml:"example"`
	Values  []string `yaml:"values"`
	// CopyOf declares a copy of another entity's field that must stay equal to it.
	CopyOf string `yaml:"copy_of"`
	// SnapshotOf declares a copy of another entity's field taken when this item is written, and
	// deliberately never updated.
	SnapshotOf string `yaml:"snapshot_of"`
	// Ref names the entity whose key this field holds.
	Ref string `yaml:"ref"`
	// Required rejects the zero value on every write that sets the field.
	Required bool            `yaml:"required"`
	Accept   Ordered[string] `yaml:"accept"`
}

// UnmarshalYAML accepts the scalar short form.
func (f *RawField) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		f.Type = n.Value
		return nil
	}
	type plain RawField
	return strictDecode(n, (*plain)(f))
}

// RawIndex declares an alternative way to reach an entity.
type RawIndex struct {
	Strategy string          `yaml:"strategy"`
	PK       string          `yaml:"pk"`
	SK       string          `yaml:"sk"`
	Project  RawProject      `yaml:"project"`
	Where    Ordered[any]    `yaml:"where"`
	Matches  *float64        `yaml:"matches"`
	Doc      string          `yaml:"doc"`
	Accept   Ordered[string] `yaml:"accept"`
}

// RawProject is "all", "keys", or a list of fields.
type RawProject struct {
	Mode   string
	Fields []string
	Set    bool
}

// UnmarshalYAML accepts a scalar mode or a list of field names.
func (p *RawProject) UnmarshalYAML(n *yaml.Node) error {
	p.Set = true
	switch n.Kind {
	case yaml.ScalarNode:
		p.Mode = n.Value
		return nil
	case yaml.SequenceNode:
		p.Mode = "include"
		return n.Decode(&p.Fields)
	}
	return fmt.Errorf("line %d: project must be all, keys, or a list of fields", n.Line)
}

// RawUnique declares a uniqueness constraint enforced by a claim item.
type RawUnique struct {
	Fields []string        `yaml:"fields"`
	PK     string          `yaml:"pk"`
	SK     string          `yaml:"sk"`
	Doc    string          `yaml:"doc"`
	Accept Ordered[string] `yaml:"accept"`
}

// RawCounter declares an atomic counter item maintained from the entity's writes.
type RawCounter struct {
	PK     string                   `yaml:"pk"`
	SK     string                   `yaml:"sk"`
	Shards int                      `yaml:"shards"`
	Doc    string                   `yaml:"doc"`
	Values Ordered[RawCounterValue] `yaml:"values"`
	Accept Ordered[string]          `yaml:"accept"`
}

// RawCounterValue is one attribute of a counter item. The short form is `name: count`.
type RawCounterValue struct {
	Count bool         `yaml:"count"`
	Sum   string       `yaml:"sum"`
	Where Ordered[any] `yaml:"where"`
	Limit any          `yaml:"limit"`
	Min   *int         `yaml:"min"`
	Doc   string       `yaml:"doc"`
}

// UnmarshalYAML accepts the scalar short form "count".
func (v *RawCounterValue) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "count" {
			return fmt.Errorf("line %d: counter value must be \"count\" or a mapping", n.Line)
		}
		v.Count = true
		return nil
	}
	type plain RawCounterValue
	return strictDecode(n, (*plain)(v))
}

// RawAccess declares a read. Short forms: `Get: get`.
type RawAccess struct {
	Get *RawGet `yaml:"get"`
	// Batch, with get: key, makes the read take several keys: the typical number per call.
	Batch any    `yaml:"batch"`
	Query string `yaml:"query"`
	// Of, with query: partition, lists the entities of the partition the read returns.
	Of      []string `yaml:"of"`
	Counter string   `yaml:"counter"`
	// All, with counter, reads every item of the counter in one partition with a Query.
	All bool `yaml:"all"`
	// Scan reads every item of the entity, a page at a time: a declared exception, with a reason.
	Scan   bool   `yaml:"scan"`
	Reason string `yaml:"reason"`
	// Freshness is what the reader needs: immediate (read-your-writes) or eventual.
	Freshness string `yaml:"freshness"`
	Order     string `yaml:"order"`
	Page      int    `yaml:"page"`
	MaxPage   int    `yaml:"max_page"`
	Range     string `yaml:"range"`
	// Consistent is nil when not given: copy queries then default to consistent reads.
	Consistent *bool           `yaml:"consistent"`
	Project    RawProject      `yaml:"project"`
	Doc        string          `yaml:"doc"`
	Rate       float64         `yaml:"rate"`
	Accept     Ordered[string] `yaml:"accept"`
}

// RawGet is `key` or `{unique: Name}`.
type RawGet struct {
	Key    bool
	Unique string
}

// UnmarshalYAML accepts "key" or a mapping with unique.
func (g *RawGet) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "key" {
			return fmt.Errorf("line %d: get must be \"key\" or {unique: Name}", n.Line)
		}
		g.Key = true
		return nil
	}
	var m struct {
		Unique string `yaml:"unique"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	if m.Unique == "" {
		return fmt.Errorf("line %d: get must be \"key\" or {unique: Name}", n.Line)
	}
	g.Unique = m.Unique
	return nil
}

// UnmarshalYAML accepts the scalar short form "get".
func (a *RawAccess) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "get" {
			return fmt.Errorf("line %d: access short form must be \"get\"", n.Line)
		}
		a.Get = &RawGet{Key: true}
		return nil
	}
	type plain RawAccess
	return strictDecode(n, (*plain)(a))
}

// RawWrite declares a write. Short forms: `Create: create`, `Delete: delete`.
type RawWrite struct {
	Create   bool                `yaml:"create"`
	Delete   bool                `yaml:"delete"`
	Update   []string            `yaml:"update"`
	Patch    []string            `yaml:"patch"`
	Set      Ordered[any]        `yaml:"set"`
	When     Ordered[any]        `yaml:"when"`
	Requires Ordered[RawRequire] `yaml:"requires"`
	// Batch makes an update take several keys: the typical number per call.
	Batch      any             `yaml:"batch"`
	Versioned  string          `yaml:"versioned"`
	Doc        string          `yaml:"doc"`
	Rate       float64         `yaml:"rate"`
	HotKeyRate float64         `yaml:"hot_key_rate"`
	Accept     Ordered[string] `yaml:"accept"`
	isUpdate   bool
}

// UnmarshalYAML accepts "create" and "delete" short forms.
func (w *RawWrite) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		switch strings.TrimSpace(n.Value) {
		case "create":
			w.Create = true
		case "delete":
			w.Delete = true
		default:
			return fmt.Errorf("line %d: write short form must be create or delete", n.Line)
		}
		return nil
	}
	type plain RawWrite
	if err := strictDecode(n, (*plain)(w)); err != nil {
		return err
	}
	// An update is declared by what it changes; `set` alone also makes one, unless the write is
	// a create (which may set constants too).
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k, v := n.Content[i].Value, n.Content[i+1].Value; k {
		case "create", "delete":
			if v != "true" {
				return fmt.Errorf("line %d: %s: %s is not meaningful; leave the key out", n.Content[i].Line, k, v)
			}
		case "update", "patch":
			w.isUpdate = true
		case "set":
			w.isUpdate = w.isUpdate || (!w.Create && !w.Delete)
		}
	}
	return nil
}

// strictDecode decodes a mapping into a struct, rejecting keys the struct does not declare so a
// typo in a schema file is an error rather than a silently ignored setting.
func strictDecode(n *yaml.Node, out any) error {
	if n.Kind == yaml.MappingNode {
		allowed := yamlKeys(reflect.TypeOf(out).Elem())
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if !allowed[k.Value] {
				hint := ""
				v := n.Content[i+1]
				bareKey := n.Style&yaml.FlowStyle != 0 && v.Tag == "!!null" && v.Value == ""
				if bareKey || strings.Contains(k.Value, " ") {
					hint = `; if this is the rest of a text value, quote the value: a comma ends it inside { }`
				}
				return fmt.Errorf("line %d: unknown key %q (expected one of: %s)%s", k.Line, k.Value, strings.Join(sortedKeys(allowed), ", "), hint)
			}
		}
	}
	return n.Decode(out)
}

func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		keys[name] = true
	}
	return keys
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Strict decoding for the struct-only types.

// UnmarshalYAML decodes a RawFile, rejecting unknown keys.
func (f *RawFile) UnmarshalYAML(n *yaml.Node) error {
	type plain RawFile
	return strictDecode(n, (*plain)(f))
}

// UnmarshalYAML decodes a RawOutput, rejecting unknown keys.
func (o *RawOutput) UnmarshalYAML(n *yaml.Node) error {
	type plain RawOutput
	return strictDecode(n, (*plain)(o))
}

// UnmarshalYAML decodes a RawTable, rejecting unknown keys.
func (t *RawTable) UnmarshalYAML(n *yaml.Node) error {
	type plain RawTable
	return strictDecode(n, (*plain)(t))
}

// UnmarshalYAML decodes a RawEntity, rejecting unknown keys.
func (e *RawEntity) UnmarshalYAML(n *yaml.Node) error {
	type plain RawEntity
	return strictDecode(n, (*plain)(e))
}

// UnmarshalYAML decodes a RawKey, rejecting unknown keys.
func (k *RawKey) UnmarshalYAML(n *yaml.Node) error {
	type plain RawKey
	return strictDecode(n, (*plain)(k))
}

// UnmarshalYAML decodes a RawIndex, rejecting unknown keys.
func (i *RawIndex) UnmarshalYAML(n *yaml.Node) error {
	type plain RawIndex
	return strictDecode(n, (*plain)(i))
}

// UnmarshalYAML decodes a RawUnique, rejecting unknown keys.
func (u *RawUnique) UnmarshalYAML(n *yaml.Node) error {
	type plain RawUnique
	return strictDecode(n, (*plain)(u))
}

// UnmarshalYAML decodes a RawCounter, rejecting unknown keys.
func (c *RawCounter) UnmarshalYAML(n *yaml.Node) error {
	type plain RawCounter
	return strictDecode(n, (*plain)(c))
}

// RawRequire is a condition on another entity checked in the same transaction as a write.
type RawRequire struct {
	Key  Ordered[string] `yaml:"key"`
	When Ordered[any]    `yaml:"when"`
	// Set changes the required item in the same transaction.
	Set Ordered[any] `yaml:"set"`
	// Ensure creates the required item when it is absent, with these fields (and set applied).
	Ensure *Ordered[any] `yaml:"ensure"`
	// Add adds to int fields of the required item.
	Add Ordered[any] `yaml:"add"`
	// Patch sets fields of the required item from this entity's fields, where those have a value.
	Patch Ordered[string] `yaml:"patch"`
	// Optional lets the write go ahead when the item is absent (or expired); when only applies if
	// it is there.
	Optional bool `yaml:"optional"`
	// Consume deletes the required item in the same transaction.
	Consume bool `yaml:"consume"`
}

// UnmarshalYAML decodes a RawRequire, rejecting unknown keys.
func (r *RawRequire) UnmarshalYAML(n *yaml.Node) error {
	type plain RawRequire
	return strictDecode(n, (*plain)(r))
}
