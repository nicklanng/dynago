package schema

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// relations links entities: each entity's parent (the entity whose partition its own nests in),
// fields declaring `ref`, and requires. Links are by field name: a Loan's libraryId and toolId
// hold its Tool's key because the Tool's key fields have those names.
func (r *resolver) relations(m *Model) {
	for _, e := range m.Entities {
		if p := parentOf(m, e); p != nil {
			rel := r.relate(m, e, p)
			rel.Kinds = appendKind(rel.Kinds, RelNests)
			e.Parent = rel
		}
	}
	for _, e := range m.Entities {
		for _, f := range e.Fields {
			if f.Ref == nil {
				continue
			}
			key, missing := mapKey(e, f.Ref, f)
			if missing != "" {
				r.errorf("entity %s field %s ref: %s's key needs %s, which %s doesn't have (fields link by name; ref supplies one key field)", e.Name, f.Name, f.Ref.Name, missing, e.Name)
				continue
			}
			rel := r.link(m, e, f.Ref, key)
			rel.Kinds = appendKind(rel.Kinds, RelRef)
		}
		for _, w := range e.Writes {
			for _, rq := range w.Requires {
				if rq.Target == nil || rq.Target == e {
					continue
				}
				rel := r.link(m, e, rq.Target, rq.Key)
				rel.Kinds = appendKind(rel.Kinds, RelRequires)
				if !slices.Contains(rel.Writes, w) {
					rel.Writes = append(rel.Writes, w)
				}
			}
		}
	}
}

// relate returns the relation from e to target through fields of the same names, creating it if
// needed; nil if e doesn't hold target's key that way.
func (r *resolver) relate(m *Model, e, target *Entity) *Relation {
	key, missing := mapKey(e, target, nil)
	if missing != "" {
		return nil
	}
	return r.link(m, e, target, key)
}

// link returns the relation with this key mapping, creating it if needed.
func (r *resolver) link(m *Model, from, to *Entity, key []RequireKey) *Relation {
	for _, rel := range m.Relations {
		if rel.From == from && rel.To == to && sameKey(rel.Key, key) {
			return rel
		}
	}
	rel := &Relation{From: from, To: to, Key: key}
	m.Relations = append(m.Relations, rel)
	return rel
}

func sameKey(a, b []RequireKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func appendKind(ks []RelationKind, k RelationKind) []RelationKind {
	if slices.Contains(ks, k) {
		return ks
	}
	return append(ks, k)
}

// mapKey maps each key field of target to the field of e holding its value. Without via, fields
// match by name and type. A ref field via supplies one key field: the one no field of e matches by
// name, or else target's last (a stewardId next to memberId refers to another Member); the rest
// match by name. It returns the first key field it can't map, if any.
func mapKey(e, target *Entity, via *Field) ([]RequireKey, string) {
	tks := target.KeyFields()
	var byRef *Field // the key field via supplies
	if via != nil {
		var candidates []*Field
		for _, tf := range tks {
			if refersTo(via, tf) {
				candidates = append(candidates, tf)
			}
		}
		for _, tf := range candidates {
			if tf.Name == via.Name {
				byRef = tf
			}
		}
		if byRef == nil {
			for _, tf := range candidates {
				if src := e.Field(tf.Name); src == nil || !sameType(src, tf) {
					byRef = tf
					break
				}
			}
		}
		if byRef == nil && len(candidates) > 0 {
			byRef = candidates[len(candidates)-1]
		}
		if byRef == nil {
			return nil, "a key field for " + via.Name
		}
	}
	var key []RequireKey
	for _, tf := range tks {
		src := e.Field(tf.Name)
		switch {
		case tf == byRef:
			src = via
		case src == nil || !sameType(src, tf) || src == via:
			return nil, tf.Name
		}
		key = append(key, RequireKey{Target: tf, Source: src})
	}
	return key, ""
}

// keyChoices returns each way e holds target's key: through fields of the same names, and through
// each ref field to target. A by-name match that is e's own key (Post.id against User.id) is a
// coincidence when e has a ref to target, and isn't offered.
func keyChoices(e, target *Entity) [][]RequireKey {
	var out [][]RequireKey
	add := func(key []RequireKey) {
		for _, k := range out {
			if sameKey(k, key) {
				return
			}
		}
		out = append(out, key)
	}
	var refs []*Field
	for _, f := range e.Fields {
		if f.Ref == target {
			refs = append(refs, f)
		}
	}
	if key, missing := mapKey(e, target, nil); missing == "" {
		if rel := (&Relation{From: e, To: target, Key: key}); len(refs) == 0 || !rel.OneToOne() || e == target {
			add(key)
		}
	}
	for _, f := range refs {
		if key, missing := mapKey(e, target, f); missing == "" {
			add(key)
		}
	}
	return out
}

// keyVia picks how e holds target's key: the only way, or the one through the field via. It
// returns a message saying why it can't pick, if it can't.
func keyVia(e, target *Entity, via string) ([]RequireKey, string) {
	choices := keyChoices(e, target)
	if via != "" {
		if e.Field(via) == nil {
			return nil, fmt.Sprintf("via: %s has no field %s", e.Name, via)
		}
		var picked [][]RequireKey
		for _, key := range choices {
			if slices.Contains(distinct(key, choices), via) {
				picked = append(picked, key)
			}
		}
		if len(picked) == 0 && len(choices) == 1 && slices.ContainsFunc(choices[0], func(k RequireKey) bool { return k.Source.Name == via }) {
			// The only way there is, and via names a field of it: nothing to pick, but not wrong.
			picked = choices
		}
		if len(picked) != 1 {
			return nil, fmt.Sprintf("via: %s doesn't pick one way %s holds %s's key (%s)", via, e.Name, target.Name, ways(choices))
		}
		return picked[0], ""
	}
	switch len(choices) {
	case 0:
		_, missing := mapKey(e, target, nil)
		return nil, fmt.Sprintf("%s doesn't hold %s's key field %s (fields link by name, or through a ref field)", e.Name, target.Name, missing)
	case 1:
		return choices[0], ""
	}
	return nil, fmt.Sprintf("%s holds %s's key more than one way (%s); say which with via", e.Name, target.Name, ways(choices))
}

// sameSources reports whether two ways of holding a key use the same fields.
func sameSources(a, b []RequireKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Source != b[i].Source || a[i].Target != b[i].Target {
			return false
		}
	}
	return true
}

// distinct returns the names of key's source fields that not every choice uses.
func distinct(key []RequireKey, choices [][]RequireKey) []string {
	var out []string
	for _, k := range key {
		for _, other := range choices {
			if !slices.ContainsFunc(other, func(o RequireKey) bool { return o.Source == k.Source }) {
				out = append(out, k.Source.Name)
				break
			}
		}
	}
	return out
}

// ways describes the choices by the fields that tell them apart: "via: memberId, or via: stewardId".
func ways(choices [][]RequireKey) string {
	var out []string
	for _, key := range choices {
		if d := distinct(key, choices); len(d) > 0 {
			out = append(out, "via: "+strings.Join(d, "+"))
		}
	}
	if len(out) == 0 {
		return "no field tells them apart"
	}
	return strings.Join(out, ", or ")
}

// HoldsKey returns e's fields holding target's key, linked as relations are (by name, or through a
// ref field), or false if e doesn't hold it, or holds it more than one way.
func HoldsKey(e, target *Entity) ([]*Field, bool) {
	key, why := keyVia(e, target, "")
	if why != "" {
		return nil, false
	}
	out := make([]*Field, len(key))
	for i, k := range key {
		out[i] = k.Source
	}
	return out, true
}

// refersTo reports whether a ref field can hold values of the key field tf: a field of its type,
// or a string_set whose elements are each one (a thread's labelIds, each a Label's labelId).
func refersTo(via, tf *Field) bool {
	return sameType(via, tf) || (via.Type == TypeStringSet && tf.Type == TypeString)
}

// Many reports whether each From item can refer to several To items: it holds their keys as the
// elements of a set.
func (r *Relation) Many() bool { return r.Set() != nil }

// Set returns the string_set field whose elements each hold a To item's key field, or nil.
func (r *Relation) Set() *Field {
	for _, k := range r.Key {
		if k.Source.Type == TypeStringSet && k.Target.Type != TypeStringSet {
			return k.Source
		}
	}
	return nil
}

func sameType(a, b *Field) bool {
	return a.Type == b.Type && (a.Type != TypeEnum || strings.Join(a.Enum, ",") == strings.Join(b.Enum, ","))
}

// parentOf finds the entity whose partition e's partition nests in: its partition key is a prefix
// of e's (literal text and placeholders alike) and e holds its key. The longest such key wins;
// an entity keyed exactly like e is a parent only if declared first (a tool's hold, keyed like
// the tool, belongs to the tool).
func parentOf(m *Model, e *Entity) *Entity {
	var best *Entity
	bestSegs, bestKeys := -1, -1
	pos := slices.Index(m.Entities, e)
	eKey := names(e.KeyFields())
	for i, p := range m.Entities {
		if p == e || !nests(p.PK, e.PK) {
			continue
		}
		pKey := names(p.KeyFields())
		if !subset(pKey, fieldNames(e)) {
			continue
		}
		if equalSets(pKey, eKey) && i > pos {
			continue
		}
		if subset(eKey, pKey) && !equalSets(pKey, eKey) {
			continue // p is finer-grained than e
		}
		if _, missing := mapKey(e, p, nil); missing != "" {
			continue
		}
		segs, keys := len(p.PK.Segments), len(pKey)
		if segs > bestSegs || (segs == bestSegs && keys > bestKeys) {
			best, bestSegs, bestKeys = p, segs, keys
		}
	}
	return best
}

// nests reports whether partition key template p is a prefix of e: the same segments, with p's
// last literal a prefix of e's literal in that place.
func nests(p, e Template) bool {
	ps, es := p.Segments, e.Segments
	if len(ps) > len(es) {
		return false
	}
	for i, s := range ps {
		t := es[i]
		if s.IsField() != t.IsField() {
			return false
		}
		switch {
		case s.IsField():
			if s.Field != t.Field || s.Transform != t.Transform {
				return false
			}
		case i == len(ps)-1:
			// The literal must end where a separator ends it: "LIB#" nests "LIB#TOOL#", but "S"
			// doesn't nest "SESSION#".
			if !strings.HasPrefix(t.Literal, s.Literal) {
				return false
			}
			if len(t.Literal) > len(s.Literal) && !separator(s.Literal[len(s.Literal)-1]) && !separator(t.Literal[len(s.Literal)]) {
				return false
			}
		case s.Literal != t.Literal:
			return false
		}
	}
	return true
}

func separator(c byte) bool {
	return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9')
}

func names(fs []*Field) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

func fieldNames(e *Entity) []string { return names(e.Fields) }

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

func equalSets(a, b []string) bool { return subset(a, b) && subset(b, a) }

// volumes resolves each entity's volume and the expected item counts.
func (r *resolver) volumes(m *Model) {
	for _, e := range m.Entities {
		raw := e.rawVolume
		where := "entity " + e.Name + " volume"
		if !raw.Set {
			continue
		}
		v := Volume{Declared: true}
		if raw.Total != nil {
			if *raw.Total < 0 {
				r.errorf("%s: must be zero or more", where)
			}
			v.Total = *raw.Total
			e.Volume = v
			continue
		}
		if raw.Typical == nil {
			r.errorf("%s: give typical (items per parent), or a total number of items (volume: N)", where)
			continue
		}
		v.Typical = *raw.Typical
		if raw.Max != nil {
			v.Max = *raw.Max
		}
		if v.Typical < 0 || v.Max < 0 || (raw.Max != nil && v.Max < v.Typical) {
			r.errorf("%s: need 0 <= typical <= max", where)
		}
		switch {
		case raw.Per != "":
			p := m.Entity(raw.Per)
			if p == nil {
				r.errorf("%s: per: %s is not an entity of this table", where, raw.Per)
				continue
			}
			key, why := keyVia(e, p, raw.Via)
			if why != "" {
				r.errorf("%s: per %s: %s", where, p.Name, why)
				continue
			}
			if rel := (&Relation{From: e, To: p, Key: key}); rel.Many() {
				r.errorf("%s: per %s: a %s holds several %s keys, in %s, so its count isn't per %s. Count it per its parent, and give how many each %s has with by: { %s: { typical: ..., max: ... } }",
					where, p.Name, e.Name, p.Name, rel.Set().Name, p.Name, p.Name, p.Name)
				continue
			}
			v.Per = r.link(m, e, p, key)
			v.Per.Kinds = appendKind(v.Per.Kinds, RelVolume)
			e.Parent = v.Per
		case raw.Via != "":
			r.errorf("%s: via picks how %s holds the key of the entity named by per; name it", where, e.Name)
			continue
		case e.Parent == nil:
			r.errorf("%s: %s's partition doesn't nest in another entity's, so typical and max have nothing to count per. Name the parent (per: <Entity>), or give a total (volume: N)", where, e.Name)
			continue
		default:
			v.Per = e.Parent
		}
		if v.Per.OneToOne() {
			switch {
			case v.Max > 1 || v.Typical > 1:
				r.errorf("%s: a %s has the key of its %s, so each %s has at most one; typical and max can't exceed 1", where, e.Name, v.Per.To.Name, v.Per.To.Name)
			case v.Max == 0:
				v.Max = 1
			}
		}
		for _, b := range raw.By {
			bw := where + " by " + b.Key
			to := m.Entity(b.Key)
			if to == nil {
				r.errorf("%s: %s is not an entity of this table", bw, b.Key)
				continue
			}
			key, why := keyVia(e, to, b.Value.Via)
			if why != "" {
				r.errorf("%s: %s", bw, why)
				continue
			}
			if to == v.Per.To && sameSources(key, v.Per.Key) {
				r.errorf("%s: the volume already counts per %s: its typical and max say how many each %s has", bw, to.Name, to.Name)
				continue
			}
			if b.Value.Typical == nil {
				r.errorf("%s: typical is required", bw)
				continue
			}
			vb := &VolumeBy{Typical: *b.Value.Typical}
			if b.Value.Max != nil {
				vb.Max = *b.Value.Max
			}
			if vb.Typical < 0 || vb.Max < 0 || (b.Value.Max != nil && vb.Max < vb.Typical) {
				r.errorf("%s: need 0 <= typical <= max", bw)
			}
			vb.Relation = r.link(m, e, to, key)
			vb.Relation.Kinds = appendKind(vb.Relation.Kinds, RelVolume)
			v.By = append(v.By, vb)
		}
		e.Volume = v
	}
	// Counts follow the volumes down from the entities given as totals.
	visiting := map[*Entity]bool{}
	done := map[*Entity]bool{}
	var count func(e *Entity) float64
	count = func(e *Entity) float64 {
		if done[e] {
			return e.Count
		}
		if visiting[e] {
			r.errorf("entity %s volume: the volumes count per each other in a circle; give one of them a total", e.Name)
			return 0
		}
		visiting[e] = true
		switch v := e.Volume; {
		case !v.Declared:
		case v.Per == nil:
			e.Count = v.Total
		default:
			e.Count = count(v.Per.To) * v.Typical
		}
		done[e] = true
		return e.Count
	}
	for _, e := range m.Entities {
		count(e)
	}
}

// String describes the volume for people: "2,000", "20 per Library (at most 40,000)".
func (v Volume) String() string {
	switch {
	case !v.Declared:
		return "not declared"
	case v.Per == nil:
		return Number(v.Total)
	}
	s := fmt.Sprintf("%s per %s", Number(v.Typical), v.Per.To.Name)
	if v.Max > 0 {
		s += fmt.Sprintf(" (at most %s)", Number(v.Max))
	}
	return s
}

// Number formats a count for people: 2,000,000; 1,234.5; 0.02; 0.004.
func Number(f float64) string {
	if f < 0 {
		return "-" + Number(-f)
	}
	digits := 2
	if _, frac := math.Modf(f); frac != 0 {
		for d := 0.01; frac < d && digits < 6; d /= 10 {
			digits++ // keep small values visible: 0.004, not 0
		}
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(f, 'f', digits, 64), ".")
	if frac = strings.TrimRight(frac, "0"); frac != "" {
		frac = "." + frac
	}
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String() + frac
}
