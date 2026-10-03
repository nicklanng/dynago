package schema

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nicklanng/dynago/internal/keytmpl"
)

// Load reads, resolves and validates a schema file.
func Load(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.Path = path
	return m, nil
}

// ParseEarlier resolves a schema written for an earlier dynago, for comparing with the current one:
// keys since replaced are read as their replacements (an entity's estimate: { items: N } as
// volume: N).
func ParseEarlier(data []byte) (*Model, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 1 {
		if entities := mapValue(doc.Content[0], "entities"); entities != nil && entities.Kind == yaml.MappingNode {
			for i := 1; i < len(entities.Content); i += 2 {
				e := entities.Content[i]
				if e.Kind != yaml.MappingNode {
					continue
				}
				for j := 0; j+1 < len(e.Content); j += 2 {
					if e.Content[j].Value != "estimate" || mapValue(e, "volume") != nil {
						continue
					}
					if items := mapValue(e.Content[j+1], "items"); items != nil {
						e.Content[j].Value, e.Content[j+1] = "volume", items
					}
				}
			}
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	return Parse(out)
}

// mapValue returns the value of key in a mapping node, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// Parse resolves and validates schema YAML.
func Parse(data []byte) (*Model, error) {
	var raw RawFile
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	r := &resolver{raw: &raw}
	m := r.resolve()
	if len(r.errs) > 0 {
		return nil, errors.Join(r.errs...)
	}
	return m, nil
}

type resolver struct {
	raw  *RawFile
	m    *Model
	errs []error
	// typeNames guards against generated Go type names colliding.
	typeNames map[string]string
}

func (r *resolver) errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *resolver) claimType(name, owner string) {
	if prev, ok := r.typeNames[name]; ok {
		r.errorf("generated Go type %s for %s collides with %s; rename one of them", name, owner, prev)
		return
	}
	r.typeNames[name] = owner
}

func (r *resolver) resolve() *Model {
	raw := r.raw
	r.typeNames = map[string]string{"Store": "the generated store", "TableSpec": "the table spec",
		"New": "the generated constructor", "EnsureTable": "the generated EnsureTable",
		"Generation": "the generated Generation constant", "TableName": "the generated TableName function"}
	m := &Model{Package: raw.Package}
	r.m = m
	if raw.Dynago != 1 {
		r.errorf("dynago: schema format version must be 1 (got %d)", raw.Dynago)
	}
	if !rePackage.MatchString(raw.Package) {
		r.errorf("package: %q is not a valid Go package name", raw.Package)
	}
	m.Table = Table{Name: raw.Table.Name, Doc: raw.Table.Doc, TTLAttr: raw.Table.TTLAttribute, Generation: raw.Table.Generation, Retain: raw.Table.Retain}
	if m.Table.Generation == 0 {
		m.Table.Generation = 1
	}
	if m.Table.Generation < 0 {
		r.errorf("table.generation must be positive")
	}
	retained := map[int]bool{}
	for _, g := range m.Table.Retain {
		switch {
		case g < 1 || g >= m.Table.Generation:
			r.errorf("table.retain: %d is not an older generation (the current one is %d)", g, m.Table.Generation)
		case retained[g]:
			r.errorf("table.retain: %d is listed twice", g)
		}
		retained[g] = true
	}
	if m.Table.Name == "" {
		r.errorf("table.name is required")
	}
	if m.Table.TTLAttr == "" {
		m.Table.TTLAttr = "ttl"
	}
	r.accepts(Subject{Kind: SubjectTable}, "table", raw.Table.Accept)
	m.Workload = Workload{Peak: 1, Horizon: raw.Workload.Horizon}
	switch p := raw.Workload.Peak; {
	case p < 0 || (p > 0 && p < 1):
		r.errorf("workload.peak: %v is not a peak-to-average ratio; it must be 1 or more", p)
	case p >= 1:
		m.Workload.Peak, m.Workload.PeakDeclared = p, true
	}
	base := fileBase(m.Table.Name)
	m.Output = Output{
		Go:         orDefault(raw.Output.Go, base+"_dynago.go"),
		Docs:       orDefault(raw.Output.Docs, base+".model.md"),
		Terraform:  orDefault(raw.Output.Terraform, base+".tf.json"),
		TableJSON:  orDefault(raw.Output.TableJSON, base+".table.json"),
		Lock:       orDefault(raw.Output.Lock, base+".dynago.lock"),
		MigrateCmd: raw.Output.MigrateCmd,
	}
	if len(raw.Entities) == 0 {
		r.errorf("entities: at least one entity is required")
	}
	for _, re := range raw.Entities {
		if e := r.entity(m, re.Key, re.Value); e != nil {
			m.Entities = append(m.Entities, e)
		}
	}
	r.crossRefs(m)
	r.relations(m)
	r.volumes(m)
	r.gsis(m)
	r.keyspaces(m)
	return m
}

func (r *resolver) entity(m *Model, name string, raw RawEntity) *Entity {
	where := "entity " + name
	if !reExported.MatchString(name) {
		r.errorf("%s: entity names must be PascalCase", where)
		return nil
	}
	e := &Entity{
		Name: name, GoName: name, Doc: raw.Doc, Version: raw.Version,
		fieldsByName: map[string]*Field{}, rawVolume: raw.Volume,
	}
	r.accepts(Subject{Kind: SubjectEntity, Entity: name}, where, raw.Accept)
	r.claimType(e.GoName, where)
	r.claimType(e.GoName+"Key", where)
	if e.Version == 0 {
		e.Version = 1
	}
	if e.Version < 0 {
		r.errorf("%s: version must be positive", where)
	}

	reserved := map[string]bool{AttrPK: true, AttrSK: true, AttrType: true, AttrVer: true, AttrRev: true, AttrCreated: true, AttrUpdated: true, m.Table.TTLAttr: true}
	attrs := map[string]string{}
	for _, rf := range raw.Fields {
		f := r.field(e, rf.Key, rf.Value)
		if f == nil {
			continue
		}
		if reserved[f.Attr] {
			r.errorf("%s: field %s uses the reserved attribute name %q", where, f.Name, f.Attr)
		}
		if prev, ok := attrs[f.Attr]; ok {
			r.errorf("%s: fields %s and %s both use attribute %q", where, prev, f.Name, f.Attr)
		}
		r.accepts(Subject{Kind: SubjectField, Entity: name, Name: f.Name}, where+" field "+f.Name, rf.Value.Accept)
		attrs[f.Attr] = f.Name
		e.Fields = append(e.Fields, f)
		e.fieldsByName[f.Name] = f
	}
	if len(e.Fields) == 0 {
		r.errorf("%s: at least one field is required", where)
		return nil
	}
	goNames := map[string]string{}
	for _, f := range e.Fields {
		if prev, ok := goNames[f.GoName]; ok {
			r.errorf("%s: fields %s and %s have the same Go name %s; rename one", where, prev, f.Name, f.GoName)
		}
		goNames[f.GoName] = f.Name
	}
	r.claimType(e.GoName+"Store", where)
	r.claimType("Store."+Plural(e.GoName), where+" (its field on Store)")
	// Unexported helpers are named after the entity too: "THING" and "Thing" both give thingItem.
	r.claimType(unexported(e.GoName)+"Item", where)

	var ok bool
	if e.PK, ok = r.template(e, where+" key.pk", raw.Key.PK, true); !ok {
		return nil
	}
	if e.SK, ok = r.template(e, where+" key.sk", raw.Key.SK, true); !ok {
		return nil
	}
	if e.SK.LiteralPrefix() == "" {
		r.errorf("%s key.sk: must start with literal text (e.g. %q) so the entity's items can be told apart", where, strings.ToUpper(name)+"#")
	}
	for _, f := range e.KeyFields() {
		f.Key = true
	}

	if raw.TTL != "" {
		f := e.Field(raw.TTL)
		switch {
		case f == nil:
			r.errorf("%s: ttl field %q is not declared", where, raw.TTL)
		case f.Type != TypeTime:
			r.errorf("%s: ttl field %q must be of type time", where, raw.TTL)
		default:
			e.TTL = f
		}
	}

	// Reads that need immediate freshness decide the strategy of an index that doesn't declare one.
	fresh := map[string][]string{}
	for _, ra := range raw.Access {
		if ra.Value.Freshness == string(FreshnessImmediate) && ra.Value.Query != "" && ra.Value.Query != "key" {
			fresh[ra.Value.Query] = append(fresh[ra.Value.Query], ra.Key)
		}
	}
	for _, ri := range raw.Indexes {
		r.accepts(Subject{Kind: SubjectIndex, Entity: name, Name: ri.Key}, where+" index "+ri.Key, ri.Value.Accept)
		if ix := r.index(e, ri.Key, ri.Value, fresh[ri.Key]); ix != nil {
			e.Indexes = append(e.Indexes, ix)
		}
	}
	for _, ru := range raw.Unique {
		r.accepts(Subject{Kind: SubjectUnique, Entity: name, Name: ru.Key}, where+" unique "+ru.Key, ru.Value.Accept)
		if u := r.unique(e, ru.Key, ru.Value); u != nil {
			e.Uniques = append(e.Uniques, u)
		}
	}
	for _, rc := range raw.Counters {
		r.accepts(Subject{Kind: SubjectCounter, Entity: name, Name: rc.Key}, where+" counter "+rc.Key, rc.Value.Accept)
		if c := r.counter(e, rc.Key, rc.Value); c != nil {
			e.Counters = append(e.Counters, c)
		}
	}
	methods := map[string]bool{}
	for _, ra := range raw.Access {
		if methods[ra.Key] {
			r.errorf("%s: %s is declared more than once across access and writes", where, ra.Key)
		}
		methods[ra.Key] = true
		r.accepts(Subject{Kind: SubjectAccess, Entity: name, Name: ra.Key}, where+" access "+ra.Key, ra.Value.Accept)
		if a := r.access(e, ra.Key, ra.Value); a != nil {
			e.Access = append(e.Access, a)
		}
	}
	for _, rw := range raw.Writes {
		if methods[rw.Key] {
			r.errorf("%s: %s is declared more than once across access and writes", where, rw.Key)
		}
		methods[rw.Key] = true
		r.accepts(Subject{Kind: SubjectWrite, Entity: name, Name: rw.Key}, where+" write "+rw.Key, rw.Value.Accept)
		if w := r.write(e, rw.Key, rw.Value); w != nil {
			e.Writes = append(e.Writes, w)
		}
	}
	return e
}

func (r *resolver) field(e *Entity, name string, raw RawField) *Field {
	where := fmt.Sprintf("entity %s field %s", e.Name, name)
	if !reLower.MatchString(name) {
		r.errorf("%s: field names must be camelCase", where)
		return nil
	}
	f := &Field{Name: name, Attr: orDefault(raw.Attr, name), GoName: GoName(name), Doc: raw.Doc, Example: raw.Example,
		copyOfRaw: raw.CopyOf, snapshotOfRaw: raw.SnapshotOf, refRaw: raw.Ref, Required: raw.Required}
	if raw.CopyOf != "" && raw.SnapshotOf != "" {
		r.errorf("%s: copy_of and snapshot_of are exclusive: the copy is either kept equal to its source or taken once", where)
	}
	switch f.GoName {
	case "Key", "Version", "Timestamps":
		r.errorf("%s: a field named %s would clash with the generated %s() method; rename it", where, name, f.GoName)
		return nil
	case "PK", "SK", "T", "V", "Rev", "TTL", "DynagoCreated", "DynagoUpdated":
		r.errorf("%s: the Go name %s is used by the generated item struct (for its %s); rename the field", where, f.GoName, map[string]string{
			"PK": "partition key", "SK": "sort key", "T": "type", "V": "schema version", "Rev": "revision", "TTL": "expiry",
			"DynagoCreated": "creation time", "DynagoUpdated": "update time"}[f.GoName])
		return nil
	}
	if !reAttr.MatchString(f.Attr) {
		r.errorf("%s: invalid attribute name %q", where, f.Attr)
	}
	switch t := FieldType(raw.Type); t {
	case TypeString, TypeInt, TypeFloat, TypeBool, TypeTime, TypeBytes, TypeStringSet, TypeList, TypeMap:
		f.Type = t
	case TypeEnum:
		f.Type = t
		if len(raw.Values) == 0 {
			r.errorf("%s: enum needs values", where)
		}
		seen := map[string]bool{}
		goNames := map[string]string{}
		for _, v := range raw.Values {
			if v == "" || seen[v] {
				r.errorf("%s: enum values must be unique and non-empty", where)
			}
			seen[v] = true
			gn := GoName(v)
			switch prev, dup := goNames[gn]; {
			case gn == "":
				r.errorf("%s: enum value %q has no letters or digits, so it has no Go constant name", where, v)
			case dup:
				r.errorf("%s: enum values %q and %q have the same Go name %s; rename one", where, prev, v, gn)
			default:
				goNames[gn] = v
			}
		}
		f.Enum = raw.Values
		r.claimType(e.GoName+f.GoName, where)
		for gn := range goNames {
			r.claimType(e.GoName+f.GoName+gn, where+" value")
		}
	default:
		r.errorf("%s: unknown type %q (want string, enum, int, float, bool, time, bytes, string_list, string_set or string_map)", where, raw.Type)
		return nil
	}
	if raw.Values != nil && f.Type != TypeEnum {
		r.errorf("%s: values is only valid for enum fields", where)
	}
	p50, p99 := defaultSize(f)
	if raw.Size != "" {
		var err error
		p50, p99, err = parseSize(raw.Size)
		if err != nil {
			r.errorf("%s: size: %v", where, err)
		}
	}
	f.SizeP50, f.SizeP99 = p50, p99
	return f
}

func defaultSize(f *Field) (int, int) {
	switch f.Type {
	case TypeEnum:
		n := 0
		for _, v := range f.Enum {
			n = max(n, len(v))
		}
		return n, n
	case TypeInt, TypeFloat:
		return 8, 11
	case TypeBool:
		return 1, 1
	case TypeTime:
		return 30, 35
	case TypeBytes:
		return 200, 2000
	case TypeStringSet, TypeList:
		return 60, 400
	case TypeMap:
		return 120, 800
	}
	return 20, 64
}

func parseSize(s string) (int, int, error) {
	a, b, found := strings.Cut(s, "/")
	p50, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil || p50 < 0 {
		return 0, 0, fmt.Errorf("want bytes or p50/p99 bytes, got %q", s)
	}
	if !found {
		return p50, p50, nil
	}
	p99, err := strconv.Atoi(strings.TrimSpace(b))
	if err != nil || p99 < p50 {
		return 0, 0, fmt.Errorf("want p50/p99 with p99 >= p50, got %q", s)
	}
	return p50, p99, nil
}

// template parses a key template and resolves its fields against the entity. Fields in multi are
// allowed although they are sets: a unique constraint renders one key per element.
func (r *resolver) template(e *Entity, where, raw string, required bool, multi ...*Field) (Template, bool) {
	if raw == "" {
		if required {
			r.errorf("%s: required", where)
		}
		return Template{}, false
	}
	kt, err := keytmpl.Parse(raw)
	if err != nil {
		r.errorf("%s: %v", where, err)
		return Template{}, false
	}
	t := Template{Template: kt}
	ok := true
	for _, sg := range kt.Segments {
		if sg.Transform == "" {
			continue
		}
		if f := e.Field(sg.Field); f != nil && f.Type != TypeString && f.Type != TypeEnum && f.Type != TypeStringSet {
			r.errorf("%s: {%s|%s}: transforms apply to string and enum fields; %s is a %s", where, sg.Field, sg.Transform, sg.Field, f.Type)
			ok = false
		}
	}
	for _, name := range kt.Fields() {
		f := e.Field(name)
		if f == nil {
			r.errorf("%s: {%s} is not a field of %s", where, name, e.Name)
			ok = false
			continue
		}
		if !f.KeyCapable() && !slices.Contains(multi, f) {
			r.errorf("%s: field %s has type %s, which cannot be part of a key", where, name, f.Type)
			ok = false
			continue
		}
		t.Fields = append(t.Fields, f)
	}
	return t, ok
}

// setFields returns the string_set fields of e that the key templates name, in order. Templates
// that don't parse name none: their own resolution reports why.
func setFields(e *Entity, templates ...string) []*Field {
	var out []*Field
	for _, raw := range templates {
		kt, err := keytmpl.Parse(raw)
		if err != nil {
			continue
		}
		for _, name := range kt.Fields() {
			if f := e.Field(name); f != nil && f.Type == TypeStringSet && !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

func (r *resolver) preds(e *Entity, where string, raw Ordered[any]) []*Pred {
	var out []*Pred
	for _, p := range raw {
		f := e.Field(p.Key)
		if f == nil {
			r.errorf("%s: %s is not a field of %s", where, p.Key, e.Name)
			continue
		}
		pr, err := pred(f, p.Value)
		if err != nil {
			r.errorf("%s: %s: %v", where, p.Key, err)
			continue
		}
		out = append(out, pr)
	}
	return out
}

// pred resolves one predicate on field f: a constant (equal to it), `{ not: <constant> }` or
// `{ in: [<constant>, ...] }`.
func pred(f *Field, raw any) (*Pred, error) {
	form, ok := raw.(map[string]any)
	if !ok {
		v, err := coerce(f, raw)
		if err != nil {
			return nil, err
		}
		return &Pred{Field: f, Value: v}, nil
	}
	if len(form) != 1 {
		return nil, fmt.Errorf("want a value, { not: <value> } or { in: [<value>, ...] }")
	}
	for op, arg := range form {
		switch op {
		case "not":
			v, err := coerce(f, arg)
			if err != nil {
				return nil, fmt.Errorf("not: %v", err)
			}
			return &Pred{Field: f, Value: v, Not: true}, nil
		case "in":
			list, ok := arg.([]any)
			if !ok || len(list) == 0 {
				return nil, fmt.Errorf("in: want a list of values")
			}
			pr := &Pred{Field: f, In: []any{}}
			for _, x := range list {
				v, err := coerce(f, x)
				if err != nil {
					return nil, fmt.Errorf("in: %v", err)
				}
				if pr.Matches(v) {
					return nil, fmt.Errorf("in: %v is listed twice", x)
				}
				pr.In = append(pr.In, v)
			}
			if len(pr.In) == 1 {
				return nil, fmt.Errorf("in: one value is an equality; write %s: %v", f.Name, list[0])
			}
			if allowed, listable := pr.Allowed(); listable && len(allowed) == len(domainOf(f)) {
				return nil, fmt.Errorf("in: every value of %s is listed, so the condition always holds; remove it", f.Name)
			}
			return pr, nil
		default:
			return nil, fmt.Errorf("unknown form %q: want a value, { not: <value> } or { in: [<value>, ...] }", op)
		}
	}
	return nil, nil
}

// domainOf lists every value an enum or bool field can hold.
func domainOf(f *Field) []string {
	switch f.Type {
	case TypeEnum:
		return f.Enum
	case TypeBool:
		return []string{"false", "true"}
	}
	return nil
}

// coerce checks a YAML scalar against a field type and returns it in canonical Go form.
func coerce(f *Field, v any) (any, error) {
	switch f.Type {
	case TypeBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case TypeInt:
		if i, ok := v.(int); ok {
			return int64(i), nil
		}
	case TypeFloat:
		switch n := v.(type) {
		case float64:
			return n, nil
		case int:
			return float64(n), nil
		}
	case TypeString:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case TypeEnum:
		if s, ok := v.(string); ok {
			for _, ev := range f.Enum {
				if ev == s {
					return s, nil
				}
			}
			return nil, fmt.Errorf("%q is not one of %v", s, f.Enum)
		}
	default:
		return nil, fmt.Errorf("fields of type %s cannot be compared to a constant", f.Type)
	}
	return nil, fmt.Errorf("value %v does not match field type %s", v, f.Type)
}

func (r *resolver) index(e *Entity, name string, raw RawIndex, fresh []string) *Index {
	where := fmt.Sprintf("entity %s index %s", e.Name, name)
	if !reExported.MatchString(name) {
		r.errorf("%s: index names must be PascalCase", where)
		return nil
	}
	ix := &Index{Name: name, GoName: name, Entity: e, Doc: raw.Doc}
	// A key holding a string_set renders one key per element, which only copies can have: a GSI
	// entry is the item itself, under one key.
	sets := setFields(e, raw.PK, raw.SK)
	strategy := raw.Strategy
	if strategy == "" {
		ix.StrategyInferred = true
		strategy = string(StrategyGSI)
		ix.StrategyReason = "no read through it needs immediate freshness"
		if len(sets) > 0 {
			strategy = string(StrategyCopy)
			ix.StrategyReason = fmt.Sprintf("it is keyed by each element of %s, and a GSI holds an item under one key", sets[0].Name)
		} else if len(fresh) > 0 {
			strategy = string(StrategyCopy)
			verb := "needs"
			if len(fresh) > 1 {
				verb = "need"
			}
			ix.StrategyReason = fmt.Sprintf("%s %s immediate freshness", strings.Join(fresh, " and "), verb)
			where += " (a copy, because " + ix.StrategyReason + ")"
		}
	}
	switch Strategy(strategy) {
	case StrategyGSI:
		ix.Strategy = StrategyGSI
		ix.PKAttr, ix.SKAttr = name+"PK", name+"SK"
	case StrategyCopy:
		ix.Strategy = StrategyCopy
		ix.PKAttr, ix.SKAttr = AttrPK, AttrSK
	default:
		r.errorf("%s: strategy must be gsi or copy", where)
		return nil
	}
	switch {
	case len(sets) > 1:
		r.errorf("%s: %s and %s are both sets; an index can be keyed by the elements of one", where, sets[0].Name, sets[1].Name)
		return nil
	case len(sets) == 1 && ix.Strategy == StrategyGSI:
		r.errorf("%s: its keys hold %s, a string_set: the entity would need an entry for each element, and a GSI holds an item under one key. Make it a copy (strategy: copy, or leave strategy out)", where, sets[0].Name)
		return nil
	case len(sets) == 1:
		ix.Set = sets[0]
	}
	var ok bool
	if ix.PK, ok = r.template(e, where+" pk", raw.PK, true, sets...); !ok {
		return nil
	}
	if raw.SK != "" {
		if ix.SK, ok = r.template(e, where+" sk", raw.SK, true, sets...); !ok {
			return nil
		}
		ix.HasSK = true
	}
	if ix.Strategy == StrategyCopy {
		if !ix.HasSK {
			r.errorf("%s: copy indexes need an sk", where)
			return nil
		}
		if ix.SK.LiteralPrefix() == "" {
			r.errorf("%s sk: copy items must start with literal text so they can be told apart", where)
		}
		// Each entity needs its own copy item, or two entities would overwrite (and delete) each
		// other's copy. A transformed field doesn't count: "Bob" and "bob" lower to one key.
		used := map[*Field]bool{}
		for _, t := range []Template{ix.PK, ix.SK} {
			for _, sg := range t.Segments {
				if sg.IsField() && sg.Transform == "" {
					used[e.Field(sg.Field)] = true
				}
			}
		}
		for _, f := range e.KeyFields() {
			if !used[f] {
				r.errorf("%s: a copy's keys must include every primary key field, untransformed, so each %s has its own copy; %s is missing", where, e.Name, f.Name)
			}
		}
	}
	if !raw.Project.Set {
		r.errorf("%s: project is required (all, keys, or a list of fields) — it decides what the index costs", where)
		return nil
	}
	switch raw.Project.Mode {
	case ProjectAll, ProjectKeys:
		ix.Projection = raw.Project.Mode
	case ProjectInclude:
		ix.Projection = ProjectInclude
		for _, fn := range raw.Project.Fields {
			f := e.Field(fn)
			if f == nil {
				r.errorf("%s: projected field %s is not a field of %s", where, fn, e.Name)
				continue
			}
			ix.Project = append(ix.Project, f)
		}
	default:
		r.errorf("%s: project must be all, keys, or a list of fields", where)
	}
	ix.Where = r.preds(e, where+" where", raw.Where)
	if m := raw.Matches; m != nil {
		switch {
		case len(raw.Where) == 0:
			r.errorf("%s: matches is the share of items that satisfy where; this index has no where, so every item is in it", where)
		case *m <= 0 || *m > 1:
			r.errorf("%s: matches is a share of the entity's items: more than 0, at most 1 (0.01 is one in a hundred)", where)
		default:
			ix.Matches = *m
		}
	}
	if !ix.ReturnsEntity() {
		r.claimType(e.GoName+ix.GoName, where)
	}
	return ix
}

func (r *resolver) unique(e *Entity, name string, raw RawUnique) *Unique {
	where := fmt.Sprintf("entity %s unique %s", e.Name, name)
	if !reExported.MatchString(name) {
		r.errorf("%s: unique names must be PascalCase", where)
		return nil
	}
	u := &Unique{Name: name, GoName: name, Entity: e, Doc: raw.Doc,
		ErrName: "Err" + e.GoName + name + "Taken"}
	if len(raw.Fields) == 0 {
		r.errorf("%s: fields is required", where)
		return nil
	}
	for _, fn := range raw.Fields {
		f := e.Field(fn)
		if f == nil {
			r.errorf("%s: %s is not a field of %s", where, fn, e.Name)
			return nil
		}
		switch {
		case f.Type == TypeStringSet && u.Set != nil:
			r.errorf("%s: %s and %s are both sets; a constraint can make one set's elements unique", where, u.Set.Name, fn)
			return nil
		case f.Type == TypeStringSet:
			u.Set = f
		case f.Type == TypeList:
			r.errorf("%s: field %s is a string_list, which can hold duplicates; make it a string_set to make its elements unique", where, fn)
			return nil
		case !f.KeyCapable():
			r.errorf("%s: field %s has type %s, which cannot be unique", where, fn, f.Type)
			return nil
		}
		u.Fields = append(u.Fields, f)
	}
	pk := raw.PK
	if pk == "" {
		var b strings.Builder
		b.WriteString("UNIQUE#" + e.Name + "." + name)
		for _, f := range u.Fields {
			b.WriteString("#{" + f.Name + "}")
		}
		pk = b.String()
	}
	var ok bool
	if u.PK, ok = r.template(e, where+" pk", pk, true, u.Set); !ok {
		return nil
	}
	if u.SK, ok = r.template(e, where+" sk", orDefault(raw.SK, "UNIQUE"), true, u.Set); !ok {
		return nil
	}
	used := map[*Field]bool{}
	for _, f := range mergeFields(u.PK.Fields, u.SK.Fields) {
		used[f] = true
	}
	for _, f := range u.Fields {
		if !used[f] {
			r.errorf("%s: key templates must include every unique field; %s is missing", where, f.Name)
		}
		delete(used, f)
	}
	for f := range used {
		r.errorf("%s: key templates may only use the unique fields; %s is not one", where, f.Name)
	}
	return u
}

func (r *resolver) counter(e *Entity, name string, raw RawCounter) *Counter {
	where := fmt.Sprintf("entity %s counter %s", e.Name, name)
	if !reExported.MatchString(name) {
		r.errorf("%s: counter names must be PascalCase", where)
		return nil
	}
	c := &Counter{Name: name, GoName: name, Entity: e, Doc: raw.Doc, Shards: raw.Shards}
	r.claimType(c.GoName, where)
	r.claimType(c.GoName+"Key", where)
	if c.Shards == 0 {
		c.Shards = 1
	}
	if c.Shards < 1 || c.Shards > 100 {
		r.errorf("%s: shards must be between 1 and 100", where)
	}
	sets := setFields(e, raw.PK, raw.SK)
	switch {
	case len(sets) > 1:
		r.errorf("%s: %s and %s are both sets; a counter can be keyed by the elements of one", where, sets[0].Name, sets[1].Name)
		return nil
	case len(sets) == 1:
		c.Set = sets[0]
	}
	var ok bool
	if c.PK, ok = r.template(e, where+" pk", raw.PK, true, sets...); !ok {
		return nil
	}
	if c.SK, ok = r.template(e, where+" sk", raw.SK, true, sets...); !ok {
		return nil
	}
	if c.SK.LiteralPrefix() == "" {
		r.errorf("%s sk: must start with literal text", where)
	}
	if len(raw.Values) == 0 {
		r.errorf("%s: values is required", where)
	}
	for _, rv := range raw.Values {
		vw := where + " value " + rv.Key
		if !reLower.MatchString(rv.Key) {
			r.errorf("%s: value names must be camelCase", vw)
			continue
		}
		v := &CounterValue{Name: rv.Key, GoName: GoName(rv.Key), Attr: rv.Key, Counter: c, Doc: rv.Value.Doc,
			ErrName: "Err" + c.GoName + GoName(rv.Key) + "Limit"}
		switch {
		case rv.Value.Count && rv.Value.Sum != "":
			r.errorf("%s: use count or sum, not both", vw)
		case rv.Value.Sum != "":
			f := e.Field(rv.Value.Sum)
			if f == nil {
				r.errorf("%s: sum field %s is not a field of %s", vw, rv.Value.Sum, e.Name)
				continue
			}
			if f.Type != TypeInt {
				r.errorf("%s: sum field %s must be an int", vw, f.Name)
			}
			v.Sum = f
		case !rv.Value.Count:
			r.errorf("%s: set count: true or sum: <field>", vw)
		}
		v.Where = r.preds(e, vw+" where", rv.Value.Where)
		switch l := rv.Value.Limit.(type) {
		case nil:
		case int:
			if l <= 0 {
				r.errorf("%s: limit must be positive", vw)
			}
			v.Limit = int64(l)
		case string:
			if l != "arg" {
				r.errorf("%s: limit must be a number or \"arg\"", vw)
			}
			v.LimitArg = true
		default:
			r.errorf("%s: limit must be a number or \"arg\"", vw)
		}
		if rv.Value.Min != nil {
			if *rv.Value.Min < 0 {
				r.errorf("%s: min must be zero or more", vw)
			}
			v.HasMin, v.Min = true, int64(*rv.Value.Min)
			v.MinErrName = "Err" + c.GoName + GoName(rv.Key) + "Min"
		}
		if (v.Limited() || v.HasMin) && c.Shards > 1 {
			r.errorf("%s: a bounded value cannot be sharded (the bound needs one item to check)", vw)
		}
		c.Values = append(c.Values, v)
	}
	return c
}

func (r *resolver) access(e *Entity, name string, raw RawAccess) *Access {
	where := fmt.Sprintf("entity %s access %s", e.Name, name)
	if !reExported.MatchString(name) {
		r.errorf("%s: access names must be PascalCase", where)
		return nil
	}
	a := &Access{Name: name, GoName: name, Entity: e, Doc: raw.Doc, Consistent: raw.Consistent != nil && *raw.Consistent, Rate: raw.Rate, Reason: strings.TrimSpace(raw.Reason)}
	kinds := 0
	if raw.Get != nil {
		kinds++
	}
	if raw.Query != "" {
		kinds++
	}
	if raw.Counter != "" {
		kinds++
	}
	if raw.Scan {
		kinds++
	}
	if kinds != 1 {
		r.errorf("%s: declare exactly one of get, query, counter or scan", where)
		return nil
	}
	switch Freshness(raw.Freshness) {
	case FreshnessUnstated, FreshnessImmediate, FreshnessEventual:
		a.Freshness = Freshness(raw.Freshness)
	default:
		r.errorf("%s: freshness must be immediate or eventual", where)
	}
	if raw.Reason != "" && !raw.Scan {
		r.errorf("%s: reason applies to scans; use doc to describe other reads", where)
	}
	switch {
	case raw.Scan:
		a.Kind = AccessScan
		if a.Reason == "" {
			r.errorf("%s: a scan reads the whole table; give the reason it is needed (reason: ...)", where)
		}
		a.Page, a.MaxPage = r.pageSize(where, raw)
	case raw.Get != nil && raw.Get.Key:
		a.Kind = AccessGet
		if raw.Batch != nil {
			n, ok := raw.Batch.(int)
			if !ok || n < 1 {
				r.errorf("%s: batch is the typical number of keys a call reads (batch: 40), which the estimates use", where)
			} else {
				a.Batch = n
			}
		}
	case raw.Get != nil:
		a.Kind = AccessGetUnique
		for _, u := range e.Uniques {
			if u.Name == raw.Get.Unique {
				a.Unique = u
			}
		}
		if a.Unique == nil {
			r.errorf("%s: %s is not a unique constraint of %s", where, raw.Get.Unique, e.Name)
			return nil
		}
	case raw.Counter != "":
		// Resolved in crossRefs: a counter may be read from any entity of the table, e.g. an
		// event's registration counts from the event.
		a.Kind = AccessCounter
		a.counterRaw = raw.Counter
	default:
		a.Kind = AccessQuery
		if raw.Query == "partition" {
			// Resolved in crossRefs: the read returns several entities' items.
			if len(raw.Of) == 0 {
				r.errorf("%s: query: partition reads every kind of item the partition holds; list the entities it returns (of: [%s, ...])", where, e.Name)
				return nil
			}
			a.ofRaw = raw.Of
			if raw.Range != "" || raw.Project.Set {
				r.errorf("%s: range and project apply to a query of one entity's items; a partition read returns several kinds whole", where)
			}
			r.claimType(e.GoName+a.GoName, where)
		} else if raw.Query != "key" {
			for _, ix := range e.Indexes {
				if ix.Name == raw.Query {
					a.Index = ix
				}
			}
			if a.Index == nil {
				r.errorf("%s: query must be \"key\" or an index of %s; %q is neither", where, e.Name, raw.Query)
				return nil
			}
			if a.Consistent && a.Index.Strategy == StrategyGSI {
				r.errorf("%s: global secondary indexes cannot be read consistently", where)
			}
			if a.Freshness == FreshnessImmediate && a.Index.Strategy == StrategyGSI {
				r.errorf("%s: freshness: immediate needs read-your-writes, but %s is a GSI, which DynamoDB updates a moment after each write. Make it a copy (strategy: copy, or leave strategy out and dynago chooses a copy), or accept eventual freshness", where, a.Index.Name)
			}
			// Read-your-writes is why an index is a copy: read it consistently unless told not to.
			if a.Index.Strategy == StrategyCopy && raw.Consistent == nil && a.Freshness != FreshnessEventual {
				a.Consistent = true
			}
		}
		switch raw.Order {
		case "", "asc":
		case "desc":
			a.Desc = true
		default:
			r.errorf("%s: order must be asc or desc", where)
		}
		a.Page, a.MaxPage = r.pageSize(where, raw)
		if sk, ok := a.QuerySK(); raw.Range != "" {
			f := e.Field(raw.Range)
			switch {
			case f == nil:
				r.errorf("%s: range field %s is not a field of %s", where, raw.Range, e.Name)
			case !f.KeyCapable():
				r.errorf("%s: range field %s is a %s; a range bounds a single value", where, f.Name, f.Type)
			case !ok || rangeSegment(sk) != f.Name:
				r.errorf("%s: range field %s must be the first placeholder of the sort key, directly after its literal prefix", where, f.Name)
			default:
				a.Range = f
			}
		}
		if a.Range != nil {
			for _, f := range a.QueryPK().Fields {
				if f.GoName == "From" || f.GoName == "To" {
					r.errorf("%s: partition key field %s collides with the range bound %s in the generated %s%sQuery; rename the field", where, f.Name, f.GoName, e.GoName, a.GoName)
				}
			}
		}
		r.claimType(e.GoName+a.GoName+"Query", where)
		if raw.Project.Set {
			switch {
			case a.Index != nil:
				r.errorf("%s: project applies to queries on the entity's own partition; an index's projection is set on the index", where)
			case raw.Project.Mode == ProjectAll:
			case raw.Project.Mode == ProjectKeys:
				a.Project = []*Field{}
			default:
				for _, fn := range raw.Project.Fields {
					f := e.Field(fn)
					if f == nil {
						r.errorf("%s: projected field %s is not a field of %s", where, fn, e.Name)
						continue
					}
					a.Project = append(a.Project, f)
				}
			}
			if a.Project != nil {
				r.claimType(e.GoName+a.GoName+"Item", where)
			}
		}
	}
	if len(raw.Of) > 0 && raw.Query != "partition" {
		r.errorf("%s: of lists the entities a partition read returns; it goes with query: partition", where)
	}
	if raw.Batch != nil && a.Kind != AccessGet {
		r.errorf("%s: batch applies to get: key (a read of several items by their keys)", where)
	}
	if raw.All {
		if a.Kind != AccessCounter {
			r.errorf("%s: all applies to a counter read: every item of the counter in one partition", where)
		} else {
			a.All = true
			a.Page, a.MaxPage = r.pageSize(where, raw)
		}
	}
	switch {
	case a.Kind == AccessScan && (raw.Order != "" || raw.Range != "" || raw.Project.Set):
		r.errorf("%s: order, range and project apply to queries; a scan takes page and max_page", where)
	case a.All && (raw.Order != "" || raw.Range != "" || raw.Project.Set):
		r.errorf("%s: order, range and project apply to queries; a read of all of a counter's items takes page and max_page", where)
	case a.Kind != AccessQuery && a.Kind != AccessScan && !a.All && (raw.Order != "" || raw.Page != 0 || raw.MaxPage != 0 || raw.Range != "" || raw.Project.Set):
		r.errorf("%s: order, page, max_page, range and project only apply to queries", where)
	}
	switch a.Freshness {
	case FreshnessImmediate:
		if raw.Consistent != nil && !*raw.Consistent {
			r.errorf("%s: freshness: immediate needs a consistent read; leave consistent out", where)
		}
		if a.Kind != AccessGetUnique && (a.Index == nil || a.Index.Strategy != StrategyGSI) {
			a.Consistent = true
		}
	case FreshnessEventual:
		if raw.Consistent != nil && *raw.Consistent {
			r.errorf("%s: consistent: true contradicts freshness: eventual (a consistent read costs twice as much, for freshness the read doesn't need); leave one out", where)
		}
	}
	return a
}

// pageSize resolves a query's or scan's page and max_page.
func (r *resolver) pageSize(where string, raw RawAccess) (page, maxPage int) {
	page = raw.Page
	if page == 0 {
		page = 50
	}
	maxPage = raw.MaxPage
	if maxPage == 0 {
		maxPage = max(100, page)
	}
	if page < 1 || maxPage < page || maxPage > 1000 {
		r.errorf("%s: need 1 <= page <= max_page <= 1000", where)
	}
	return page, maxPage
}

// accepts records a subject's accepted findings. The analysis checks the rule names and that each
// acceptance still matches a finding.
func (r *resolver) accepts(s Subject, where string, raw Ordered[string]) {
	for _, a := range raw {
		if strings.TrimSpace(a.Value) == "" {
			r.errorf("%s accept %s: give the reason the finding is acceptable", where, a.Key)
			continue
		}
		r.m.Accepts = append(r.m.Accepts, &Acceptance{Subject: s, Rule: a.Key, Reason: strings.TrimSpace(a.Value), Line: a.Line})
	}
}

func (r *resolver) write(e *Entity, name string, raw RawWrite) *Write {
	where := fmt.Sprintf("entity %s write %s", e.Name, name)
	if !reExported.MatchString(name) {
		r.errorf("%s: write names must be PascalCase", where)
		return nil
	}
	w := &Write{Name: name, GoName: name, Entity: e, Doc: raw.Doc, Rate: raw.Rate, HotKeyRate: raw.HotKeyRate}
	kinds := 0
	if raw.Create {
		kinds++
		w.Kind = WriteCreate
	}
	if raw.Delete {
		kinds++
		w.Kind = WriteDelete
	}
	if raw.isUpdate {
		kinds++
		w.Kind = WriteUpdate
	}
	if kinds != 1 {
		r.errorf("%s: declare exactly one of create, update or delete", where)
		return nil
	}
	if w.Kind != WriteUpdate && len(raw.When) > 0 {
		r.errorf("%s: when only applies to updates", where)
	}
	if w.Kind == WriteDelete && len(raw.Set) > 0 {
		r.errorf("%s: set applies to creates and updates", where)
	}
	switch raw.Versioned {
	case "", "optional":
	case "required":
		if w.Kind == WriteCreate {
			r.errorf("%s: a create has no earlier version to check; versioned applies to updates and deletes", where)
		}
		w.VersionRequired = true
	default:
		r.errorf("%s: versioned must be optional or required", where)
	}
	seen := map[string]bool{}
	argField := func(list, fn string) *Field {
		f := e.Field(fn)
		switch {
		case f == nil:
			r.errorf("%s %s: %s is not a field of %s", where, list, fn, e.Name)
			return nil
		case f.Key:
			r.errorf("%s %s: %s is part of the primary key and cannot be updated", where, list, fn)
			return nil
		case seen[fn]:
			r.errorf("%s: %s is listed more than once across update, patch and set", where, fn)
		}
		seen[fn] = true
		return f
	}
	for _, fn := range raw.Update {
		if f := argField("update", fn); f != nil {
			w.Args = append(w.Args, f)
		}
	}
	for _, fn := range raw.Patch {
		if f := argField("patch", fn); f != nil {
			w.Patch = append(w.Patch, f)
		}
	}
	for _, sc := range raw.Set {
		f := argField("set", sc.Key)
		if f == nil {
			continue
		}
		v, err := coerce(f, sc.Value)
		if err != nil {
			r.errorf("%s set: %s: %v", where, sc.Key, err)
			continue
		}
		if f.Required && isZero(v) {
			r.errorf("%s set: %s is required, so it cannot be set to its zero value", where, sc.Key)
		}
		w.Sets = append(w.Sets, SetConst{Field: f, Value: v})
	}
	if w.Kind == WriteUpdate {
		if len(w.Args)+len(w.Patch)+len(w.Sets) == 0 {
			r.errorf("%s: an update must change at least one field", where)
		}
		w.When = r.preds(e, where+" when", raw.When)
		for _, p := range w.When {
			if p.Field.Key {
				r.errorf("%s when: %s is a primary key field; the key already selects the item", where, p.Field.Name)
			}
		}
		if len(w.Args)+len(w.Patch) > 0 {
			r.claimType(e.GoName+w.GoName, where)
		}
	}
	if raw.Batch != nil {
		n, ok := raw.Batch.(int)
		switch {
		case !ok || n < 1:
			r.errorf("%s: batch is the typical number of items a call changes (batch: 50), which the estimates use", where)
		case w.Kind != WriteUpdate:
			r.errorf("%s: batch applies to updates: the same change to several items", where)
		case len(raw.Requires) > 0:
			r.errorf("%s: a batch write can't have requires: several of its items could require the same item, which a transaction may touch only once", where)
		case w.VersionRequired:
			r.errorf("%s: a batch write takes keys, not versions; versioned: required applies to a write of one item", where)
		default:
			w.Batch = n
		}
	}
	w.rawRequires = raw.Requires
	return w
}

// crossRefs resolves what refers to other entities (requires, copy_of, counters read from another
// entity) and then plans every write, which depends on them.
func (r *resolver) crossRefs(m *Model) {
	byName := map[string]*Entity{}
	for _, e := range m.Entities {
		byName[e.Name] = e
	}
	counters := map[string]*Counter{}
	for _, e := range m.Entities {
		for _, c := range e.Counters {
			counters[c.Name] = c
		}
	}
	source := func(e *Entity, f *Field, key, raw string) (*Entity, *Field) {
		where := fmt.Sprintf("entity %s field %s %s", e.Name, f.Name, key)
		en, fn, ok := strings.Cut(raw, ".")
		te := byName[en]
		switch {
		case !ok || te == nil:
			r.errorf("%s: %q must be Entity.field, naming an entity of this table", where, raw)
		case te == e:
			r.errorf("%s: copies within one entity are kept in sync already; %s is for another entity's field", where, key)
		case te.Field(fn) == nil:
			r.errorf("%s: %s has no field %s", where, en, fn)
		case te.Field(fn).Type != f.Type:
			r.errorf("%s: %s.%s is a %s, but %s is a %s", where, en, fn, te.Field(fn).Type, f.Name, f.Type)
		default:
			return te, te.Field(fn)
		}
		return nil, nil
	}
	entries := map[*Counter]bool{}
	for _, e := range m.Entities {
		for _, f := range e.Fields {
			if f.copyOfRaw != "" {
				f.CopyOfEntity, f.CopyOf = source(e, f, "copy_of", f.copyOfRaw)
			}
			if f.snapshotOfRaw != "" {
				f.SnapshotOfEntity, f.SnapshotOf = source(e, f, "snapshot_of", f.snapshotOfRaw)
			}
			if f.refRaw != "" {
				if f.Ref = byName[f.refRaw]; f.Ref == nil {
					r.errorf("entity %s field %s ref: %s is not an entity of this table", e.Name, f.Name, f.refRaw)
				}
			}
		}
		for _, a := range e.Access {
			if a.ofRaw != nil {
				r.partitionRead(e, a, byName)
			}
			if a.Kind != AccessCounter {
				continue
			}
			a.Counter = counters[a.counterRaw]
			if a.Counter == nil {
				r.errorf("entity %s access %s: %s is not a counter of this table", e.Name, a.Name, a.counterRaw)
				continue
			}
			if a.All {
				r.counterAll(e, a, entries)
			}
		}
		for _, w := range e.Writes {
			for _, rq := range w.rawRequires {
				if req := r.require(e, w, rq.Key, rq.Value, byName, counters); req != nil {
					w.Requires = append(w.Requires, req)
				}
			}
			r.sameItemTwice(w)
			r.planWrite(w)
		}
	}
}

// partitionRead resolves the entities a partition read returns. Each must key its items under
// the same partition key as the reading entity, so that one Query of that key finds them.
func (r *resolver) partitionRead(e *Entity, a *Access, byName map[string]*Entity) {
	where := fmt.Sprintf("entity %s access %s", e.Name, a.Name)
	names := map[string]string{}
	for _, name := range a.ofRaw {
		oe := byName[name]
		switch {
		case oe == nil:
			r.errorf("%s: of: %s is not an entity of this table", where, name)
			continue
		case slices.Contains(a.Of, oe):
			r.errorf("%s: of: %s is listed twice", where, name)
			continue
		case oe.PK.Raw != e.PK.Raw:
			r.errorf("%s: of: %s's items are under partition key %q, not %s's %q, so one Query can't read both. A partition read returns entities whose partition key templates are the same", where, name, oe.PK.Raw, e.Name, e.PK.Raw)
			continue
		}
		same := true
		for i, f := range oe.PK.Fields {
			if f.Type != e.PK.Fields[i].Type {
				r.errorf("%s: of: %s.%s is a %s, but %s.%s is a %s: the two render different partition keys", where, name, f.Name, f.Type, e.Name, f.Name, e.PK.Fields[i].Type)
				same = false
			}
		}
		if !same {
			continue
		}
		field := Plural(oe.GoName)
		if oe.Singleton() {
			field = oe.GoName
		}
		if prev, ok := names[field]; ok {
			r.errorf("%s: of: %s and %s would both be the field %s of the generated %s%s; rename one of the entities", where, prev, name, field, e.GoName, a.GoName)
			continue
		}
		names[field] = name
		a.Of = append(a.Of, oe)
	}
	if a.Of == nil {
		a.Of = []*Entity{}
	}
}

// counterAll checks a read of every item of a counter in one partition: the items must be several
// (the sort key has fields of its own), on one partition key (not sharded), and their keys must
// be readable back from the sort key, since a counter item stores nothing else to say which it is.
func (r *resolver) counterAll(e *Entity, a *Access, entries map[*Counter]bool) {
	c := a.Counter
	where := fmt.Sprintf("entity %s access %s", e.Name, a.Name)
	own := c.ItemFields()
	switch {
	case c.Shards > 1:
		r.errorf("%s: %s is sharded: its items are spread over %d partition keys, which one Query can't read", where, c.Name, c.Shards)
		return
	case len(own) == 0:
		r.errorf("%s: %s has one item per partition key (its sort key %q has no field of its own); read it with counter alone", where, c.Name, c.SK.Raw)
		return
	}
	segs := c.SK.Segments
	for i, sg := range segs {
		if !sg.IsField() {
			continue
		}
		f := c.Entity.Field(sg.Field)
		switch {
		case sg.Transform != "":
			r.errorf("%s: %s's sort key holds {%s|%s}, which can't be read back into %s; a counter read with all needs its sort key fields as they are", where, c.Name, sg.Field, sg.Transform, sg.Field)
			return
		case f.Type != TypeString && f.Type != TypeEnum && f.Type != TypeStringSet:
			r.errorf("%s: %s's sort key holds %s, a %s; a counter read with all needs string or enum fields there, to read each item's key back", where, c.Name, f.Name, f.Type)
			return
		case i+1 < len(segs) && segs[i+1].IsField():
			r.errorf("%s: %s's sort key puts {%s} directly before {%s}, so the two can't be told apart when read back; put literal text between them", where, c.Name, sg.Field, segs[i+1].Field)
			return
		}
	}
	r.claimType(e.GoName+a.GoName+"Query", where)
	if !entries[c] {
		entries[c] = true
		r.claimType(c.GoName+"Entry", "counter "+c.Name+" (read with all)")
	}
}

// require resolves one `requires` entry: a condition on another item, keyed by fields of this
// entity and checked in the write's transaction. The item is an entity, which the write may also
// change (set) or delete (consume), or a counter, whose values it compares with constants.
func (r *resolver) require(e *Entity, w *Write, target string, raw RawRequire, byName map[string]*Entity, counters map[string]*Counter) *Require {
	where := fmt.Sprintf("entity %s write %s requires %s", e.Name, w.Name, target)
	req := &Require{Name: target, Optional: raw.Optional, Consume: raw.Consume, ErrName: "Err" + e.GoName + w.GoName + "Requires" + target}
	var keyFields []*Field
	var owner *Entity
	switch te, c := byName[target], counters[target]; {
	case te != nil:
		req.Target, owner, keyFields = te, te, te.KeyFields()
	case c != nil:
		req.Counter, owner, keyFields = c, c.Entity, c.KeyFields()
		if c.Shards > 1 {
			r.errorf("%s: %s is sharded, so no single item holds its values to check", where, target)
		}
		if len(raw.Set)+len(raw.Add)+len(raw.Patch) > 0 || raw.Optional || raw.Ensure != nil {
			r.errorf("%s: a counter can only be checked with when (an absent value counts as 0), and its item deleted with consume once it reads zero", where)
		}
	default:
		r.errorf("%s: %s is not an entity or counter of this table", where, target)
		return nil
	}
	given := map[string]string{}
	for _, k := range raw.Key {
		given[k.Key] = k.Value
	}
	for _, tf := range keyFields {
		src, ok := given[tf.Name]
		if !ok {
			r.errorf("%s: key must give %s's key field %s (as %s: <field of %s>)", where, target, tf.Name, tf.Name, e.Name)
			continue
		}
		delete(given, tf.Name)
		if req.Counter != nil && tf == req.Counter.Set {
			// The counter has an item for each element of the set: the key names one of them.
			sf := e.Field(src)
			switch {
			case sf == nil:
				r.errorf("%s key %s: %s is not a field of %s", where, tf.Name, src, e.Name)
			case sf.Type != TypeString:
				r.errorf("%s key %s: %s has an item for each element of %s, so the key takes one element: a string field of %s, and %s is a %s", where, tf.Name, target, tf.Name, e.Name, src, sf.Type)
			default:
				req.Key = append(req.Key, RequireKey{Target: tf, Source: sf})
			}
			continue
		}
		sf := r.source(e, where+" key "+tf.Name, src, owner, tf)
		if sf != nil {
			req.Key = append(req.Key, RequireKey{Target: tf, Source: sf})
		}
	}
	for k := range given {
		r.errorf("%s: key: %s is not a key field of %s", where, k, target)
	}
	if req.Counter != nil {
		for _, p := range raw.When {
			var v *CounterValue
			for _, cv := range req.Counter.Values {
				if cv.Name == p.Key {
					v = cv
				}
			}
			n, ok := p.Value.(int)
			switch {
			case v == nil:
				r.errorf("%s when: %s is not a value of counter %s", where, p.Key, target)
			case !ok:
				r.errorf("%s when: %s: want an integer, got %v", where, p.Key, p.Value)
			default:
				req.CounterWhen = append(req.CounterWhen, CounterPred{Value: v, Equals: int64(n)})
			}
		}
		if req.Consume {
			// The item goes, so everything it counts must have gone first: every value, not only
			// those when names.
			for _, v := range req.Counter.Values {
				named := false
				for _, p := range req.CounterWhen {
					if p.Value != v {
						continue
					}
					named = true
					if p.Equals != 0 {
						r.errorf("%s when: consume deletes the counter item, which must read zero: %s can't be required to be %d", where, v.Name, p.Equals)
					}
				}
				if !named {
					req.CounterWhen = append(req.CounterWhen, CounterPred{Value: v, Equals: 0})
				}
			}
		}
		if len(req.CounterWhen) == 0 {
			r.errorf("%s: when must give at least one value of the counter", where)
		}
		return req
	}
	te := req.Target
	for _, p := range raw.When {
		f := te.Field(p.Key)
		if f == nil {
			r.errorf("%s when: %s is not a field of %s", where, p.Key, te.Name)
			continue
		}
		// A reference to the writing entity's field: "{memberId}", or { not: "{memberId}" }.
		raw, not := p.Value, false
		if form, ok := raw.(map[string]any); ok && len(form) == 1 {
			if arg, ok := form["not"]; ok {
				if _, isRef := fieldRef(arg); isRef {
					raw, not = arg, true
				}
			}
		}
		if ref, ok := fieldRef(raw); ok {
			if sf := r.source(e, where+" when "+p.Key, ref, te, f); sf != nil {
				req.When = append(req.When, &Pred{Field: f, Source: sf, Not: not})
			}
			continue
		}
		pr, err := pred(f, p.Value)
		if err != nil {
			r.errorf("%s when: %s: %v", where, p.Key, err)
			continue
		}
		req.When = append(req.When, pr)
	}
	for _, sc := range raw.Set {
		f := te.Field(sc.Key)
		switch {
		case f == nil:
			r.errorf("%s set: %s is not a field of %s", where, sc.Key, te.Name)
			continue
		case f.Key:
			r.errorf("%s set: %s is part of %s's primary key and cannot be changed", where, sc.Key, te.Name)
			continue
		}
		if ref, ok := fieldRef(sc.Value); ok {
			if sf := r.source(e, where+" set "+sc.Key, ref, te, f); sf != nil {
				req.Sets = append(req.Sets, SetConst{Field: f, Source: sf})
			}
			continue
		}
		v, err := coerce(f, sc.Value)
		if err != nil {
			r.errorf("%s set: %s: %v", where, sc.Key, err)
			continue
		}
		if f.Required && isZero(v) {
			r.errorf("%s set: %s.%s is required, so it cannot be set to its zero value", where, te.Name, sc.Key)
		}
		req.Sets = append(req.Sets, SetConst{Field: f, Value: v})
	}
	// changes resolves the target field of an add or a patch, which may name each field once
	// across set, add and patch.
	changes := func(kind, name string) *Field {
		f := te.Field(name)
		switch {
		case f == nil:
			r.errorf("%s %s: %s is not a field of %s", where, kind, name, te.Name)
			return nil
		case f.Key:
			r.errorf("%s %s: %s is part of %s's primary key and cannot be changed", where, kind, name, te.Name)
			return nil
		}
		for _, s := range req.Sets {
			if s.Field == f {
				r.errorf("%s: %s is given more than once across set, add, patch and ensure", where, name)
				return nil
			}
		}
		return f
	}
	for _, ad := range raw.Add {
		f := changes("add", ad.Key)
		if f == nil {
			continue
		}
		if f.Type != TypeInt {
			r.errorf("%s add: %s.%s is a %s; add applies to int fields", where, te.Name, ad.Key, f.Type)
			continue
		}
		if ref, ok := fieldRef(ad.Value); ok {
			if sf := r.source(e, where+" add "+ad.Key, ref, te, f); sf != nil {
				req.Sets = append(req.Sets, SetConst{Field: f, Source: sf, Add: true})
			}
			continue
		}
		n, ok := ad.Value.(int)
		if !ok || n == 0 {
			r.errorf("%s add: %s: want a whole number other than 0 (negative to subtract), or \"{field}\" for an int field of %s", where, ad.Key, e.Name)
			continue
		}
		req.Sets = append(req.Sets, SetConst{Field: f, Value: int64(n), Add: true})
	}
	for _, pt := range raw.Patch {
		f := changes("patch", pt.Key)
		if f == nil {
			continue
		}
		ref, ok := fieldRef(pt.Value)
		if !ok {
			r.errorf("%s patch: %s: want \"{field}\", a field of %s: the %s's %s is set to it when it has a value, and left alone when it doesn't. A constant goes in set", where, pt.Key, e.Name, te.Name, pt.Key)
			continue
		}
		if sf := r.source(e, where+" patch "+pt.Key, ref, te, f); sf != nil {
			req.Sets = append(req.Sets, SetConst{Field: f, Source: sf, IfSet: true})
		}
	}
	if raw.Ensure != nil {
		req.Ensure = true
		for _, en := range *raw.Ensure {
			f := changes("ensure", en.Key)
			if f == nil {
				continue
			}
			if ref, ok := fieldRef(en.Value); ok {
				if sf := r.source(e, where+" ensure "+en.Key, ref, te, f); sf != nil {
					req.EnsureSets = append(req.EnsureSets, SetConst{Field: f, Source: sf})
				}
				continue
			}
			v, err := coerce(f, en.Value)
			if err != nil {
				r.errorf("%s ensure: %s: %v", where, en.Key, err)
				continue
			}
			req.EnsureSets = append(req.EnsureSets, SetConst{Field: f, Value: v})
		}
		switch {
		case req.Optional:
			r.errorf("%s: ensure creates the %s when it is absent, and optional lets the write go ahead without one: choose one", where, te.Name)
		case req.Consume:
			r.errorf("%s: ensure creates the %s when it is absent, and consume deletes it: choose one", where, te.Name)
		}
		// A new item needs its required fields: what gives none of them a value can never create it.
		given := map[*Field]SetConst{}
		for _, s := range append(append([]SetConst{}, req.EnsureSets...), req.Sets...) {
			given[s.Field] = s
		}
		for _, f := range te.Fields {
			s, ok := given[f]
			switch {
			case !f.Required || f.Key:
			case !ok:
				r.errorf("%s: %s.%s is required, so a %s this write creates needs it: give it in ensure (%s: \"{field}\" or a constant)", where, te.Name, f.Name, te.Name, f.Name)
			case s.Fixes() && isZero(s.Value):
				r.errorf("%s ensure: %s.%s is required, so it cannot be given its zero value", where, te.Name, f.Name)
			}
		}
		for _, c := range te.Counters {
			for _, v := range c.Values {
				if v.LimitArg && CanGrow(req.TargetCreate(), v, true) {
					r.errorf("%s: creating a %s can grow %s.%s, whose limit callers supply; only %s's own writes can take it", where, te.Name, c.Name, v.Name, te.Name)
				}
			}
		}
	}
	if req.Optional && len(req.When) == 0 && !req.Writes() {
		r.errorf("%s: optional with no when, set or consume checks nothing", where)
	}
	if len(req.Sets) > 0 && req.Consume {
		r.errorf("%s: set, add and patch are exclusive with consume: the item is either changed or deleted", where)
	}
	if len(req.Sets) > 0 {
		// The change to the target behaves like an update of it declared with this set and when.
		tw := req.TargetWrite()
		changed := map[string]bool{}
		for _, s := range req.Sets {
			changed[s.Field.Name] = true
		}
		for _, c := range te.Counters {
			for _, v := range c.Values {
				if v.LimitArg && CanGrow(tw, v, false) {
					r.errorf("%s: the change can grow %s.%s, whose limit callers supply; only %s's own writes can take it", where, c.Name, v.Name, te.Name)
				}
			}
		}
		req.Fast = !req.Optional && transitionable(te, tw, changed)
	}
	if req.Ensure && len(req.Sets) == 0 {
		// Nothing to change on one that is there: a check, which finds out if it isn't.
		req.Fast = true
	}
	if req.Consume {
		req.Fast = !te.HasDerived()
	}
	return req
}

// sameItemTwice refuses requires that would make a transaction touch one item twice, which
// DynamoDB rejects on every call: requiring the write's own item, or checking a counter the write
// (or a change it makes through requires) also updates. Two changes to one counter item are fine:
// they are merged.
func (r *resolver) sameItemTwice(w *Write) {
	e := w.Entity
	for _, rq := range w.Requires {
		where := fmt.Sprintf("entity %s write %s requires %s", e.Name, w.Name, rq.Name)
		if rq.Target == e {
			self := true
			for _, k := range rq.Key {
				self = self && k.Source == k.Target
			}
			if self {
				r.errorf("%s: that is the item being written; a transaction can touch an item only once. Use when on the write itself", where)
			}
		}
		if rq.Counter == nil {
			continue
		}
		c := rq.Counter
		if c.Entity == e && touchesCounter(w, c) {
			r.errorf("%s: the write also updates counter %s, and a transaction can touch an item only once. Use a limit or min on the counter value instead", where, c.Name)
		}
		for _, other := range w.Requires {
			if other.Target != c.Entity || !other.Writes() {
				continue
			}
			tw := other.TargetWrite()
			if other.Consume {
				tw = &Write{Entity: other.Target, Kind: WriteDelete}
			}
			if touchesCounter(tw, c) {
				r.errorf("%s: the change to the %s also updates counter %s, and a transaction can touch an item only once. Use a limit or min on the counter value instead", where, other.Name, c.Name)
			}
		}
	}
}

// touchesCounter reports whether a write of the counter's entity can change the counter.
func touchesCounter(w *Write, c *Counter) bool {
	if w.Kind != WriteUpdate {
		return true
	}
	in := map[*Field]bool{}
	for _, f := range c.KeyFields() {
		in[f] = true
	}
	for _, v := range c.Values {
		if v.Sum != nil {
			in[v.Sum] = true
		}
		for _, p := range v.Where {
			in[p.Field] = true
		}
	}
	for _, f := range w.Changed() {
		if in[f] {
			return true
		}
	}
	return false
}

// TargetWrite describes the change a requirement makes to its target as an update of it.
func (rq *Require) TargetWrite() *Write {
	return &Write{Name: "requires", Entity: rq.Target, Kind: WriteUpdate, Sets: rq.Sets, When: rq.When}
}

// TargetCreate describes the creation a requirement with ensure makes when its target is absent,
// as a create of it.
func (rq *Require) TargetCreate() *Write {
	return &Write{Name: "requires", Entity: rq.Target, Kind: WriteCreate, Sets: append(append([]SetConst{}, rq.EnsureSets...), rq.Sets...)}
}

// source resolves a field of the writing entity e that supplies a value for field tf of another
// entity (or counter owner), checking the types match.
func (r *resolver) source(e *Entity, where, name string, owner *Entity, tf *Field) *Field {
	sf := e.Field(name)
	switch {
	case sf == nil:
		r.errorf("%s: %s is not a field of %s", where, name, e.Name)
		return nil
	case sf.Type != tf.Type || (sf.Type == TypeEnum && strings.Join(sf.Enum, ",") != strings.Join(tf.Enum, ",")):
		r.errorf("%s: %s.%s is a %s, but %s.%s is a %s", where, e.Name, name, sf.Type, owner.Name, tf.Name, tf.Type)
		return nil
	}
	return sf
}

// fieldRef recognises a value that names a field of the writing entity: "{memberId}".
func fieldRef(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || len(s) < 3 || s[0] != '{' || s[len(s)-1] != '}' {
		return "", false
	}
	name := s[1 : len(s)-1]
	return name, reLower.MatchString(name)
}

func isZero(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case int64:
		return x == 0
	case float64:
		return x == 0
	case string:
		return x == ""
	}
	return false
}

// planWrite decides whether the write reads first and which caller-supplied limits it takes.
func (r *resolver) planWrite(w *Write) {
	e := w.Entity
	switch w.Kind {
	case WriteCreate:
		w.ReadFirst = false
		for _, c := range e.Counters {
			for _, v := range c.Values {
				if v.LimitArg && CanGrow(w, v, true) {
					w.Limits = append(w.Limits, v)
				}
			}
		}
	case WriteDelete:
		w.ReadFirst = e.HasDerived() || len(w.Requires) > 0
	case WriteUpdate:
		w.ReadFirst, w.Transition = PlanReads(e, w)
		for _, c := range e.Counters {
			for _, v := range c.Values {
				if v.LimitArg && CanGrow(w, v, false) {
					w.Limits = append(w.Limits, v)
				}
			}
		}
		if w.Batch > 0 {
			// Its items are read together, and each written under the revision read.
			w.ReadFirst, w.Transition = true, false
			if len(w.Limits) > 0 {
				r.errorf("entity %s write %s: a batch write can't grow %s.%s, whose limit callers supply for one item at a time", e.Name, w.Name, w.Limits[0].Counter.Name, w.Limits[0].Name)
				w.Limits = nil
			}
		}
	}
	if len(w.Limits) > 0 {
		r.claimType(e.GoName+w.GoName+"Limits", "entity "+e.Name+" write "+w.Name)
	}
}

// PlanReads decides whether an update of e must read the item first, and whether it can run
// read-free instead. e is normally w.Entity; the analysis passes variants of it (another index
// strategy) to compare designs.
func PlanReads(e *Entity, w *Write) (readFirst, transition bool) {
	switch w.Kind {
	case WriteCreate:
		return false, false
	case WriteDelete:
		return e.HasDerived() || len(w.Requires) > 0, false
	}
	inputs := e.DerivedInputs()
	changed := map[string]bool{}
	for _, f := range w.Changed() {
		changed[f.Name] = true
		if inputs[f.Name] {
			readFirst = true
		}
	}
	if len(w.Requires) > 0 {
		// Condition checks and changes to other items need a transaction, which the single
		// UpdateItem path cannot be.
		readFirst = true
	}
	return readFirst, readFirst && transitionable(e, w, changed) && requiresKnown(w)
}

// gsis merges entity indexes with the gsi strategy into physical GSIs.
func (r *resolver) gsis(m *Model) {
	byName := map[string]*GSI{}
	for _, e := range m.Entities {
		for _, ix := range e.Indexes {
			if ix.Strategy != StrategyGSI {
				continue
			}
			g := byName[ix.Name]
			if g == nil {
				g = &GSI{Name: ix.Name, PKAttr: ix.PKAttr, SKAttr: ix.SKAttr, HasSK: ix.HasSK}
				byName[ix.Name] = g
				m.GSIs = append(m.GSIs, g)
			}
			if g.HasSK != ix.HasSK {
				r.errorf("gsi %s: every entity sharing it must declare an sk, or none may", ix.Name)
			}
			g.Users = append(g.Users, ix)
			ix.GSI = g
		}
	}
	for _, g := range m.GSIs {
		g.Projection = ProjectInclude
		attrs := []string{AttrType, AttrVer}
		seen := map[string]bool{AttrType: true, AttrVer: true}
		for _, ix := range g.Users {
			if ix.Projection == ProjectAll {
				g.Projection = ProjectAll
			}
			for _, f := range ix.ProjectedFields() {
				if !seen[f.Attr] {
					seen[f.Attr] = true
					attrs = append(attrs, f.Attr)
				}
			}
			// Queries leave out expired items, which needs the TTL attribute in the index.
			if ix.Entity.TTL != nil && !seen[m.Table.TTLAttr] {
				seen[m.Table.TTLAttr] = true
				attrs = append(attrs, m.Table.TTLAttr)
			}
		}
		if g.Projection == ProjectInclude {
			g.NonKeyAttrs = attrs
		}
		if len(g.Users) > 1 {
			if !g.HasSK {
				r.errorf("gsi %s: shared by several entities, so it needs an sk to tell them apart", g.Name)
				continue
			}
			for i, a := range g.Users {
				for _, b := range g.Users[i+1:] {
					if a.Entity == b.Entity {
						continue
					}
					if keytmpl.MayOverlap(a.PK.Template, b.PK.Template) && keytmpl.PrefixCollides(a.SK.Template, b.SK.Template) {
						r.errorf("gsi %s: %s and %s may share partitions and their sort keys %q and %q are not distinguishable by prefix",
							g.Name, a.Entity.Name, b.Entity.Name, a.SK.Raw, b.SK.Raw)
					}
				}
			}
		}
	}
	// DynamoDB counts projected (INCLUDE) attributes across all of a table's indexes: an attribute
	// projected into two indexes counts twice.
	projected := 0
	for _, g := range m.GSIs {
		projected += len(g.NonKeyAttrs)
	}
	if projected > 100 {
		r.errorf("the GSIs project %d attributes in total (counting dynago's _t and _v, and the TTL attribute, in each); DynamoDB allows 100 per table, summed across indexes. Project fewer fields, or use project: all for a wide index", projected)
	}
	// A field stored under a GSI key attribute's name would be overwritten by the index key.
	keyAttrs := map[string]string{}
	for _, g := range m.GSIs {
		keyAttrs[g.PKAttr] = g.Name
		if g.HasSK {
			keyAttrs[g.SKAttr] = g.Name
		}
	}
	for _, e := range m.Entities {
		for _, f := range e.Fields {
			if gsi, ok := keyAttrs[f.Attr]; ok {
				r.errorf("entity %s field %s: attribute %q is a key attribute of GSI %s; give the field another attr", e.Name, f.Name, f.Attr, gsi)
			}
		}
	}
	if len(m.GSIs) > 20 {
		r.errorf("table has %d GSIs; DynamoDB allows 20 by default", len(m.GSIs))
	}
}

// keyspace is one family of items in the base table.
type keyspace struct {
	what   string
	pk, sk keytmpl.Template
}

// keyspaces checks that every item family in the base table can be told apart: two families
// that may share a partition must have sort keys that no begins_with query can confuse.
func (r *resolver) keyspaces(m *Model) {
	var ks []keyspace
	for _, e := range m.Entities {
		ks = append(ks, keyspace{"entity " + e.Name, e.PK.Template, e.SK.Template})
		for _, ix := range e.Indexes {
			if ix.Strategy == StrategyCopy {
				ks = append(ks, keyspace{fmt.Sprintf("copy index %s.%s", e.Name, ix.Name), ix.PK.Template, ix.SK.Template})
			}
		}
		for _, u := range e.Uniques {
			ks = append(ks, keyspace{fmt.Sprintf("unique %s.%s", e.Name, u.Name), u.PK.Template, u.SK.Template})
		}
		for _, c := range e.Counters {
			ks = append(ks, keyspace{fmt.Sprintf("counter %s", c.Name), c.PK.Template, c.SK.Template})
		}
	}
	for i, a := range ks {
		for _, b := range ks[i+1:] {
			if keytmpl.MayOverlap(a.pk, b.pk) && keytmpl.PrefixCollides(a.sk, b.sk) {
				r.errorf("%s (pk %q, sk %q) and %s (pk %q, sk %q) may share a partition and their sort keys are not distinguishable by prefix",
					a.what, a.pk.Raw, a.sk.Raw, b.what, b.pk.Raw, b.sk.Raw)
			}
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func fileBase(table string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(table) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return filepath.Base(b.String())
}

// rangeSegment returns the placeholder directly after the sort key's literal prefix.
func rangeSegment(sk Template) string {
	i := 0
	if len(sk.Segments) > 0 && !sk.Segments[0].IsField() {
		i = 1
	}
	if i < len(sk.Segments) {
		return sk.Segments[i].Field
	}
	return ""
}

// requiresKnown reports whether every field a write's requires read is known without reading the
// item: a key field, pinned by when, or given by the call.
func requiresKnown(w *Write) bool {
	known := map[*Field]bool{}
	for _, f := range w.Entity.KeyFields() {
		known[f] = true
	}
	for _, p := range w.When {
		if p.Pins() {
			known[p.Field] = true
		}
	}
	for _, f := range w.Args {
		known[f] = true
	}
	for _, s := range w.Sets {
		known[s.Field] = true
	}
	for _, rq := range w.Requires {
		for _, f := range rq.Sources() {
			if !known[f] {
				return false
			}
		}
	}
	return true
}

// transitionable reports whether a read-first update can run without the read: every derived value
// it changes must depend only on fields whose before and after values are known from the call —
// key fields, and fields pinned by `when`. Index keys and copies need the whole item, so any
// change to them rules it out, as does a patch field that feeds anything (it may or may not be
// given).
func transitionable(e *Entity, w *Write, changed map[string]bool) bool {
	known := map[*Field]bool{}
	for _, f := range e.KeyFields() {
		known[f] = true
	}
	for _, p := range w.When {
		// `not` and `in` leave several values possible: the field isn't known.
		if p.Pins() {
			known[p.Field] = true
		}
	}
	touches := func(fs ...*Field) bool {
		for _, f := range fs {
			if changed[f.Name] {
				return true
			}
		}
		return false
	}
	allKnown := func(fs ...*Field) bool {
		for _, f := range fs {
			if !known[f] {
				return false
			}
		}
		return true
	}
	inputs := e.DerivedInputs()
	for _, f := range w.Patch {
		if inputs[f.Name] {
			return false
		}
	}
	for _, ix := range e.Indexes {
		keys := append(append([]*Field{}, ix.PK.Fields...), ix.SK.Fields...)
		for _, p := range ix.Where {
			keys = append(keys, p.Field)
		}
		if touches(keys...) || (ix.Strategy == StrategyCopy && touches(ix.ProjectedFields()...)) {
			return false
		}
	}
	for _, u := range e.Uniques {
		if touches(u.Fields...) && !allKnown(u.Fields...) {
			return false
		}
	}
	for _, c := range e.Counters {
		// Any change to the counter needs its key: an unknown key field would render no key, and
		// the change would silently not be written.
		changes := touches(c.KeyFields()...)
		for _, v := range c.Values {
			in := []*Field{}
			if v.Sum != nil {
				in = append(in, v.Sum)
			}
			for _, p := range v.Where {
				in = append(in, p.Field)
			}
			if touches(in...) {
				changes = true
				if !allKnown(in...) {
					return false
				}
			}
		}
		if changes && !allKnown(c.KeyFields()...) {
			return false
		}
		if touches(c.KeyFields()...) {
			for _, v := range c.Values {
				for _, p := range v.Where {
					if !known[p.Field] {
						return false
					}
				}
				if v.Sum != nil && !known[v.Sum] {
					return false
				}
			}
		}
	}
	return true
}

// CanGrow reports whether an update can increase a counter value, and so must be given its limit.
// It cannot if it leaves the value's inputs alone, or sets a `where` field to a constant that
// fails the condition (returning a loan never grows the active-loans count). With anyChange, the
// question is whether the update's result can contribute at all: an item written before the value
// existed starts contributing on its next rewrite, whatever the write changed.
func CanGrow(w *Write, v *CounterValue, anyChange bool) bool {
	for _, p := range v.Where {
		for _, s := range w.Sets {
			if s.Field == p.Field && s.Fixes() && !p.Matches(s.Value) {
				return false // the result never matches, so never contributes
			}
		}
	}
	if anyChange {
		return true
	}
	changed := map[*Field]bool{}
	for _, f := range w.Changed() {
		changed[f] = true
	}
	for _, f := range v.Counter.KeyFields() {
		if changed[f] {
			return true
		}
	}
	if v.Sum != nil && changed[v.Sum] {
		return true
	}
	for _, p := range v.Where {
		if changed[p.Field] {
			return true
		}
	}
	return false
}
