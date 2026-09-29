// Package lock keeps the history of each entity's storage shape in a checked-in lock file.
//
// The storage shape is everything that decides what items look like: fields and attribute
// names, key templates, indexes, uniqueness claims and counters. Changing it requires bumping the
// entity's version, so every stored item's _v says which shape wrote it. The history also tells
// the generator which version introduced each derived item (counter, claim, copy): items written
// before that version never contributed to it, so writes must not subtract their "contribution".
package lock

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nicklanng/dynago/internal/schema"
)

// File is the lock file.
type File struct {
	Dynago   int                 `json:"dynago"`
	Entities map[string]*History `json:"entities"`
}

// History is the recorded versions of one entity, oldest first.
type History struct {
	Versions []Version `json:"versions"`
}

// Version is one recorded storage shape.
type Version struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	Shape       Shape  `json:"shape"`
}

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
	Name   string   `json:"name"`
	Attr   string   `json:"attr"`
	Type   string   `json:"type"`
	Values []string `json:"values,omitempty"`
}

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
	// NewHistory accepts entities above version 1 with no recorded history, starting their
	// history at the current version. Only right when no stored item predates that version.
	NewHistory bool
}

// Apply checks the model against the lock, records new versions, and sets the Since version of
// every derived item on the model. It returns the updated lock (the input is not modified).
func Apply(m *schema.Model, prev *File, opts Options) (*File, []Note, error) {
	next := &File{Dynago: 1, Entities: map[string]*History{}}
	for name, h := range prev.Entities {
		next.Entities[name] = &History{Versions: append([]Version{}, h.Versions...)}
	}
	var notes []Note
	var errs []error
	for _, e := range m.Entities {
		shape := ShapeOf(e, m.Table.TTLAttr)
		fp := fingerprint(shape)
		h := next.Entities[e.Name]
		if h == nil {
			h = &History{}
			next.Entities[e.Name] = h
		}
		if n := len(h.Versions); n > 0 {
			last := h.Versions[n-1]
			// Compare shapes, not the stored fingerprint, so a lock written by an older dynago
			// (another fingerprint, or fields it didn't record yet) stays valid. A matching entry
			// is refreshed in the current format.
			if e.Version == last.Version && fingerprint(upgrade(last.Shape, shape)) == fp {
				h.Versions[n-1] = Version{e.Version, fp, shape}
				setSince(e, h)
				continue
			}
			switch {
			case e.Version < last.Version:
				errs = append(errs, fmt.Errorf("entity %s: version %d is older than the recorded version %d; versions only go up", e.Name, e.Version, last.Version))
				continue
			case e.Version == last.Version:
				errs = append(errs, fmt.Errorf("entity %s: its storage shape changed but its version is still %d. Set `version: %d` so stored items record which shape wrote them. Changes: %s",
					e.Name, e.Version, e.Version+1, strings.Join(Diff(last.Shape, shape), "; ")))
				continue
			case e.Version > last.Version:
				if bad := inPlaceChanges(last.Shape, shape); len(bad) > 0 {
					errs = append(errs, fmt.Errorf("entity %s: %s", e.Name, strings.Join(bad, "; ")))
					continue
				}
				changes := Diff(last.Shape, shape)
				if len(changes) == 0 {
					notes = append(notes, Note{e.Name, false, fmt.Sprintf("version %d → %d without a storage change", last.Version, e.Version)})
				}
				for _, c := range changes {
					notes = append(notes, Note{e.Name, migrationNeeded(c), fmt.Sprintf("v%d → v%d: %s", last.Version, e.Version, c)})
				}
				h.Versions = append(h.Versions, Version{e.Version, fp, shape})
			}
		} else {
			if e.Version > 1 && !opts.NewHistory {
				// The history decides which stored items count towards each counter, claim and
				// copy. Starting it now would treat everything as introduced at this version, so
				// deleting an older item would never release what it contributed.
				errs = append(errs, fmt.Errorf("entity %s: it is at version %d, but the lock file has no history for it. Restore %s from version control. If no stored %s predates version %d (a new entity or a new table), run with -new-history to start its history there",
					e.Name, e.Version, m.Output.Lock, e.Name, e.Version))
				continue
			}
			h.Versions = append(h.Versions, Version{e.Version, fp, shape})
		}
		setSince(e, h)
	}
	errs = append(errs, lateLimits(m)...)
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	return next, notes, nil
}

// lateLimits refuses counter values with caller-supplied limits added after version 1 to an
// entity that other writes change through requires. Items older than the value start counting
// when they are next rewritten, so such a change can grow the value, but only the entity's own
// writes take limits: the other write would fail with ErrLimitRequired until a backfill.
func lateLimits(m *schema.Model) []error {
	var errs []error
	for _, e := range m.Entities {
		for _, w := range e.Writes {
			for _, rq := range w.Requires {
				if rq.Target == nil || len(rq.Sets) == 0 {
					continue
				}
				for _, c := range rq.Target.Counters {
					for _, v := range c.Values {
						if v.LimitArg && v.Since > 1 && schema.CanGrow(rq.TargetWrite(), v, true) {
							errs = append(errs, fmt.Errorf("entity %s: counter value %s.%s (limit: arg) is newer than version 1, and %s.%s changes %s items through requires, where no limit can be given: items from before version %d start counting when changed, so %s.%s would fail with ErrLimitRequired. Use a constant limit instead",
								rq.Target.Name, c.Name, v.Name, e.Name, w.Name, rq.Target.Name, v.Since, e.Name, w.Name))
						}
					}
				}
			}
		}
	}
	return errs
}

// setSince finds, for each derived item, the oldest version from which its definition has been
// unchanged up to now.
func setSince(e *schema.Entity, h *History) {
	since := func(key string) int {
		v := e.Version
		for i := len(h.Versions) - 1; i >= 0; i-- {
			if !derivedKeys(h.Versions[i].Shape)[key] {
				break
			}
			v = h.Versions[i].Version
		}
		return v
	}
	shape := ShapeOf(e, "") // only its derived items matter here
	for i, ix := range e.Indexes {
		ix.Since = since(indexKey(shape.Indexes[i]))
	}
	for i, u := range e.Uniques {
		u.Since = since(uniqueKey(shape.Uniques[i]))
	}
	for i, c := range e.Counters {
		cs := shape.Counters[i]
		c.Since = since(counterKey(cs))
		for j, v := range c.Values {
			v.Since = since(counterValueKey(cs, cs.Values[j]))
		}
	}
	// An item written before a limited value existed starts contributing the next time any
	// read-first write rewrites it, so every read-first write must take that value's limit.
	for _, w := range e.Writes {
		if !w.ReadFirst {
			continue
		}
		for _, c := range e.Counters {
			for _, v := range c.Values {
				if v.LimitArg && v.Since > 1 && schema.CanGrow(w, v, true) && !hasValue(w.Limits, v) {
					w.Limits = append(w.Limits, v)
				}
			}
		}
	}
}

func hasValue(vs []*schema.CounterValue, v *schema.CounterValue) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// inPlaceChanges lists changes to derived items that existing items would be stranded by. Code
// only knows the current definitions, so it cannot release a copy, claim or counter contribution
// that an older item made under a different definition. Such changes must be made as a new
// derived item under a new name (and the old one removed).
func inPlaceChanges(a, b Shape) []string {
	var out []string
	oldCopies := map[string]IndexShape{}
	for _, ix := range a.Indexes {
		if ix.Strategy == string(schema.StrategyCopy) {
			oldCopies[ix.Name] = ix
		}
	}
	for _, ix := range b.Indexes {
		if old, ok := oldCopies[ix.Name]; ok && mustJSON(old) != mustJSON(ix) {
			out = append(out, fmt.Sprintf("copy index %s cannot change in place (older items' copies would never be removed); add it under a new name and remove the old one", ix.Name))
		}
	}
	oldUniques := map[string]UniqueShape{}
	for _, u := range a.Uniques {
		oldUniques[u.Name] = u
	}
	for _, u := range b.Uniques {
		if old, ok := oldUniques[u.Name]; ok && mustJSON(old) != mustJSON(u) {
			out = append(out, fmt.Sprintf("unique claim %s cannot change in place (older items' claims would never be released); add it under a new name and remove the old one", u.Name))
		}
	}
	oldCounters := map[string]CounterShape{}
	for _, c := range a.Counters {
		oldCounters[c.Name] = c
	}
	for _, c := range b.Counters {
		old, ok := oldCounters[c.Name]
		if !ok {
			continue
		}
		if old.PK != c.PK || old.SK != c.SK || old.Shards != c.Shards {
			out = append(out, fmt.Sprintf("counter %s cannot change its keys or shards in place (older items' contributions are on the old counter items); add a new counter and remove the old one", c.Name))
			continue
		}
		oldValues := map[string]CounterAttr{}
		for _, v := range old.Values {
			oldValues[v.Name] = v
		}
		for _, v := range c.Values {
			if ov, ok := oldValues[v.Name]; ok && mustJSON(ov) != mustJSON(v) {
				out = append(out, fmt.Sprintf("counter value %s.%s cannot change what it counts in place (older items contributed under the old definition); add a value with a new name", c.Name, v.Name))
			}
		}
	}
	return out
}

func derivedKeys(s Shape) map[string]bool {
	out := map[string]bool{}
	for _, ix := range s.Indexes {
		out[indexKey(ix)] = true
	}
	for _, u := range s.Uniques {
		out[uniqueKey(u)] = true
	}
	for _, c := range s.Counters {
		out[counterKey(c)] = true
		for _, v := range c.Values {
			out[counterValueKey(c, v)] = true
		}
	}
	return out
}

func indexKey(x IndexShape) string   { return "index:" + mustJSON(x) }
func uniqueKey(x UniqueShape) string { return "unique:" + mustJSON(x) }
func counterKey(x CounterShape) string {
	return "counter:" + mustJSON([]any{x.Name, x.PK, x.SK, x.Shards})
}

func counterValueKey(c CounterShape, v CounterAttr) string {
	return counterKey(c) + ":" + mustJSON(v)
}

// ShapeOf extracts the storage shape of an entity.
func ShapeOf(e *schema.Entity, ttlAttr string) Shape {
	s := Shape{PK: e.PK.Raw, SK: e.SK.Raw}
	for _, f := range e.Fields {
		s.Fields = append(s.Fields, FieldShape{Name: f.Name, Attr: f.Attr, Type: string(f.Type), Values: f.Enum})
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

// Diff describes the changes from one shape to another, one sentence per change.
func Diff(a, b Shape) []string {
	var out []string
	if a.PK != b.PK || a.SK != b.SK {
		out = append(out, fmt.Sprintf("primary key changed from %s / %s to %s / %s (existing items stay under the old key: needs a key rewrite migration)", a.PK, a.SK, b.PK, b.SK))
	}
	if a.TTL != b.TTL {
		out = append(out, fmt.Sprintf("ttl field changed from %q to %q", a.TTL, b.TTL))
	}
	if a.TTLAttr != "" && b.TTLAttr != "" && a.TTLAttr != b.TTLAttr {
		out = append(out, fmt.Sprintf("table TTL attribute changed from %q to %q (existing items keep their expiry in the old attribute, so they never expire and keep their keys taken: needs a rewrite migration)", a.TTLAttr, b.TTLAttr))
	}
	out = append(out, diffNamed("field", toMap(a.Fields, func(f FieldShape) string { return f.Name }), toMap(b.Fields, func(f FieldShape) string { return f.Name }),
		"", "existing items keep the old attribute or type: needs a rewrite migration", nil)...)
	const indexNote = "existing items get their entry when next written; a backfill adds the rest"
	out = append(out, diffNamed("index", toMap(a.Indexes, func(x IndexShape) string { return x.Name }), toMap(b.Indexes, func(x IndexShape) string { return x.Name }),
		indexNote, indexNote, gsiRebuild(a.Indexes, b.Indexes))...)
	const claimNote = "existing items are not claimed until next written: needs a backfill before uniqueness holds"
	out = append(out, diffNamed("unique claim", toMap(a.Uniques, func(x UniqueShape) string { return x.Name }), toMap(b.Uniques, func(x UniqueShape) string { return x.Name }),
		claimNote, claimNote, nil)...)
	const counterNote = "existing items are not counted until next written: needs a backfill before the counter is complete"
	out = append(out, diffNamed("counter", toMap(a.Counters, func(x CounterShape) string { return x.Name }), toMap(b.Counters, func(x CounterShape) string { return x.Name }),
		counterNote, "new values count only items written from this version: needs a backfill before they are complete", nil)...)
	return out
}

func removedNote(kind string) string {
	switch kind {
	case "unique claim":
		return "claims made by existing items stay behind and keep their values taken: needs a cleanup migration"
	case "index":
		return "existing items keep their entries or copies until rewritten; a cleanup migration removes them"
	}
	return ""
}

func migrationNeeded(change string) bool {
	return strings.Contains(change, "needs a")
}

func toMap[T any](xs []T, key func(T) string) map[string]string {
	m := map[string]string{}
	for _, x := range xs {
		m[key(x)] = mustJSON(x)
	}
	return m
}

// upgrade fills in what a lock written by an older dynago didn't record, taking it from the
// current shape: an absence there is not a change.
func upgrade(old, now Shape) Shape {
	if old.TTL != "" && old.TTLAttr == "" {
		old.TTLAttr = now.TTLAttr
	}
	return old
}

// gsiRebuild returns, for each GSI whose keys and where are unchanged but whose projection
// changed, what that change actually means. It returns nil for none.
func gsiRebuild(a, b []IndexShape) map[string]string {
	out := map[string]string{}
	before := map[string]IndexShape{}
	for _, x := range a {
		before[x.Name] = x
	}
	for _, y := range b {
		x, ok := before[y.Name]
		if !ok || x.Strategy != "gsi" || y.Strategy != "gsi" || x.PK != y.PK || x.SK != y.SK || mustJSON(x.Where) != mustJSON(y.Where) {
			continue
		}
		if x.Projection != y.Projection || mustJSON(x.Project) != mustJSON(y.Project) {
			out[y.Name] = "DynamoDB cannot change a GSI's projection: the index is deleted and re-created (Terraform replaces it), and queries through it fail until DynamoDB has rebuilt it from the table. No data backfill"
		}
	}
	return out
}

func diffNamed(kind string, a, b map[string]string, addedNote, changedNote string, changedNotes map[string]string) []string {
	var out []string
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	note := func(n string) string {
		if n == "" {
			return ""
		}
		return " (" + n + ")"
	}
	for _, n := range sorted {
		av, inA := a[n]
		bv, inB := b[n]
		switch {
		case !inA:
			out = append(out, fmt.Sprintf("%s %s added%s", kind, n, note(addedNote)))
		case !inB:
			out = append(out, fmt.Sprintf("%s %s removed%s", kind, n, note(removedNote(kind))))
		case av != bv && changedNotes[n] != "":
			out = append(out, fmt.Sprintf("%s %s changed%s", kind, n, note(changedNotes[n])))
		case av != bv:
			out = append(out, fmt.Sprintf("%s %s changed%s", kind, n, note(changedNote)))
		}
	}
	return out
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
