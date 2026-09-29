package gocode

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nicklanng/dynago/internal/schema"
)

// This file generates the migration job's code: reading the previous table generation, converting
// each entity, and writing it with its derived items into the current one.

// migration emits RunMigration, and for a generation above 1 everything it needs.
func (g *gen) migration() {
	m := g.m
	t := m.Table
	g.p("// ---- migration ----")
	g.p("")
	g.p("// RunMigration runs the migration job's command line (see dynago.MigrateUsage): it copies the")
	g.p("// previous table generation into this one. A generated main (output.migrate_cmd) calls it.")
	g.p("func RunMigration(ctx context.Context, db *dynamo.DB, args []string, out io.Writer) error {")
	g.p("command, base, workers, passes, rate, err := dynago.ParseMigrate(args, %q)", t.Name)
	g.p("if err != nil {")
	g.p("return err")
	g.p("}")
	if m.Previous == nil {
		g.p("_, _, _, _ = base, workers, passes, rate")
		g.p("if command != \"copy\" && command != \"finish\" && command != \"status\" {")
		g.p("return fmt.Errorf(\"unknown command %%q\\n\\n%%s\", command, dynago.MigrateUsage)")
		g.p("}")
		g.p("_, err = fmt.Fprintln(out, \"generation 1 has no previous generation to migrate from\")")
		g.p("return err")
		g.p("}")
		g.p("")
		return
	}
	g.p("mig := NewMigration(db, base)")
	g.p("mig.Workers, mig.MaxPasses, mig.Rate, mig.Out = workers, passes, rate, out")
	g.p("return mig.Run(ctx, command)")
	g.p("}")
	g.p("")
	prev := m.Previous
	var types []string
	for _, e := range m.Entities {
		if _, ok := prev.Entities[e.Name]; ok {
			types = append(types, fmt.Sprintf("%q: true", e.Name))
		}
	}
	g.p("// NewMigration returns the job copying generation %d of the table into generation %d: base-g%d", prev.Generation, t.Generation, prev.Generation)
	g.p("// into base-g%d. Entities are converted by the Migrate<Entity> functions, where set, and copied", t.Generation)
	g.p("// field by field otherwise.")
	g.p("func NewMigration(db *dynamo.DB, base string) *dynago.Migration {")
	g.p("st := New(db, TableName(base))")
	g.p("return &dynago.Migration{DB: db, From: base + \"-g%d\", To: TableName(base),", prev.Generation)
	g.p("Types: map[string]bool{%s},", strings.Join(types, ", "))
	g.p("Copy: st.migrateCopy, KeyOf: st.migrateKeyOf, Remove: st.migrateRemove, Check: migrationCheck, TTLAttr: %q}", prev.TTLAttr)
	g.p("}")
	g.p("")
	for _, e := range m.Entities {
		if fields, ok := prev.Entities[e.Name]; ok {
			g.previousStruct(e, fields)
		}
	}
	g.migrationCheck()
	g.migrationDispatch()
	for _, e := range m.Entities {
		g.migrateEntity(e)
	}
}

// previousType is the Go type decoding a field of the previous generation: enums decode as plain
// strings, since the current code may no longer have the enum type or all of its values.
func previousType(f schema.PreviousField) string {
	switch f.Type {
	case schema.TypeEnum, schema.TypeString:
		return "string"
	case schema.TypeInt:
		return "int64"
	case schema.TypeFloat:
		return "float64"
	case schema.TypeBool:
		return "bool"
	case schema.TypeTime:
		return "time.Time"
	case schema.TypeBytes:
		return "[]byte"
	case schema.TypeStringSet, schema.TypeList:
		return "[]string"
	case schema.TypeMap:
		return "map[string]string"
	}
	return "string"
}

// conversionGaps returns why an entity can't be copied field by field from the previous
// generation, or nil. A field that disappeared might have been renamed, so it needs a conversion
// that says what happens to it.
func conversionGaps(e *schema.Entity, prev []schema.PreviousField) []string {
	var gaps []string
	old := map[string]schema.PreviousField{}
	for _, f := range prev {
		old[f.Name] = f
	}
	for _, f := range e.Fields {
		pf, had := old[f.Name]
		delete(old, f.Name)
		switch {
		case !had && f.Required:
			gaps = append(gaps, fmt.Sprintf("%s is new and required", f.Name))
		case !had:
		case f.Required && !pf.Required:
			gaps = append(gaps, fmt.Sprintf("%s is now required", f.Name))
		case previousType(pf) != previousType(schema.PreviousField{Type: f.Type}) || (pf.Type == schema.TypeEnum) != (f.Type == schema.TypeEnum):
			gaps = append(gaps, fmt.Sprintf("%s changed from %s to %s", f.Name, pf.Type, f.Type))
		case f.Type == schema.TypeEnum:
			for _, v := range pf.Values {
				if !slices.Contains(f.Enum, v) {
					gaps = append(gaps, fmt.Sprintf("%s no longer has the value %q", f.Name, v))
				}
			}
		}
	}
	var gone []string
	for name := range old {
		gone = append(gone, name)
	}
	slices.Sort(gone)
	for _, name := range gone {
		gaps = append(gaps, fmt.Sprintf("%s is gone (removed or renamed?)", name))
	}
	return gaps
}

func (g *gen) previousStruct(e *schema.Entity, fields []schema.PreviousField) {
	prev := g.m.Previous
	old := fmt.Sprintf("%sG%d", e.GoName, prev.Generation)
	g.p("// %s is %s %s as generation %d stored it: what the migration job reads.", old, article(e.Name), e.Name, prev.Generation)
	g.p("type %s struct {", old)
	for _, f := range fields {
		g.p("%s %s `dynamo:\"%s\"`", schema.GoName(f.Name), previousType(f), f.Attr)
	}
	g.p("}")
	g.p("")
	gaps := conversionGaps(e, fields)
	fn := "Migrate" + e.GoName
	if len(gaps) > 0 {
		comment(&g.buf, fmt.Sprintf("%s converts a generation-%d %s into this generation's. The migration job can't run without it, because %s. Set it in a file of your own in this package, for example from an init function; AutoMigrate%s copies the fields that carry over.",
			fn, prev.Generation, e.Name, strings.Join(gaps, "; "), e.GoName))
	} else {
		comment(&g.buf, fmt.Sprintf("%s, if set, converts a generation-%d %s into this generation's. Without it, the migration job uses AutoMigrate%s: every field carries over. Set it in a file of your own in this package to fill new fields.",
			fn, prev.Generation, e.Name, e.GoName))
	}
	g.p("var %s func(old %s) (%s, error)", fn, old, e.GoName)
	g.p("")
	g.p("// AutoMigrate%s copies the fields of a generation-%d %s that exist in this generation with the", e.GoName, prev.Generation, e.Name)
	g.p("// same type (enums by value, lists made sets without repeats); the others are left zero.")
	g.p("func AutoMigrate%s(old %s) %s {", e.GoName, old, e.GoName)
	have := map[string]schema.PreviousField{}
	for _, f := range fields {
		have[f.Name] = f
	}
	var kv []string
	for _, f := range e.Fields {
		pf, ok := have[f.Name]
		if !ok || previousType(pf) != previousType(schema.PreviousField{Type: f.Type}) {
			continue
		}
		val := "old." + schema.GoName(pf.Name)
		switch {
		case f.Type == schema.TypeEnum:
			val = goType(e, f) + "(" + val + ")"
		case pf.Type == schema.TypeList && f.Type == schema.TypeStringSet:
			val = "dynago.Distinct(" + val + ")"
		}
		kv = append(kv, fmt.Sprintf("%s: %s", f.GoName, val))
	}
	g.p("return %s{%s}", e.GoName, strings.Join(kv, ", "))
	g.p("}")
	g.p("")
}

func (g *gen) migrationCheck() {
	prev := g.m.Previous
	g.p("// migrationCheck reports the conversions the migration job needs but hasn't been given.")
	g.p("func migrationCheck() error {")
	needed := false
	for _, e := range g.m.Entities {
		if fields, ok := prev.Entities[e.Name]; ok && len(conversionGaps(e, fields)) > 0 {
			needed = true
		}
	}
	if !needed {
		g.p("return nil // every entity carries over field by field")
		g.p("}")
		g.p("")
		return
	}
	g.p("var missing string")
	for _, e := range g.m.Entities {
		fields, ok := prev.Entities[e.Name]
		if !ok {
			continue
		}
		if gaps := conversionGaps(e, fields); len(gaps) > 0 {
			g.p("if Migrate%s == nil {", e.GoName)
			g.p("missing += %s", strconvQuote("\n  Migrate"+e.GoName+": "+strings.Join(gaps, "; ")))
			g.p("}")
		}
	}
	g.p("if missing != \"\" {")
	g.p("return fmt.Errorf(\"dynago migrate: set these conversions in package %s before migrating:%%s\", missing)", g.m.Package)
	g.p("}")
	g.p("return nil")
	g.p("}")
	g.p("")
}

func (g *gen) migrationDispatch() {
	prev := g.m.Previous
	ttl := prev.TTLAttr // items being copied expire by the old table's TTL
	var carried []*schema.Entity
	for _, e := range g.m.Entities {
		if _, ok := prev.Entities[e.Name]; ok {
			carried = append(carried, e)
		}
	}
	for _, e := range carried {
		g.p("// convert%s converts a %s stored by generation %d into this generation's.", e.GoName, e.Name, prev.Generation)
		g.p("func convert%s(raw dynamo.Item) (%s, error) {", e.GoName, e.GoName)
		g.p("var old %sG%d", e.GoName, prev.Generation)
		g.p("if err := dynamo.UnmarshalItem(raw, &old); err != nil {")
		g.p("return %s{}, err", e.GoName)
		g.p("}")
		g.p("if Migrate%s == nil {", e.GoName)
		g.p("return AutoMigrate%s(old), nil", e.GoName)
		g.p("}")
		g.p("e, err := Migrate%s(old)", e.GoName)
		g.p("if err != nil {")
		g.p("return %s{}, fmt.Errorf(\"%%w: %%w\", dynago.ErrMigrationConflict, err)", e.GoName)
		g.p("}")
		g.p("return e, nil")
		g.p("}")
		g.p("")
	}
	g.p("// migrateCopy converts one item of the previous generation and writes it into this one.")
	g.p("func (s *Store) migrateCopy(ctx context.Context, raw dynamo.Item, fence dynago.Op) error {")
	g.p("if dynago.ExpiredItem(raw, %q) {", ttl)
	g.p("return dynago.ErrUnchanged // expired: gone, even if DynamoDB hasn't deleted it yet")
	g.p("}")
	g.dispatch(carried, func(e *schema.Entity) {
		g.p("e, err := convert%s(raw)", e.GoName)
		g.p("if err != nil {")
		g.p("return err")
		g.p("}")
		g.p("src, srcRev, err := dynago.SourceOf(raw)")
		g.p("if err != nil {")
		g.p("return err")
		g.p("}")
		g.p("return s.%s.migrate(ctx, &e, src, srcRev, fence)", schema.Plural(e.GoName))
	})
	g.p("return dynago.ErrUnchanged")
	g.p("}")
	g.p("")
	g.p("// migrateKeyOf returns the key an item of the previous generation converts to, or false if it")
	g.p("// isn't copied.")
	g.p("func (s *Store) migrateKeyOf(raw dynamo.Item) (dynago.Key, bool, error) {")
	g.p("if dynago.ExpiredItem(raw, %q) {", ttl)
	g.p("return dynago.Key{}, false, nil")
	g.p("}")
	g.dispatch(carried, func(e *schema.Entity) {
		g.p("e, err := convert%s(raw)", e.GoName)
		g.p("if err != nil {")
		g.p("return dynago.Key{}, false, err")
		g.p("}")
		g.p("key, err := e.Key().dynamoKey()")
		g.p("return key, err == nil, err")
	})
	g.p("return dynago.Key{}, false, nil")
	g.p("}")
	g.p("")
	g.p("// migrateRemove deletes an entity the migration copied, because its source is gone or now")
	g.p("// converts to another key.")
	g.p("func (s *Store) migrateRemove(ctx context.Context, raw dynamo.Item, fence dynago.Op) error {")
	g.dispatch(g.m.Entities, func(e *schema.Entity) {
		g.p("return s.%s.migrateRemove(ctx, raw, fence)", schema.Plural(e.GoName))
	})
	g.p("return nil")
	g.p("}")
	g.p("")
}

// dispatch emits a branch per entity on the stored item's type: a switch, or an if for one.
func (g *gen) dispatch(es []*schema.Entity, body func(e *schema.Entity)) {
	switch len(es) {
	case 0:
	case 1:
		g.p("if dynago.ItemType(raw) == %q {", es[0].Name)
		body(es[0])
		g.p("}")
	default:
		g.p("switch dynago.ItemType(raw) {")
		for _, e := range es {
			g.p("case %q:", e.Name)
			body(e)
		}
		g.p("}")
	}
}

// migrateEntity emits the writes the migration job makes for one entity: a copy that replaces
// whatever the new table holds, and a removal, both maintaining derived items and carrying the
// job's fence (a check that it still holds its lease).
func (g *gen) migrateEntity(e *schema.Entity) {
	lo := lowerFirst(e.GoName)
	limits := ""
	if hasLimitArgs(e) {
		limits = fmt.Sprintf(", %sLimits{}", lo)
	}
	// Copying counts what the old table holds, whose writes already enforced its rules: limits
	// and lower bounds are left out (dynago.Unbounded), so copying in any order can't trip them.
	derived := func(recv string) string {
		return fmt.Sprintf("dynago.Unbounded(%sDerived(%s, key%s))", lo, recv, limits)
	}
	g.p("// migrate writes e, converted from the item at src in the previous generation (revision")
	g.p("// srcRev), replacing what the new table holds for it, with its derived items.")
	g.p("func (s *%sStore) migrate(ctx context.Context, e *%s, src dynago.Key, srcRev int64, fence dynago.Op) error {", e.GoName, e.GoName)
	g.p("key, err := e.Key().dynamoKey()")
	g.p("if err != nil {")
	g.p("return err")
	g.p("}")
	g.requiredChecks(e, e.Fields, "e", "return err")
	g.p("if err := e.checkKeyParts(); err != nil {")
	g.p("return err")
	g.p("}")
	g.p("return dynago.Retry(ctx, func() error {")
	g.p("var raw dynamo.Item")
	g.p("found, err := dynago.GetOne(ctx, s.t, key, true, &raw)")
	g.p("if err != nil {")
	g.p("return err")
	g.p("}")
	g.p("rev := dynago.NewRev()")
	g.p("var before []dynago.Derived")
	g.p("if found {")
	g.p("same, other := dynago.MigratedFrom(raw, src, srcRev)")
	g.p("switch {")
	g.p("case other:")
	g.p("return fmt.Errorf(\"%%w: another item of the previous generation converts to the same %s key\", dynago.ErrMigrationConflict)", e.Name)
	g.p("case same:")
	g.p("return dynago.ErrUnchanged")
	g.p("}")
	g.p("it, err := %sDecode(raw)", lo)
	g.p("if err != nil {")
	g.p("return err")
	g.p("}")
	if e.HasDerived() {
		g.p("before = %s", derived("&it."+e.GoName))
	}
	g.p("rev = it.Rev + 1")
	g.p("}")
	g.p("item, err := dynago.MigrationItem(%sToItem(e, key, rev), src, srcRev)", lo)
	g.p("if err != nil {")
	g.p("return err")
	g.p("}")
	g.p("put := s.t.Put(item).If(\"attribute_not_exists($)\", \"PK\")")
	g.p("if found {")
	g.p("put = s.t.Put(item).If(\"$ = ?\", \"_rev\", rev-1)")
	g.p("}")
	g.p("ops := []dynago.Op{dynago.PutOp(key, put, dynago.ErrStale), fence}")
	if e.HasDerived() {
		g.p("derived, err := dynago.Diff(s.t, key, before, %s)", derived("e"))
		g.p("if err != nil {")
		g.p("return err")
		g.p("}")
		g.p("ops = append(ops, derived...)")
	} else {
		g.p("_ = before")
	}
	g.p("return dynago.Run(ctx, s.db, ops)")
	g.p("})")
	g.p("}")
	g.p("")
	g.p("// migrateRemove deletes an entity the migration copied, with its derived items, unless it has")
	g.p("// been rewritten since raw was read: then it is a fresher copy, not the stale one.")
	g.p("func (s *%sStore) migrateRemove(ctx context.Context, raw dynamo.Item, fence dynago.Op) error {", e.GoName)
	g.p("return dynago.Retry(ctx, func() error {")
	g.p("var current dynamo.Item")
	g.p("found, err := dynago.GetOne(ctx, s.t, dynago.Key{PK: dynago.ItemKey(raw).PK, SK: dynago.ItemKey(raw).SK}, true, &current)")
	g.p("if err != nil || !found {")
	g.p("return err")
	g.p("}")
	g.p("it, err := %sDecode(current)", lo)
	g.p("if err != nil {")
	g.p("return err")
	g.p("}")
	g.p("if it.Rev != dynago.ItemRev(raw) {")
	g.p("return nil")
	g.p("}")
	g.p("key := dynago.Key{PK: it.PK, SK: it.SK}")
	g.p("ops := []dynago.Op{dynago.DeleteOp(key, s.t.Delete(\"PK\", key.PK).Range(\"SK\", key.SK).If(\"$ = ?\", \"_rev\", it.Rev), dynago.ErrStale), fence}")
	if e.HasDerived() {
		g.p("derived, err := dynago.Diff(s.t, key, %s, nil)", derived("&it."+e.GoName))
		g.p("if err != nil {")
		g.p("return err")
		g.p("}")
		g.p("ops = append(ops, derived...)")
	}
	g.p("return dynago.Run(ctx, s.db, ops)")
	g.p("})")
	g.p("}")
	g.p("")
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }

// GenerateMigrateCmd returns the migration job's main package, which calls RunMigration of the
// store package at importPath.
func GenerateMigrateCmd(m *schema.Model, importPath, source string) ([]byte, error) {
	g := &gen{m: m}
	g.p("// Code generated by dynago from %s. DO NOT EDIT.", source)
	g.p("")
	comment(&g.buf, fmt.Sprintf("Command migrate-%s copies the previous generation of the %s table into the current one. Run `copy` as a job before rolling out a version with a new table generation, and `finish` once writes to the old generation have stopped (for example from an init container of the new pods). The AWS SDK's usual environment configures it: credentials, AWS_REGION, and AWS_ENDPOINT_URL_DYNAMODB for DynamoDB Local.",
		fileBaseName(m), m.Table.Name))
	g.p("//")
	g.p("//\tmigrate-%s [-table base] [-workers n] [-rate items/s] [-passes n] copy|finish|status", fileBaseName(m))
	g.p("package main")
	g.p("")
	g.p("import (")
	g.p(`"context"`)
	g.p(`"fmt"`)
	g.p(`"os"`)
	g.p(`"os/signal"`)
	g.p(`"syscall"`)
	g.p("")
	g.p(`"github.com/aws/aws-sdk-go-v2/config"`)
	g.p(`"github.com/guregu/dynamo/v2"`)
	g.p("")
	g.p("%s %q", m.Package, importPath)
	g.p(")")
	g.p("")
	g.p("func main() {")
	g.p("if err := run(); err != nil {")
	g.p("fmt.Fprintln(os.Stderr, err)")
	g.p("os.Exit(1)")
	g.p("}")
	g.p("}")
	g.p("")
	g.p("func run() error {")
	g.p("ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)")
	g.p("defer stop()")
	g.p("cfg, err := config.LoadDefaultConfig(ctx)")
	g.p("if err != nil {")
	g.p("return err")
	g.p("}")
	g.p("return %s.RunMigration(ctx, dynamo.New(cfg), os.Args[1:], os.Stdout)", m.Package)
	g.p("}")
	return formatted(g)
}

func fileBaseName(m *schema.Model) string {
	var b strings.Builder
	for _, r := range strings.ToLower(m.Table.Name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}
