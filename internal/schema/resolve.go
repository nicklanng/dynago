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
		"New": "the generated constructor", "EnsureTable": "the generated EnsureTable"}
	m := &Model{Package: raw.Package}
	if raw.Dynago != 1 {
		r.errorf("dynago: schema format version must be 1 (got %d)", raw.Dynago)
	}
	if !rePackage.MatchString(raw.Package) {
		r.errorf("package: %q is not a valid Go package name", raw.Package)
	}
	m.Table = Table{Name: raw.Table.Name, Doc: raw.Table.Doc, TTLAttr: raw.Table.TTLAttribute}
	if m.Table.Name == "" {
		r.errorf("table.name is required")
	}
	if m.Table.TTLAttr == "" {
		m.Table.TTLAttr = "ttl"
	}
	base := fileBase(m.Table.Name)
	m.Output = Output{
		Go:        orDefault(raw.Output.Go, base+"_dynago.go"),
		Docs:      orDefault(raw.Output.Docs, base+".model.md"),
		Terraform: orDefault(raw.Output.Terraform, base+".tf.json"),
		TableJSON: orDefault(raw.Output.TableJSON, base+".table.json"),
		Lock:      orDefault(raw.Output.Lock, base+".dynago.lock"),
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
		Name: name, GoName: name, Doc: raw.Doc, Version: raw.Version, Items: raw.Estimate.Items,
		fieldsByName: map[string]*Field{},
	}
	r.claimType(e.GoName, where)
	r.claimType(e.GoName+"Key", where)
	if e.Version == 0 {
		e.Version = 1
	}
	if e.Version < 0 {
		r.errorf("%s: version must be positive", where)
	}

	reserved := map[string]bool{AttrPK: true, AttrSK: true, AttrType: true, AttrVer: true, AttrRev: true, m.Table.TTLAttr: true}
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

	for _, ri := range raw.Indexes {
		if ix := r.index(e, ri.Key, ri.Value); ix != nil {
			e.Indexes = append(e.Indexes, ix)
		}
	}
	for _, ru := range raw.Unique {
		if u := r.unique(e, ru.Key, ru.Value); u != nil {
			e.Uniques = append(e.Uniques, u)
		}
	}
	for _, rc := range raw.Counters {
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
		if a := r.access(e, ra.Key, ra.Value); a != nil {
			e.Access = append(e.Access, a)
		}
	}
	for _, rw := range raw.Writes {
		if methods[rw.Key] {
			r.errorf("%s: %s is declared more than once across access and writes", where, rw.Key)
		}
		methods[rw.Key] = true
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
	f := &Field{Name: name, Attr: orDefault(raw.Attr, name), GoName: GoName(name), Doc: raw.Doc, Example: raw.Example, copyOfRaw: raw.CopyOf, Required: raw.Required}
	switch f.GoName {
	case "Key", "Version":
		r.errorf("%s: a field named %s would clash with the generated %s() method; rename it", where, name, f.GoName)
		return nil
	case "PK", "SK", "T", "V", "Rev", "TTL":
		r.errorf("%s: the Go name %s is used by the generated item struct (for its %s); rename the field", where, f.GoName, map[string]string{
			"PK": "partition key", "SK": "sort key", "T": "type", "V": "schema version", "Rev": "revision", "TTL": "expiry"}[f.GoName])
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

func (r *resolver) preds(e *Entity, where string, raw Ordered[any]) []*Pred {
	var out []*Pred
	for _, p := range raw {
		f := e.Field(p.Key)
		if f == nil {
			r.errorf("%s: %s is not a field of %s", where, p.Key, e.Name)
			continue
		}
		v, err := coerce(f, p.Value)
		if err != nil {
			r.errorf("%s: %s: %v", where, p.Key, err)
			continue
		}
		out = append(out, &Pred{Field: f, Value: v})
	}
	return out
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

func (r *resolver) index(e *Entity, name string, raw RawIndex) *Index {
	where := fmt.Sprintf("entity %s index %s", e.Name, name)
	if !reExported.MatchString(name) {
		r.errorf("%s: index names must be PascalCase", where)
		return nil
	}
	ix := &Index{Name: name, GoName: name, Entity: e, Doc: raw.Doc, Since: e.Version}
	switch Strategy(orDefault(raw.Strategy, string(StrategyGSI))) {
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
	var ok bool
	if ix.PK, ok = r.template(e, where+" pk", raw.PK, true); !ok {
		return nil
	}
	if raw.SK != "" {
		if ix.SK, ok = r.template(e, where+" sk", raw.SK, true); !ok {
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
	u := &Unique{Name: name, GoName: name, Entity: e, Doc: raw.Doc, Since: e.Version,
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
	c := &Counter{Name: name, GoName: name, Entity: e, Doc: raw.Doc, Shards: raw.Shards, Since: e.Version}
	r.claimType(c.GoName, where)
	r.claimType(c.GoName+"Key", where)
	if c.Shards == 0 {
		c.Shards = 1
	}
	if c.Shards < 1 || c.Shards > 100 {
		r.errorf("%s: shards must be between 1 and 100", where)
	}
	var ok bool
	if c.PK, ok = r.template(e, where+" pk", raw.PK, true); !ok {
		return nil
	}
	if c.SK, ok = r.template(e, where+" sk", raw.SK, true); !ok {
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
		v := &CounterValue{Name: rv.Key, GoName: GoName(rv.Key), Attr: rv.Key, Counter: c, Doc: rv.Value.Doc, Since: e.Version,
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
	a := &Access{Name: name, GoName: name, Entity: e, Doc: raw.Doc, Consistent: raw.Consistent, Rate: raw.Rate}
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
	if kinds != 1 {
		r.errorf("%s: declare exactly one of get, query or counter", where)
		return nil
	}
	switch {
	case raw.Get != nil && raw.Get.Key:
		a.Kind = AccessGet
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
		if raw.Query != "key" {
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
		}
		switch raw.Order {
		case "", "asc":
		case "desc":
			a.Desc = true
		default:
			r.errorf("%s: order must be asc or desc", where)
		}
		a.Page = raw.Page
		if a.Page == 0 {
			a.Page = 50
		}
		a.MaxPage = raw.MaxPage
		if a.MaxPage == 0 {
			a.MaxPage = max(100, a.Page)
		}
		if a.Page < 1 || a.MaxPage < a.Page || a.MaxPage > 1000 {
			r.errorf("%s: need 1 <= page <= max_page <= 1000", where)
		}
		if sk, ok := a.QuerySK(); raw.Range != "" {
			f := e.Field(raw.Range)
			switch {
			case f == nil:
				r.errorf("%s: range field %s is not a field of %s", where, raw.Range, e.Name)
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
	if a.Kind != AccessQuery && (raw.Order != "" || raw.Page != 0 || raw.MaxPage != 0 || raw.Range != "" || raw.Project.Set) {
		r.errorf("%s: order, page, max_page, range and project only apply to queries", where)
	}
	return a
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
	for _, e := range m.Entities {
		for _, f := range e.Fields {
			if f.copyOfRaw == "" {
				continue
			}
			where := fmt.Sprintf("entity %s field %s copy_of", e.Name, f.Name)
			en, fn, ok := strings.Cut(f.copyOfRaw, ".")
			te := byName[en]
			switch {
			case !ok || te == nil:
				r.errorf("%s: %q must be Entity.field, naming an entity of this table", where, f.copyOfRaw)
			case te == e:
				r.errorf("%s: copies within one entity are kept in sync already; copy_of is for another entity's field", where)
			case te.Field(fn) == nil:
				r.errorf("%s: %s has no field %s", where, en, fn)
			case te.Field(fn).Type != f.Type:
				r.errorf("%s: %s.%s is a %s, but %s is a %s", where, en, fn, te.Field(fn).Type, f.Name, f.Type)
			default:
				f.CopyOf, f.CopyOfEntity = te.Field(fn), te
			}
		}
		for _, a := range e.Access {
			if a.Kind != AccessCounter {
				continue
			}
			a.Counter = counters[a.counterRaw]
			if a.Counter == nil {
				r.errorf("entity %s access %s: %s is not a counter of this table", e.Name, a.Name, a.counterRaw)
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
		if len(raw.Set) > 0 || raw.Optional || raw.Consume {
			r.errorf("%s: a counter can only be checked with when (an absent value counts as 0)", where)
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
		if ref, ok := fieldRef(p.Value); ok {
			if sf := r.source(e, where+" when "+p.Key, ref, te, f); sf != nil {
				req.When = append(req.When, &Pred{Field: f, Source: sf})
			}
			continue
		}
		v, err := coerce(f, p.Value)
		if err != nil {
			r.errorf("%s when: %s: %v", where, p.Key, err)
			continue
		}
		req.When = append(req.When, &Pred{Field: f, Value: v})
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
	if req.Optional && len(req.When) == 0 && !req.Writes() {
		r.errorf("%s: optional with no when, set or consume checks nothing", where)
	}
	if len(req.Sets) > 0 && req.Consume {
		r.errorf("%s: set and consume are exclusive: the item is either changed or deleted", where)
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
		req.Fast = !req.Optional && transitionable(tw, changed)
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
		inputs := e.DerivedInputs()
		changed := map[string]bool{}
		for _, f := range w.Changed() {
			changed[f.Name] = true
			if inputs[f.Name] {
				w.ReadFirst = true
			}
		}
		if len(w.Requires) > 0 {
			// Condition checks and changes to other items need a transaction, which the single
			// UpdateItem path cannot be.
			w.ReadFirst = true
		}
		w.Transition = w.ReadFirst && transitionable(w, changed) && requiresKnown(w)
		for _, c := range e.Counters {
			for _, v := range c.Values {
				if v.LimitArg && CanGrow(w, v, false) {
					w.Limits = append(w.Limits, v)
				}
			}
		}
	}
	if len(w.Limits) > 0 {
		r.claimType(e.GoName+w.GoName+"Limits", "entity "+e.Name+" write "+w.Name)
	}
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
		known[p.Field] = true
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
func transitionable(w *Write, changed map[string]bool) bool {
	e := w.Entity
	known := map[*Field]bool{}
	for _, f := range e.KeyFields() {
		known[f] = true
	}
	for _, p := range w.When {
		known[p.Field] = true
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
			if s.Field == p.Field && fmt.Sprint(s.Value) != fmt.Sprint(p.Value) {
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
