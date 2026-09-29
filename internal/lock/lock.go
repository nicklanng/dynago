// Package lock keeps the history of each entity's storage shape, and of the table generations, in
// a checked-in lock file.
//
// The storage shape is everything that decides what items look like: fields and attribute names,
// key templates, indexes, uniqueness claims and counters. Changing it requires bumping the
// entity's version. Within one table generation, a version may only change the shape in ways
// existing items still fit, such as adding an optional field or removing a field. Any
// other change needs a new generation: a new table, filled by the generated migration job, with
// the old one kept for rollback. The history tells the generator what the previous generation
// stored, so the job can read it.
package lock

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/internal/gen/infra"
	"github.com/nicklanng/dynago/internal/schema"
)

// File is the lock file.
type File struct {
	Dynago int `json:"dynago"`
	// Format is the lock file's own format: 2 records whether each field is required. Older
	// files are upgraded, taking what they didn't record from the schema.
	Format int `json:"format,omitempty"`
	// Generation is the table generation the schema was last generated at (0 in lock files
	// written before generations existed, meaning 1).
	Generation int `json:"generation,omitempty"`
	// Tables records each generation's physical table, so Terraform can keep declaring the
	// tables of retained generations.
	Tables   []TableRecord       `json:"tables,omitempty"`
	Entities map[string]*History `json:"entities"`
}

// TableRecord is one generation's physical table.
type TableRecord struct {
	Generation int              `json:"generation"`
	Spec       dynago.TableSpec `json:"spec"`
}

// History is the recorded versions of one entity, oldest first.
type History struct {
	Versions []Version `json:"versions"`
}

// Version is one recorded storage shape.
type Version struct {
	Version int `json:"version"`
	// Generation is the table generation the version was recorded in (0 meaning 1).
	Generation  int    `json:"generation,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Shape       Shape  `json:"shape"`
	// StartsGeneration marks a version recorded with a new generation: the migration job copied
	// that generation's items in with this shape, not an earlier one.
	StartsGeneration bool `json:"startsGeneration,omitempty"`
}

func (v Version) gen() int { return max(v.Generation, 1) }

// Shape is the storage-relevant part of an entity definition.
type Shape struct {
	Fields []FieldShape `json:"fields"`
	PK     string       `json:"pk"`
	SK     string       `json:"sk"`
	TTL    string       `json:"ttl,omitempty"`
	// TTLAttr is the table's TTL attribute, which items of an expiring entity store their expiry in.
	TTLAttr  string         `json:"ttl_attribute,omitempty"`
	Indexes  []IndexShape   `json:"indexes,omitempty"`
	Uniques  []UniqueShape  `json:"uniques,omitempty"`
	Counters []CounterShape `json:"counters,omitempty"`
}

// FieldShape is a stored attribute.
type FieldShape struct {
	Name     string   `json:"name"`
	Attr     string   `json:"attr"`
	Type     string   `json:"type"`
	Values   []string `json:"values,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// lockFormat is the format this code writes.
const lockFormat = 2

// IndexShape is an index definition.
type IndexShape struct {
	Name       string   `json:"name"`
	Strategy   string   `json:"strategy"`
	PK         string   `json:"pk"`
	SK         string   `json:"sk,omitempty"`
	Projection string   `json:"projection"`
	Project    []string `json:"project,omitempty"`
	Where      []string `json:"where,omitempty"`
}

// UniqueShape is a uniqueness claim definition.
type UniqueShape struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
	PK     string   `json:"pk"`
	SK     string   `json:"sk"`
}

// CounterShape is a counter definition. Limits are not part of it: they constrain writes but do
// not change what is stored.
type CounterShape struct {
	Name   string        `json:"name"`
	PK     string        `json:"pk"`
	SK     string        `json:"sk"`
	Shards int           `json:"shards"`
	Values []CounterAttr `json:"values"`
}

// CounterAttr is one counter value definition.
type CounterAttr struct {
	Name  string   `json:"name"`
	Sum   string   `json:"sum,omitempty"`
	Where []string `json:"where,omitempty"`
}

// Read loads a lock file; a missing file is an empty lock.
func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{Dynago: 1, Entities: map[string]*History{}}, nil
	}
	if err != nil {
		return nil, err
	}
	f := &File{}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Entities == nil {
		f.Entities = map[string]*History{}
	}
	return f, nil
}

// Marshal renders the lock file deterministically.
func (f *File) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Note is something the user should know about a schema change.
type Note struct {
	Entity  string
	Warning bool
	Message string
}

// Options adjusts Apply.
type Options struct {
	// NewHistory accepts a schema whose history the lock file doesn't have (entities above
	// version 1, or a table above generation 1), starting the history there. Only right when the
	// lock file was lost and nothing it would have recorded matters.
	NewHistory bool
}

// Apply checks the model against the lock and returns the updated lock (the input is not
// modified). It sets the model's Previous generation for the migration job.
func Apply(m *schema.Model, prev *File, opts Options) (*File, []Note, error) {
	gen := m.Table.Generation
	prevGen := prev.Generation
	if prevGen == 0 && len(prev.Entities) > 0 {
		prevGen = 1 // written before generations existed
	}
	next := &File{Dynago: 1, Format: lockFormat, Generation: gen, Tables: slices.Clone(prev.Tables), Entities: map[string]*History{}}
	for name, h := range prev.Entities {
		next.Entities[name] = &History{Versions: slices.Clone(h.Versions)}
	}
	newGen := prevGen != 0 && gen != prevGen
	var notes []Note
	var errs []error
	switch {
	case prevGen == 0 && gen > 1 && !opts.NewHistory:
		errs = append(errs, fmt.Errorf("table: it is at generation %d, but the lock file has no history. Restore %s from version control, or run with -new-history to start the history here", gen, m.Output.Lock))
	case gen < prevGen:
		errs = append(errs, fmt.Errorf("table: generation %d is older than the recorded generation %d; generations only go up", gen, prevGen))
	case newGen && gen != prevGen+1:
		errs = append(errs, fmt.Errorf("table: generation goes from %d to %d; it must go up by one, since the migration job copies from the previous generation", prevGen, gen))
	}
	for _, e := range m.Entities {
		shape := ShapeOf(e, m.Table.TTLAttr)
		fp := fingerprint(shape)
		h := next.Entities[e.Name]
		if h == nil {
			h = &History{}
			next.Entities[e.Name] = h
		}
		n := len(h.Versions)
		if n == 0 {
			if e.Version > 1 && !opts.NewHistory {
				errs = append(errs, fmt.Errorf("entity %s: it is at version %d, but the lock file has no history for it. Restore %s from version control, or run with -new-history to start its history here",
					e.Name, e.Version, m.Output.Lock))
				continue
			}
			h.Versions = append(h.Versions, Version{e.Version, gen, fp, shape, newGen})
			continue
		}
		last := h.Versions[n-1]
		// Compare shapes, not the stored fingerprint, so a lock written by an older dynago
		// (another fingerprint, or fields it didn't record yet) stays valid. A matching entry is
		// refreshed in the current format.
		if prev.Format < 2 {
			last.Shape = upgradeRequired(last.Shape, shape)
		}
		same := fingerprint(upgrade(last.Shape, shape)) == fp
		switch {
		case e.Version < last.Version:
			errs = append(errs, fmt.Errorf("entity %s: version %d is older than the recorded version %d; versions only go up", e.Name, e.Version, last.Version))
		case e.Version == last.Version && same:
			h.Versions[n-1] = Version{last.Version, last.gen(), fp, shape, last.StartsGeneration}
		case e.Version == last.Version:
			errs = append(errs, fmt.Errorf("entity %s: its storage shape changed but its version is still %d. Set `version: %d` so stored items record which shape wrote them. Changes: %s",
				e.Name, e.Version, e.Version+1, strings.Join(texts(Changes(last.Shape, shape, e)), "; ")))
		default:
			changes := Changes(last.Shape, shape, e)
			if !newGen {
				// A new generation's table holds only what the migration copies in, in this shape.
				changes = append(changes, reusedAttrs(h, gen, shape)...)
			}
			var misfits []string
			for _, c := range changes {
				if !c.Compatible {
					misfits = append(misfits, c.Text)
				}
			}
			if len(misfits) > 0 && !newGen {
				errs = append(errs, fmt.Errorf("entity %s: existing items don't fit version %d: %s. Start a new table generation (`table.generation: %d`): the generated migration job copies every item into the new table, and the old one stays for rollback",
					e.Name, e.Version, strings.Join(misfits, "; "), gen+1))
				continue
			}
			if len(changes) == 0 {
				notes = append(notes, Note{e.Name, false, fmt.Sprintf("version %d → %d without a storage change", last.Version, e.Version)})
			}
			for _, c := range changes {
				notes = append(notes, Note{e.Name, !c.Compatible, fmt.Sprintf("v%d → v%d: %s", last.Version, e.Version, c.Text)})
			}
			h.Versions = append(h.Versions, Version{e.Version, gen, fp, shape, newGen})
		}
	}
	if newGen && len(errs) == 0 {
		notes = append(notes, Note{"table", true, fmt.Sprintf("generation %d → %d: a new table, %s. Run the migration job to copy %s into it before this version serves; %s stays for rollback until you remove it",
			prevGen, gen, m.Table.GenerationTable(gen), m.Table.GenerationTable(prevGen), m.Table.GenerationTable(prevGen))})
	}
	// Record this generation's table, and check retained ones are known.
	spec := infra.Spec(m)
	next.Tables = slices.DeleteFunc(next.Tables, func(t TableRecord) bool { return t.Generation == gen })
	next.Tables = append(next.Tables, TableRecord{gen, spec})
	sort.Slice(next.Tables, func(i, j int) bool { return next.Tables[i].Generation < next.Tables[j].Generation })
	for _, g := range m.Table.Retain {
		if next.Table(g) == nil {
			errs = append(errs, fmt.Errorf("table.retain: the lock file has no record of generation %d's table", g))
		}
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	m.Previous = previous(m, next)
	return next, notes, nil
}

// Table returns the record of a generation's table, or nil.
func (f *File) Table(gen int) *dynago.TableSpec {
	for i := range f.Tables {
		if f.Tables[i].Generation == gen {
			return &f.Tables[i].Spec
		}
	}
	return nil
}

// previous describes the generation before the current one: each entity's last shape in it.
// Entities new in the current generation have none.
func previous(m *schema.Model, f *File) *schema.Previous {
	gen := m.Table.Generation
	if gen <= 1 {
		return nil
	}
	p := &schema.Previous{Generation: gen - 1, TTLAttr: m.Table.TTLAttr, Entities: map[string][]schema.PreviousField{}}
	if spec := f.Table(gen - 1); spec != nil {
		p.TTLAttr = spec.TTLAttr
	}
	for _, e := range m.Entities {
		h := f.Entities[e.Name]
		if h == nil {
			continue
		}
		var last *Version
		for i := range h.Versions {
			if h.Versions[i].gen() < gen {
				last = &h.Versions[i]
			}
		}
		if last == nil {
			continue
		}
		var fields []schema.PreviousField
		for _, fs := range last.Shape.Fields {
			fields = append(fields, schema.PreviousField{Name: fs.Name, Attr: fs.Attr, Type: schema.FieldType(fs.Type), Values: fs.Values, Required: fs.Required})
		}
		p.Entities[e.Name] = fields
	}
	return p
}

// ShapeOf extracts the storage shape of an entity.
func ShapeOf(e *schema.Entity, ttlAttr string) Shape {
	s := Shape{PK: e.PK.Raw, SK: e.SK.Raw}
	for _, f := range e.Fields {
		s.Fields = append(s.Fields, FieldShape{Name: f.Name, Attr: f.Attr, Type: string(f.Type), Values: f.Enum, Required: f.Required})
	}
	if e.TTL != nil {
		s.TTL, s.TTLAttr = e.TTL.Name, ttlAttr
	}
	for _, ix := range e.Indexes {
		is := IndexShape{Name: ix.Name, Strategy: string(ix.Strategy), PK: ix.PK.Raw, Projection: ix.Projection, Where: preds(ix.Where)}
		if ix.HasSK {
			is.SK = ix.SK.Raw
		}
		for _, f := range ix.Project {
			is.Project = append(is.Project, f.Name)
		}
		s.Indexes = append(s.Indexes, is)
	}
	for _, u := range e.Uniques {
		us := UniqueShape{Name: u.Name, PK: u.PK.Raw, SK: u.SK.Raw}
		for _, f := range u.Fields {
			us.Fields = append(us.Fields, f.Name)
		}
		s.Uniques = append(s.Uniques, us)
	}
	for _, c := range e.Counters {
		cs := CounterShape{Name: c.Name, PK: c.PK.Raw, SK: c.SK.Raw, Shards: c.Shards}
		for _, v := range c.Values {
			ca := CounterAttr{Name: v.Attr, Where: preds(v.Where)}
			if v.Sum != nil {
				ca.Sum = v.Sum.Name
			}
			cs.Values = append(cs.Values, ca)
		}
		s.Counters = append(s.Counters, cs)
	}
	return s
}

func preds(ps []*schema.Pred) []string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s=%v", p.Field.Name, p.Value))
	}
	return out
}

// Change is one difference between two versions of a shape. Compatible changes leave existing
// items valid, so they can happen within a table generation; others need a new generation.
type Change struct {
	Text       string
	Compatible bool
}

func texts(cs []Change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Text
	}
	return out
}

// Changes describes the changes from shape a to shape b of entity e, one per change.
func Changes(a, b Shape, e *schema.Entity) []Change {
	var out []Change
	add := func(compatible bool, format string, args ...any) {
		out = append(out, Change{fmt.Sprintf(format, args...), compatible})
	}
	if a.PK != b.PK || a.SK != b.SK {
		add(false, "primary key changed from %s / %s to %s / %s", a.PK, a.SK, b.PK, b.SK)
	}
	if a.TTL != b.TTL {
		add(false, "ttl field changed from %q to %q", a.TTL, b.TTL)
	}
	if a.TTLAttr != "" && b.TTLAttr != "" && a.TTLAttr != b.TTLAttr {
		add(false, "table TTL attribute changed from %q to %q (existing items keep their expiry in the old one)", a.TTLAttr, b.TTLAttr)
	}
	// Fields.
	oldFields := map[string]FieldShape{}
	for _, f := range a.Fields {
		oldFields[f.Name] = f
	}
	for _, f := range b.Fields {
		old, had := oldFields[f.Name]
		delete(oldFields, f.Name)
		switch {
		case !had && e != nil && e.Field(f.Name) != nil && e.Field(f.Name).Required:
			add(false, "field %s added as required (existing items don't have it)", f.Name)
		case !had:
			add(true, "field %s added", f.Name)
		case !old.Required && f.Required:
			add(false, "field %s made required (existing items may not have it)", f.Name)
		case old.Attr != f.Attr || old.Type != f.Type:
			add(false, "field %s changed from %s %s to %s %s (existing items hold the old one)", f.Name, old.Type, old.Attr, f.Type, f.Attr)
		case mustJSON(old.Values) != mustJSON(f.Values):
			var dropped []string
			for _, v := range old.Values {
				if !slices.Contains(f.Values, v) {
					dropped = append(dropped, v)
				}
			}
			if len(dropped) > 0 {
				add(false, "enum %s no longer has %s (existing items may hold them)", f.Name, strings.Join(dropped, ", "))
			} else {
				add(true, "enum %s gained values", f.Name)
			}
		}
	}
	for _, name := range sortedKeys(oldFields) {
		add(true, "field %s removed (existing items keep the attribute until the next generation)", name)
	}
	// Derived items: existing items have none of a new or changed one. A dropped one is no better:
	// during a rolling deploy or after a rollback, the version that still has it keeps maintaining
	// it while this one doesn't, so its counts drift and its claims stop protecting anything.
	derived := func(kind string, a, b map[string]string) {
		for _, name := range sortedKeys(b) {
			switch av, had := a[name]; {
			case !had:
				add(false, "%s %s added (existing items don't have it)", kind, name)
			case av != b[name]:
				add(false, "%s %s changed (existing items have the old one)", kind, name)
			}
		}
		for _, name := range sortedKeys(a) {
			if _, has := b[name]; !has {
				add(false, "%s %s removed (a version that still has it would keep maintaining it while this one doesn't)", kind, name)
			}
		}
	}
	derived("index", toMap(a.Indexes, func(x IndexShape) string { return x.Name }), toMap(b.Indexes, func(x IndexShape) string { return x.Name }))
	derived("unique claim", toMap(a.Uniques, func(x UniqueShape) string { return x.Name }), toMap(b.Uniques, func(x UniqueShape) string { return x.Name }))
	counters := func(s Shape) map[string]string {
		out := map[string]string{}
		for _, c := range s.Counters {
			for _, v := range c.Values {
				out[c.Name+"."+v.Name] = mustJSON([]any{c.PK, c.SK, c.Shards, v})
			}
		}
		return out
	}
	derived("counter value", counters(a), counters(b))
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toMap[T any](xs []T, key func(T) string) map[string]string {
	m := map[string]string{}
	for _, x := range xs {
		m[key(x)] = mustJSON(x)
	}
	return m
}

// reusedAttrs finds fields whose attribute held another type earlier in the table generation: a
// field removed and later re-added with another type, or one attribute reused by a new field.
// Existing items may still hold the old value.
func reusedAttrs(h *History, gen int, now Shape) []Change {
	// The versions whose items the table can hold: those recorded in this generation, and, unless
	// the entity's version changed with the generation, the last one before it (the shape the
	// migration copied in).
	started := slices.ContainsFunc(h.Versions, func(v Version) bool { return v.gen() == gen && v.StartsGeneration })
	var versions []Version
	for i, v := range h.Versions {
		carried := !started && v.gen() < gen && (i+1 == len(h.Versions) || h.Versions[i+1].gen() == gen)
		if v.gen() == gen || carried {
			versions = append(versions, v)
		}
	}
	var out []Change
	for _, f := range now.Fields {
		for _, v := range versions {
			for _, old := range v.Shape.Fields {
				switch {
				case old.Attr != f.Attr:
				case old.Type != f.Type:
					out = append(out, Change{fmt.Sprintf("attribute %q of field %s held a %s (field %s at version %d) earlier in this generation, so existing items may still hold one", f.Attr, f.Name, old.Type, old.Name, v.Version), false})
				case f.Type == string(schema.TypeEnum):
					for _, val := range old.Values {
						if !slices.Contains(f.Values, val) {
							out = append(out, Change{fmt.Sprintf("attribute %q of enum %s held %q (field %s at version %d) earlier in this generation, which the enum no longer has", f.Attr, f.Name, val, old.Name, v.Version), false})
						}
					}
				}
			}
		}
	}
	return dedupeChanges(out)
}

func dedupeChanges(cs []Change) []Change {
	var out []Change
	seen := map[string]bool{}
	for _, c := range cs {
		if !seen[c.Text] {
			seen[c.Text] = true
			out = append(out, c)
		}
	}
	return out
}

// upgradeRequired fills in whether each field was required, which lock files before format 2
// didn't record, from the current shape: an absence there is not a change.
func upgradeRequired(old, now Shape) Shape {
	required := map[string]bool{}
	for _, f := range now.Fields {
		required[f.Name] = f.Required
	}
	old.Fields = slices.Clone(old.Fields)
	for i := range old.Fields {
		old.Fields[i].Required = required[old.Fields[i].Name]
	}
	return old
}

// upgrade fills in what a lock written by an older dynago didn't record, taking it from the
// current shape: an absence there is not a change.
func upgrade(old, now Shape) Shape {
	if old.TTL != "" && old.TTLAttr == "" {
		old.TTLAttr = now.TTLAttr
	}
	return old
}

// fingerprint identifies a shape regardless of declaration order: reordering fields, indexes,
// claims or counters in the schema changes nothing stored.
func fingerprint(s Shape) string {
	c := s
	c.Fields = sortedBy(s.Fields, func(x FieldShape) string { return x.Name })
	c.Indexes = sortedBy(s.Indexes, func(x IndexShape) string { return x.Name })
	c.Uniques = sortedBy(s.Uniques, func(x UniqueShape) string { return x.Name })
	c.Counters = sortedBy(s.Counters, func(x CounterShape) string { return x.Name })
	for i := range c.Counters {
		c.Counters[i].Values = sortedBy(c.Counters[i].Values, func(x CounterAttr) string { return x.Name })
	}
	sum := sha256.Sum256([]byte(mustJSON(c)))
	return hex.EncodeToString(sum[:8])
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func sortedBy[T any](xs []T, key func(T) string) []T {
	out := append([]T(nil), xs...)
	sort.SliceStable(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}
