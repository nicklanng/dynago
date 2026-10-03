// Package schema parses a dynago schema file, validates it, and resolves it into the model the
// generators and the cost report work from.
package schema

import (
	"fmt"

	"github.com/nicklanng/dynago/internal/keytmpl"
)

// Reserved attribute names written on every item.
const (
	AttrPK   = "PK"
	AttrSK   = "SK"
	AttrType = "_t"
	AttrVer  = "_v"
	AttrRev  = "_rev"
	// AttrCreated and AttrUpdated hold when dynago first wrote an item and last changed it.
	AttrCreated = "_created"
	AttrUpdated = "_updated"
)

// Model is a validated, resolved schema file.
type Model struct {
	Path     string
	Package  string
	Table    Table
	Output   Output
	Entities []*Entity
	GSIs     []*GSI
	// Previous is the generation the migration job copies from, set by the lock.
	Previous *Previous
	Workload Workload
	// Relations are the links between entities: partition nesting, references, requires.
	Relations []*Relation
	// Accepts are findings the schema records as deliberate, each with its reason.
	Accepts []*Acceptance
}

// Entity returns the named entity, or nil.
func (m *Model) Entity(name string) *Entity {
	for _, e := range m.Entities {
		if e.Name == name {
			return e
		}
	}
	return nil
}

// Workload holds the table-wide workload assumptions.
type Workload struct {
	// Peak multiplies every declared (average) rate to give the peak rate. 1 when not declared.
	Peak         float64
	PeakDeclared bool
	// Horizon says when the declared volumes are expected (free text), or "".
	Horizon string
}

// SubjectKind is the kind of schema object a finding is about.
type SubjectKind string

// Subject kinds.
const (
	SubjectTable   SubjectKind = "table"
	SubjectEntity  SubjectKind = "entity"
	SubjectField   SubjectKind = "field"
	SubjectIndex   SubjectKind = "index"
	SubjectUnique  SubjectKind = "unique"
	SubjectCounter SubjectKind = "counter"
	SubjectAccess  SubjectKind = "access"
	SubjectWrite   SubjectKind = "write"
)

// Subject names a schema object: the table, an entity, or something declared on an entity.
type Subject struct {
	Kind   SubjectKind
	Entity string
	Name   string
}

// String renders the subject as people write it: "table", "Loan", "Loan.Borrow".
func (s Subject) String() string {
	switch {
	case s.Kind == SubjectTable:
		return "table"
	case s.Name == "":
		return s.Entity
	}
	return s.Entity + "." + s.Name
}

// Acceptance records a finding as a deliberate decision.
type Acceptance struct {
	Subject Subject
	Rule    string
	Reason  string
	Line    int
}

// RelationKind says how a relation between two entities is known.
type RelationKind string

// Relation kinds.
const (
	// RelNests: the From items live in (or under) the To item's partition.
	RelNests RelationKind = "nests"
	// RelRef: a field of From declares `ref: To`.
	RelRef RelationKind = "ref"
	// RelRequires: a write of From requires a To item.
	RelRequires RelationKind = "requires"
	// RelVolume: From's volume is given per To (volume.per or volume.by).
	RelVolume RelationKind = "volume"
)

// Relation links items of one entity (From) to the item of another (To) whose key they hold.
type Relation struct {
	From, To *Entity
	// Key maps each key field of To (Target) to the From field holding its value (Source).
	Key []RequireKey
	// Kinds lists how the relation is known, in the order found.
	Kinds []RelationKind
	// Writes are the From writes whose requires reach To.
	Writes []*Write
}

// Is reports whether the relation is known in the given way.
func (r *Relation) Is(k RelationKind) bool {
	for _, x := range r.Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// OneToOne reports whether each To item has at most one From item: From's key is To's key.
func (r *Relation) OneToOne() bool {
	fromKey := map[*Field]bool{}
	for _, f := range r.From.KeyFields() {
		fromKey[f] = true
	}
	for _, k := range r.Key {
		delete(fromKey, k.Source)
	}
	return len(fromKey) == 0
}

// Volume is how many items of an entity to expect.
type Volume struct {
	Declared bool
	// Total is the number of items in the table, when given as one number.
	Total float64
	// Per is the parent the per-parent numbers count against (nil with a Total).
	Per *Relation
	// Typical and Max are items per parent item; Max is 0 when unknown.
	Typical, Max float64
	// By gives the spread over other entities the items refer to.
	By []*VolumeBy
}

// VolumeBy is how many items of an entity each item of a related entity has.
type VolumeBy struct {
	Relation     *Relation
	Typical, Max float64
}

// Output holds the paths of generated files, relative to the schema file's directory.
type Output struct {
	Go        string
	Docs      string
	Terraform string
	TableJSON string
	Lock      string
	// MigrateCmd is the directory of the migration job's main package ("" for none).
	MigrateCmd string
}

// Table is the physical table.
type Table struct {
	// Name is the base name: each generation's table is Name-g<Generation>.
	Name    string
	Doc     string
	TTLAttr string
	// Generation is the current table generation (1 for a new schema).
	Generation int
	// Retain lists older generations whose tables are kept for rollback.
	Retain []int
}

// GenerationTable returns the name of a generation's table.
func (t Table) GenerationTable(gen int) string { return fmt.Sprintf("%s-g%d", t.Name, gen) }

// Previous describes the table generation before the current one, which the migration job reads.
// The lock file supplies it; it is nil for a first generation.
type Previous struct {
	Generation int
	// TTLAttr is the attribute DynamoDB's TTL used in that generation's table, if any.
	TTLAttr string
	// Entities maps entity names to their last shape in that generation: the fields to decode.
	Entities map[string][]PreviousField
}

// PreviousField is a field as the previous generation stored it.
type PreviousField struct {
	Name, Attr string
	Type       FieldType
	Values     []string // of an enum
	Required   bool
}

// FieldType is a normalized field type.
type FieldType string

// Supported field types.
const (
	TypeString    FieldType = "string"
	TypeEnum      FieldType = "enum"
	TypeInt       FieldType = "int"
	TypeFloat     FieldType = "float"
	TypeBool      FieldType = "bool"
	TypeTime      FieldType = "time"
	TypeBytes     FieldType = "bytes"
	TypeStringSet FieldType = "string_set"
	TypeList      FieldType = "string_list"
	TypeMap       FieldType = "string_map"
)

// Field is one attribute of an entity.
type Field struct {
	Name    string
	Attr    string
	GoName  string
	Type    FieldType
	Enum    []string
	Doc     string
	Example string
	SizeP50 int
	SizeP99 int
	// Key is true when the field appears in the entity's primary key templates.
	Key bool
	// CopyOf is the field of another entity this field copies (copy_of), which dynago does not
	// keep in sync.
	CopyOf       *Field
	CopyOfEntity *Entity
	copyOfRaw    string
	// SnapshotOf is the field of another entity whose value this field takes when written
	// (snapshot_of), deliberately never updated after.
	SnapshotOf       *Field
	SnapshotOfEntity *Entity
	snapshotOfRaw    string
	// Ref is the entity whose key this field holds (ref), or nil.
	Ref    *Entity
	refRaw string
	// Required fields may not be written with their zero value.
	Required bool
}

// KeyCapable reports whether the field can appear in a key template.
func (f *Field) KeyCapable() bool {
	switch f.Type {
	case TypeString, TypeEnum, TypeInt, TypeTime, TypeBool:
		return true
	}
	return false
}

// Sparse reports whether a zero value of the field means "absent" for key rendering: an index
// entry, copy, claim or counter keyed by an absent field is not written.
func (f *Field) Sparse() bool {
	switch f.Type {
	case TypeString, TypeEnum, TypeTime:
		return true
	}
	return false
}

// Template is a parsed key template with its fields resolved.
type Template struct {
	keytmpl.Template
	Fields []*Field
}

// Pred is a predicate on a field: equal to a constant Value, or, in a write's `requires`, to the
// writing entity's Source field ("{memberId}"). With Not it holds when the field differs from
// that value; with In, when the field equals one of several constants.
type Pred struct {
	Field  *Field
	Value  any
	Source *Field
	// Not inverts the comparison with Value (or Source): `{ not: trash }`.
	Not bool
	// In lists the constants the field may equal: `{ in: [inbox, archived] }`. Value is unused.
	In []any
}

// Pins reports whether the predicate fixes the field to one value: when it holds, the field's
// value is known without reading the item.
func (p *Pred) Pins() bool { return !p.Not && p.In == nil }

// Matches reports whether a field holding the constant v satisfies the predicate. A predicate
// that compares with another entity's field (Source) can't be judged, and reports false.
func (p *Pred) Matches(v any) bool {
	if p.Source != nil {
		return false
	}
	if p.In != nil {
		for _, x := range p.In {
			if sameValue(x, v) {
				return true
			}
		}
		return false
	}
	return sameValue(p.Value, v) != p.Not
}

// Allowed returns the values of an enum or bool field that satisfy the predicate, as the schema
// writes them ("trash", "true"), and whether they can be listed: a predicate on any other type,
// or one comparing with another entity's field, admits values that can't be enumerated.
func (p *Pred) Allowed() ([]string, bool) {
	if p.Source != nil {
		return nil, false
	}
	var domain []any
	switch p.Field.Type {
	case TypeEnum:
		for _, v := range p.Field.Enum {
			domain = append(domain, v)
		}
	case TypeBool:
		domain = []any{false, true}
	default:
		if p.Pins() {
			return []string{fmt.Sprint(p.Value)}, true
		}
		return nil, false
	}
	var out []string
	for _, v := range domain {
		if p.Matches(v) {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out, true
}

func sameValue(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// Entity is a domain type stored as items in the table.
type Entity struct {
	Name     string
	GoName   string
	Doc      string
	Version  int
	Fields   []*Field
	PK, SK   Template
	TTL      *Field
	Indexes  []*Index
	Uniques  []*Unique
	Counters []*Counter
	Access   []*Access
	Writes   []*Write
	Volume   Volume
	// Count is the expected number of items in the table, from the volumes; 0 when unknown.
	Count float64
	// Parent is the relation to the entity the volume counts against, or that the entity's
	// partition nests in; nil for a top-level entity.
	Parent *Relation

	fieldsByName map[string]*Field
	rawVolume    RawVolume
}

// Field returns the named field, or nil.
func (e *Entity) Field(name string) *Field { return e.fieldsByName[name] }

// KeyFields returns the fields of the primary key in template order.
func (e *Entity) KeyFields() []*Field {
	return mergeFields(e.PK.Fields, e.SK.Fields)
}

// NonKeyFields returns the fields not in the primary key.
func (e *Entity) NonKeyFields() []*Field {
	var out []*Field
	for _, f := range e.Fields {
		if !f.Key {
			out = append(out, f)
		}
	}
	return out
}

// HasDerived reports whether any write must maintain items other than the entity's own item.
func (e *Entity) HasDerived() bool {
	for _, ix := range e.Indexes {
		if ix.Strategy == StrategyCopy {
			return true
		}
	}
	return len(e.Uniques) > 0 || len(e.Counters) > 0
}

// DerivedInputs returns the set of field names whose change requires reading the item first:
// they feed counters, claims, copies, or index keys.
func (e *Entity) DerivedInputs() map[string]bool {
	in := map[string]bool{}
	add := func(fs []*Field) {
		for _, f := range fs {
			in[f.Name] = true
		}
	}
	for _, ix := range e.Indexes {
		add(ix.PK.Fields)
		if ix.HasSK {
			add(ix.SK.Fields)
		}
		for _, p := range ix.Where {
			in[p.Field.Name] = true
		}
		if ix.Strategy == StrategyCopy {
			add(ix.ProjectedFields())
		}
	}
	for _, u := range e.Uniques {
		add(u.Fields)
	}
	for _, c := range e.Counters {
		add(c.PK.Fields)
		add(c.SK.Fields)
		for _, v := range c.Values {
			if v.Sum != nil {
				in[v.Sum.Name] = true
			}
			for _, p := range v.Where {
				in[p.Field.Name] = true
			}
		}
	}
	return in
}

// Strategy is how an index is maintained.
type Strategy string

// Index strategies.
const (
	// StrategyGSI keys are attributes on the entity item; DynamoDB maintains a global secondary index.
	StrategyGSI Strategy = "gsi"
	// StrategyCopy writes a separate copy item in the same transaction as the entity item.
	StrategyCopy Strategy = "copy"
)

// Projection modes.
const (
	ProjectAll     = "all"
	ProjectKeys    = "keys"
	ProjectInclude = "include"
)

// Index is an alternative key for reaching an entity.
type Index struct {
	Name       string
	GoName     string
	Entity     *Entity
	Strategy   Strategy
	PK, SK     Template
	HasSK      bool
	Projection string
	Project    []*Field
	Where      []*Pred
	// Matches is the declared share of the entity's items that satisfy Where (0 < share <= 1), or
	// 0 when the schema doesn't say: the analysis then counts every item, as an upper bound.
	Matches float64
	Doc     string
	GSI     *GSI
	// StrategyInferred is true when the schema left strategy out and dynago chose it from the
	// freshness its reads need; StrategyReason says why.
	StrategyInferred bool
	StrategyReason   string
	// PKAttr and SKAttr are the attribute names holding the rendered keys: the GSI's key
	// attributes, or PK/SK for copy items.
	PKAttr, SKAttr string
}

// ProjectedFields returns the entity fields visible through the index, key fields included.
func (ix *Index) ProjectedFields() []*Field {
	if ix.Projection == ProjectAll {
		return ix.Entity.Fields
	}
	fields := mergeFields(ix.Entity.KeyFields(), ix.PK.Fields, ix.SK.Fields)
	if ix.Projection == ProjectInclude {
		fields = mergeFields(fields, ix.Project)
	}
	// Keep entity order.
	set := map[*Field]bool{}
	for _, f := range fields {
		set[f] = true
	}
	var out []*Field
	for _, f := range ix.Entity.Fields {
		if set[f] {
			out = append(out, f)
		}
	}
	return out
}

// Share is the fraction of the entity's items counted in the index: the declared Matches, or all
// of them.
func (ix *Index) Share() float64 {
	if ix.Matches > 0 {
		return ix.Matches
	}
	return 1
}

// Unsized reports whether the index holds only some of the entity's items (it has a where) and
// nothing says how many: its sizes and traffic are then upper bounds.
func (ix *Index) Unsized() bool { return len(ix.Where) > 0 && ix.Matches == 0 }

// Membership says whether an item is in a sparse index.
type Membership int

// Memberships.
const (
	// Maybe: nothing the write says decides it.
	Maybe Membership = iota
	Yes
	No
)

// Members reports whether the item a write touches satisfies the index's where before the write
// and after it, as far as the write itself says: its `when` fixes values before, its `set` fixes
// values after. A create has no item before, a delete none after.
func (ix *Index) Members(w *Write) (before, after Membership) {
	if len(ix.Where) == 0 {
		before, after = Yes, Yes
	} else {
		var b, a []Membership
		for _, p := range ix.Where {
			was := Maybe
			for _, q := range w.When {
				if q.Field == p.Field {
					was = implies(q, p)
				}
			}
			is := was
			for _, f := range append(append([]*Field{}, w.Args...), w.Patch...) {
				if f == p.Field {
					is = Maybe
				}
			}
			for _, s := range w.Sets {
				if s.Field != p.Field {
					continue
				}
				switch {
				case !s.Fixes():
					is = Maybe
				case p.Matches(s.Value):
					is = Yes
				default:
					is = No
				}
			}
			b, a = append(b, was), append(a, is)
		}
		before, after = all(b), all(a)
	}
	switch w.Kind {
	case WriteCreate:
		before = No
	case WriteDelete:
		after = No
	}
	return before, after
}

// implies reports whether a field satisfying q satisfies p.
func implies(q, p *Pred) Membership {
	vs, ok := q.Allowed()
	if !ok || p.Source != nil {
		return Maybe
	}
	n := 0
	for _, v := range vs {
		if p.Matches(v) {
			n++
		}
	}
	switch n {
	case len(vs):
		return Yes
	case 0:
		return No
	}
	return Maybe
}

func all(ms []Membership) Membership {
	out := Yes
	for _, m := range ms {
		switch m {
		case No:
			return No
		case Maybe:
			out = Maybe
		}
	}
	return out
}

// ReturnsEntity reports whether a query through the index returns whole entities.
func (ix *Index) ReturnsEntity() bool { return ix.Projection == ProjectAll }

// GSI is a physical global secondary index, possibly shared by several entities.
type GSI struct {
	Name        string
	PKAttr      string
	SKAttr      string
	HasSK       bool
	Projection  string
	NonKeyAttrs []string
	Users       []*Index
}

// Unique is a uniqueness constraint enforced by a claim item.
type Unique struct {
	Name   string
	GoName string
	Entity *Entity
	Fields []*Field
	// Set, if not nil, is the one string_set field among Fields: each of its elements is claimed.
	Set     *Field
	PK, SK  Template
	Doc     string
	ErrName string
}

// Counter is an atomic counter item maintained by the entity's writes.
type Counter struct {
	Name   string
	GoName string
	Entity *Entity
	PK, SK Template
	Shards int
	Values []*CounterValue
	Doc    string
}

// KeyFields returns the fields that address the counter item.
func (c *Counter) KeyFields() []*Field { return mergeFields(c.PK.Fields, c.SK.Fields) }

// CounterValue is one attribute of a counter.
type CounterValue struct {
	Name     string
	GoName   string
	Attr     string
	Counter  *Counter
	Sum      *Field
	Where    []*Pred
	Limit    int64
	LimitArg bool
	// HasMin marks a lower bound: writes that would take the value below Min fail.
	HasMin     bool
	Min        int64
	MinErrName string
	Doc        string
	ErrName    string
}

// Limited reports whether increments are bounded.
func (v *CounterValue) Limited() bool { return v.Limit > 0 || v.LimitArg }

// AccessKind is the kind of a read.
type AccessKind string

// Access kinds.
const (
	AccessGet       AccessKind = "get"
	AccessGetUnique AccessKind = "get_unique"
	AccessQuery     AccessKind = "query"
	AccessCounter   AccessKind = "counter"
	AccessScan      AccessKind = "scan"
)

// Freshness is how up to date a read must be.
type Freshness string

// Freshness requirements.
const (
	// FreshnessUnstated: the schema doesn't say.
	FreshnessUnstated Freshness = ""
	// FreshnessImmediate: a caller must see its own writes (read-your-writes).
	FreshnessImmediate Freshness = "immediate"
	// FreshnessEventual: a moment's lag is fine.
	FreshnessEventual Freshness = "eventual"
)

// Access is a declared read.
type Access struct {
	Name       string
	GoName     string
	Entity     *Entity
	Kind       AccessKind
	Unique     *Unique
	Index      *Index // nil for a query on the entity's own partition
	Counter    *Counter
	Desc       bool
	Page       int
	MaxPage    int
	Range      *Field
	Consistent bool
	Freshness  Freshness
	// Reason says why a scan is needed.
	Reason string
	// Project lists the fields a query on the entity's own partition reads (empty: all).
	Project []*Field
	Doc     string
	Rate    float64

	counterRaw string
}

// ProjectedFields returns what a projected base query returns: key fields plus Project, in
// entity order.
func (a *Access) ProjectedFields() []*Field {
	set := map[*Field]bool{}
	for _, f := range mergeFields(a.Entity.KeyFields(), a.Project) {
		set[f] = true
	}
	var out []*Field
	for _, f := range a.Entity.Fields {
		if set[f] {
			out = append(out, f)
		}
	}
	return out
}

// QueryPK returns the partition key template the access queries.
func (a *Access) QueryPK() Template {
	if a.Index != nil {
		return a.Index.PK
	}
	return a.Entity.PK
}

// QuerySK returns the sort key template the access queries, and whether there is one.
func (a *Access) QuerySK() (Template, bool) {
	if a.Index != nil {
		return a.Index.SK, a.Index.HasSK
	}
	return a.Entity.SK, true
}

// WriteKind is the kind of a write.
type WriteKind string

// Write kinds.
const (
	WriteCreate WriteKind = "create"
	WriteUpdate WriteKind = "update"
	WriteDelete WriteKind = "delete"
)

// SetConst is a field set by a write to a constant Value, or, in a write's `requires`, to the
// writing entity's Source field. A requirement can also add to the field (Add), or set it only
// when the writer's field has a value (IfSet).
type SetConst struct {
	Field  *Field
	Value  any
	Source *Field
	// Add adds the int Value (or Source) to the field instead of replacing it.
	Add bool
	// IfSet leaves the field as it is when Source holds its zero value.
	IfSet bool
}

// Fixes reports whether the change gives the field a constant, known without reading anything.
func (s SetConst) Fixes() bool { return s.Source == nil && !s.Add }

// Write is a declared write.
type Write struct {
	Name   string
	GoName string
	Entity *Entity
	Kind   WriteKind
	Args   []*Field
	// Patch fields are optional arguments: nil leaves the field unchanged.
	Patch       []*Field
	Sets        []SetConst
	When        []*Pred
	Requires    []*Require
	rawRequires Ordered[RawRequire]
	// Transition is true when a read-first update can instead run as conditional writes without
	// reading: every derived value it changes depends only on key fields and fields its `when`
	// pins, so the before and after contributions are known in advance.
	Transition bool
	Doc        string
	Rate       float64
	HotKeyRate float64
	// ReadFirst is true when the write must read the item to maintain derived items.
	ReadFirst bool
	// VersionRequired is true when callers must say which version they read (versioned: required).
	VersionRequired bool
	// Limits are counter values with caller-supplied limits this write can increase.
	Limits []*CounterValue
}

// Require is a condition on another item, checked in the same transaction as the write: an
// entity (Target), which the write may also change (Sets) or delete (Consume), or a counter
// (Counter), whose values must equal constants.
type Require struct {
	Name    string
	Target  *Entity
	Counter *Counter
	// Key maps each key field of the target (or counter) to the field of this entity that holds
	// its value.
	Key         []RequireKey
	When        []*Pred
	CounterWhen []CounterPred
	Sets        []SetConst
	// Optional lets the write go ahead when the target is absent or expired.
	Optional bool
	Consume  bool
	// Fast is true when the change to the target can be written without reading it first: its
	// derived changes are known from its key and the state `when` pins. Otherwise it is read.
	Fast    bool
	ErrName string
}

// Writes reports whether the requirement changes its target, rather than only checking it.
func (rq *Require) Writes() bool { return len(rq.Sets) > 0 || rq.Consume }

// Sources returns the fields of the writing entity the requirement reads: its key and references.
func (rq *Require) Sources() []*Field {
	var out []*Field
	for _, k := range rq.Key {
		out = append(out, k.Source)
	}
	for _, p := range rq.When {
		if p.Source != nil {
			out = append(out, p.Source)
		}
	}
	for _, s := range rq.Sets {
		if s.Source != nil {
			out = append(out, s.Source)
		}
	}
	return mergeFields(out)
}

// CounterPred requires a counter value to equal a constant; an absent value counts as zero.
type CounterPred struct {
	Value  *CounterValue
	Equals int64
}

// RequireKey is one key field of a required entity and where its value comes from.
type RequireKey struct {
	Target *Field
	Source *Field
}

// Changed returns every field the update writes.
func (w *Write) Changed() []*Field {
	out := append([]*Field{}, w.Args...)
	out = append(out, w.Patch...)
	for _, s := range w.Sets {
		out = append(out, s.Field)
	}
	return out
}

func mergeFields(lists ...[]*Field) []*Field {
	var out []*Field
	seen := map[*Field]bool{}
	for _, l := range lists {
		for _, f := range l {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}
