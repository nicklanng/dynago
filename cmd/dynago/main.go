// Command dynago generates typed DynamoDB access code, documentation and infrastructure from a
// schema file, analyses the design, compares designs for review, and finds DynamoDB calls that
// bypass the schema.
//
//	dynago generate [-check] [-policy file] schema.dynago.yaml...
//	dynago check [-json] [-policy file] schema.dynago.yaml...
//	dynago diff [-base ref | -from file] schema.dynago.yaml...
//	dynago vet [-tests] [-list] [packages]
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/internal/analysis"
	"github.com/nicklanng/dynago/internal/arch"
	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/gen/docs"
	"github.com/nicklanng/dynago/internal/gen/gocode"
	"github.com/nicklanng/dynago/internal/gen/infra"
	"github.com/nicklanng/dynago/internal/lock"
	"github.com/nicklanng/dynago/internal/schema"
	"github.com/nicklanng/dynago/internal/vet"
)

const usage = `dynago turns a schema file into typed DynamoDB access code, docs and infrastructure, and
analyses the design it describes.

Usage:
  dynago generate [-check] [-prices wru,rru,gb] [-policy file] [-new-history] <schema.dynago.yaml>...
      Write the Go store, the model document, Terraform, the CreateTable JSON and the lock file
      next to each schema. With -check, write nothing and fail if any output is out of date.
      Refuses while the design has open findings the policy fails on (errors, by default).

  dynago check [-json] [-prices wru,rru,gb] [-policy file] [-new-history] <schema.dynago.yaml>...
      Validate each schema and print its analysis: costs, partitions and findings. With -json,
      print the analysed design as JSON instead. Fails on the findings the policy fails on.

  dynago diff [-base ref | -from file] [-prices wru,rru,gb] [-policy file] <schema.dynago.yaml>...
      Print the architectural changes since the schema at a git ref (default HEAD) or in another
      file, as Markdown for a pull request: entities, indexes, reads and writes, guarantees,
      partitions, findings, costs, and whether existing items need migrating.

  dynago vet [-tests] [-list] [packages]
      Find DynamoDB calls outside generated code (default ./...): requests through the AWS SDK,
      guregu/dynamo or dynago's runtime that bypass the schema. Mark a call that must stay with
      //dynago:raw <reason>. With -list, also print the marked calls and their reasons.

Flags may come before or after the arguments.

  -prices        on-demand prices: $ per million WRU, $ per million RRU, $ per GB-month
                 (default 0.625,0.125,0.25, us-east-1)
  -policy        the policy file (default: dynago.policy.yaml in the schema's directory or above,
                 up to the repository root)
  -new-history   accept a schema whose history the lock file doesn't have (entities above
                 version 1, or a table above generation 1), starting the history there. Only right
                 for a new table: otherwise restore the lock file instead.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are the parsed flags.
type options struct {
	checkOnly  bool
	prices     cost.Prices
	policy     string
	newHistory bool
	json       bool
	base, from string
	tests      bool
	list       bool
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	var o options
	pricesFlag := ""
	switch cmd {
	case "generate", "check", "diff":
		fs.StringVar(&pricesFlag, "prices", "", "on-demand prices: wru,rru,gb")
		fs.StringVar(&o.policy, "policy", "", "the policy file")
	}
	switch cmd {
	case "generate":
		fs.BoolVar(&o.checkOnly, "check", false, "fail if generated files are out of date instead of writing them")
		fs.BoolVar(&o.newHistory, "new-history", false, "start a lock history the lock file doesn't have")
	case "check":
		fs.BoolVar(&o.newHistory, "new-history", false, "start a lock history the lock file doesn't have")
		fs.BoolVar(&o.json, "json", false, "print the analysed design as JSON")
	case "diff":
		fs.StringVar(&o.base, "base", "", "the git ref to compare with (default HEAD)")
		fs.StringVar(&o.from, "from", "", "a schema file to compare with, instead of a git ref")
	case "vet":
		fs.BoolVar(&o.tests, "tests", false, "include test files")
		fs.BoolVar(&o.list, "list", false, "also list the calls marked //dynago:raw")
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "dynago: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	// Accept flags anywhere: the flag package stops at the first file name.
	var files []string
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		files, rest = append(files, rest[0]), rest[1:]
	}
	if cmd == "vet" {
		return vetCmd(files, o, stdout, stderr)
	}
	if len(files) == 0 {
		fmt.Fprintf(stderr, "dynago %s: no schema files given\n", cmd)
		return 2
	}
	if o.base != "" && o.from != "" {
		fmt.Fprintln(stderr, "dynago diff: give -base or -from, not both")
		return 2
	}
	o.prices = cost.DefaultPrices
	if pricesFlag != "" {
		p, err := parsePrices(pricesFlag)
		if err != nil {
			fmt.Fprintf(stderr, "dynago: -prices: %v\n", err)
			return 2
		}
		o.prices = p
	}
	status := 0
	for _, path := range files {
		var err error
		switch cmd {
		case "generate":
			err = generate(path, o, stdout, stderr)
		case "check":
			err = check(path, o, stdout)
		case "diff":
			err = diff(path, o, stdout)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			status = 1
		}
	}
	return status
}

func parsePrices(s string) (cost.Prices, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return cost.Prices{}, errors.New("want three comma-separated numbers")
	}
	var v [3]float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f < 0 {
			return cost.Prices{}, fmt.Errorf("%q is not a price", p)
		}
		v[i] = f
	}
	return cost.Prices{WRUPerMillion: v[0], RRUPerMillion: v[1], GBMonth: v[2]}, nil
}

// loadPolicy reads the policy named by -policy, or the nearest dynago.policy.yaml above the
// schema, or returns the default policy.
func loadPolicy(schemaPath, flagPath string) (*analysis.Policy, error) {
	path := flagPath
	if path == "" {
		found, err := analysis.FindPolicy(filepath.Dir(schemaPath))
		if err != nil {
			return nil, err
		}
		path = found
	}
	if path == "" {
		return analysis.DefaultPolicy(), nil
	}
	p, err := analysis.LoadPolicy(path)
	if err != nil {
		return nil, err
	}
	// The model document shows the policy relative to the schema, so it is the same on every checkout.
	dir, err := filepath.Abs(filepath.Dir(schemaPath))
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(dir, abs); err == nil {
		p.Path = filepath.ToSlash(rel)
	}
	return p, nil
}

// output is one generated file.
type output struct {
	path string
	data []byte
}

// build loads a schema, checks it against its lock and analyses it. With render, it also renders
// every output, without writing anything; check doesn't need them (nor, for the migration command,
// a Go module).
func build(path string, o options, render bool) (*schema.Model, *analysis.Result, []lock.Note, []output, error) {
	m, err := schema.Load(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	dir := filepath.Dir(path)
	source := filepath.Base(path)
	prev, err := lock.Read(filepath.Join(dir, m.Output.Lock))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	next, notes, err := lock.Apply(m, prev, lock.Options{NewHistory: o.newHistory})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	pol, err := loadPolicy(path, o.policy)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	result := analysis.Analyze(m, o.prices, pol)
	if !render {
		return m, result, notes, nil, nil
	}

	goSrc, err := gocode.Generate(m, source)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	retained := map[int]dynago.TableSpec{}
	for _, g := range m.Table.Retain {
		retained[g] = *next.Table(g)
	}
	tf, err := infra.Terraform(m, source, retained)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tableJSON, err := infra.CreateTableJSON(m)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	lockData, err := next.Marshal()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var migrateCmd []byte
	if m.Output.MigrateCmd != "" {
		importPath, err := goImportPath(filepath.Dir(filepath.Join(dir, m.Output.Go)))
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("%s: output.migrate_cmd: %w", path, err)
		}
		if migrateCmd, err = gocode.GenerateMigrateCmd(m, importPath, source); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	outs := []output{
		{filepath.Join(dir, m.Output.Go), goSrc},
		{filepath.Join(dir, m.Output.Docs), docs.Generate(m, result, source)},
		{filepath.Join(dir, m.Output.Terraform), tf},
		{filepath.Join(dir, m.Output.TableJSON), tableJSON},
		{filepath.Join(dir, m.Output.Lock), lockData},
	}
	if migrateCmd != nil {
		outs = append(outs, output{filepath.Join(dir, m.Output.MigrateCmd, "main.go"), migrateCmd})
	}
	return m, result, notes, outs, nil
}

func generate(path string, o options, stdout, stderr io.Writer) error {
	_, result, notes, outs, err := build(path, o, true)
	if err != nil {
		return err
	}
	for _, n := range notes {
		level := "note"
		if n.Warning {
			level = "warning"
		}
		fmt.Fprintf(stderr, "%s: %s: %s %s\n", path, level, n.Entity, n.Message)
	}
	if failing := result.Failing(); len(failing) > 0 {
		printFindings(stderr, path, failing)
		return fmt.Errorf("%s: %s; nothing was written", path, failText(result))
	}
	var stale []string
	for _, out := range outs {
		current, err := os.ReadFile(out.path)
		if err == nil && bytes.Equal(current, out.data) {
			continue
		}
		if o.checkOnly {
			stale = append(stale, out.path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out.path, out.data, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", out.path)
	}
	if len(stale) > 0 {
		return fmt.Errorf("%s: generated files are out of date (run dynago generate): %s", path, strings.Join(stale, ", "))
	}
	return nil
}

func failText(r *analysis.Result) string {
	n := len(r.Failing())
	what := "open finding"
	if n != 1 {
		what += "s"
	}
	level := string(r.Policy.FailOn)
	if r.Policy.FailOn != analysis.Error {
		level += " or worse"
	}
	src := "the default policy"
	if r.Policy.Path != "" {
		src = r.Policy.Path
	}
	return fmt.Sprintf("the design has %d %s of severity %s, which %s fails on", n, what, level, src)
}

func check(path string, o options, w io.Writer) error {
	m, result, notes, _, err := build(path, o, false)
	if err != nil {
		return err
	}
	if o.json {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(arch.Build(result)); err != nil {
			return err
		}
		if len(result.Failing()) > 0 {
			return fmt.Errorf("%s: %s", path, failText(result))
		}
		return nil
	}
	report := result.Cost
	fmt.Fprintf(w, "%s: table %s, %s, %s\n", path, m.Table.Name, plural(len(m.Entities), "entity", "entities"), plural(len(m.GSIs), "GSI", "GSIs"))
	fmt.Fprint(w, "(a/b is typical/p99, or typical/largest; units are per call; ? is unknown until a volume is declared; $ is at the declared rates)\n\n")
	declared := false // any volume or rate, without which there is no cost to estimate
	for _, er := range report.Entities {
		declared = declared || er.Entity.Count > 0
		for _, rc := range er.Reads {
			declared = declared || rc.Access.Rate > 0
		}
		for _, wc := range er.Writes {
			declared = declared || wc.Write.Rate > 0
		}
	}
	for _, er := range report.Entities {
		e := er.Entity
		fmt.Fprintf(w, "%s (v%d)  item %s / %s", e.Name, e.Version, cost.Human(er.Item.P50), cost.Human(er.Item.P99))
		if e.Count > 0 {
			fmt.Fprintf(w, "  %s items", schema.Number(float64(int64(e.Count+0.5))))
		}
		if er.StorageGB > 0 {
			fmt.Fprintf(w, "  storage %.2f GB ($%.2f/month)", er.StorageGB, er.StorageUSD)
		}
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		for _, rc := range er.Reads {
			monthly := ""
			if rc.Monthly > 0 {
				monthly = fmt.Sprintf("$%.2f/month", rc.Monthly)
			}
			fmt.Fprintf(tw, "  read\t%s\t%s\t%s RRU\t%s\n", rc.Access.Name, rc.Requests, units(rc.RRU), monthly)
		}
		for _, wc := range er.Writes {
			kind := "single item"
			if wc.Transactional {
				kind = fmt.Sprintf("tx %d items", wc.MaxTxItems)
			}
			if wc.ReadFirst {
				kind += " + read"
			}
			if wc.BatchSize > 0 {
				kind = fmt.Sprintf("batch of %d, %d to a tx + read", wc.Write.Batch, wc.BatchSize)
			}
			monthly := ""
			if wc.Monthly > 0 {
				monthly = fmt.Sprintf("$%.2f/month", wc.Monthly)
			}
			fmt.Fprintf(tw, "  write\t%s\t%s\t%s WRU\t%s\n", wc.Write.Name, kind, units(wc.WRU), monthly)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "partitions (items per key: typical / largest; size: typical / largest; busiest key at peak)")
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, p := range result.Partitions {
		var holds []string
		for _, mb := range p.Members {
			h := mb.Label + " " + estimate(mb.Count)
			if mb.Bound && mb.Count.Known {
				h += " at most (only those matching its where)"
			}
			holds = append(holds, h)
		}
		busy, risk := "-", "-"
		if p.Rated {
			busy = fmt.Sprintf("%.2f WRU/s %.2f RRU/s", p.PeakWRU, p.PeakRRU)
			risk = fmt.Sprintf("%s (%s%% of a partition)", p.Risk, percent(p.Headroom))
		}
		grows := ""
		if p.Grows {
			grows = "grows"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s / %s\t%s\t%s\t%s\n", p.PK, p.Space(), strings.Join(holds, ", "),
			sizeOr(p.Size.Typical, p.Size.Known), sizeOr(p.Size.Max, p.Size.MaxKnown), grows, busy, risk)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	for _, n := range notes {
		fmt.Fprintf(w, "change: %s %s\n", n.Entity, n.Message)
	}
	printFindings(w, "", result.Open())
	for _, f := range result.Findings {
		if f.Accepted != nil {
			fmt.Fprintf(w, "accepted [%s] %s: %s\n  reason: %s\n", f.Rule, analysis.SubjectText(f.Subject), f.Message, f.Accepted.Reason)
		}
	}
	if declared {
		fmt.Fprintf(w, "\nestimated total: $%.2f/month at declared volumes and rates\n", report.MonthlyUSD)
	} else {
		fmt.Fprint(w, "\nestimated total: not estimated; declare a `volume` on entities and a `rate` on reads and writes (docs/guides/analysis.md)\n")
	}
	if len(result.Failing()) > 0 {
		return fmt.Errorf("%s: %s", path, failText(result))
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func estimate(e analysis.Estimate) string {
	switch {
	case !e.Known:
		return "?"
	case e.MaxKnown && e.Max != e.Typical:
		return schema.Number(round2(e.Typical)) + "/" + schema.Number(round2(e.Max))
	case e.MaxKnown:
		return schema.Number(round2(e.Typical))
	}
	return schema.Number(round2(e.Typical)) + "/?"
}

func percent(f float64) string {
	if f > 0 && f < 0.01 {
		return "<1"
	}
	return strconv.FormatFloat(f*100, 'f', 0, 64)
}

func round2(f float64) float64 {
	if f >= 10 {
		return float64(int64(f + 0.5))
	}
	return float64(int64(f*100+0.5)) / 100
}

func sizeOr(b float64, known bool) string {
	if !known {
		return "?"
	}
	return cost.HumanBytes(b)
}

func printFindings(w io.Writer, prefix string, fs []analysis.Finding) {
	for _, f := range fs {
		p := ""
		if prefix != "" {
			p = prefix + ": "
		}
		fmt.Fprintf(w, "%s%s [%s] %s: %s\n", p, f.Severity, f.Rule, analysis.SubjectText(f.Subject), f.Message)
	}
}

func units(u cost.Units) string {
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
	if u.P50 == u.P99 {
		return f(u.P50)
	}
	return f(u.P50) + "/" + f(u.P99)
}

// diff prints the architectural changes to a schema since a git ref or another file.
func diff(path string, o options, w io.Writer) error {
	pol, err := loadPolicy(path, o.policy)
	if err != nil {
		return err
	}
	m, err := schema.Load(path)
	if err != nil {
		return err
	}
	next := arch.Build(analysis.Analyze(m, o.prices, pol))
	var data []byte
	from := o.from
	if o.from != "" {
		if data, err = os.ReadFile(o.from); err != nil {
			return err
		}
	} else {
		ref := o.base
		if ref == "" {
			ref = "HEAD"
		}
		from = ref
		if data, err = gitShow(ref, path); err != nil {
			return err
		}
	}
	var prev *arch.Snapshot
	var st arch.Storage
	if data != nil {
		old, err := schema.ParseEarlier(data)
		if err != nil {
			return fmt.Errorf("%s at %s: %w", path, from, err)
		}
		prev = arch.Build(analysis.Analyze(old, o.prices, pol))
		// The lock file recorded with the base schema, so the diff judges storage changes as
		// generate will.
		var lockData []byte
		if o.from != "" {
			lockData, err = os.ReadFile(filepath.Join(filepath.Dir(o.from), old.Output.Lock))
			if errors.Is(err, os.ErrNotExist) {
				lockData, err = nil, nil
			}
		} else {
			lockData, err = gitShow(from, filepath.Join(filepath.Dir(path), old.Output.Lock))
		}
		if err != nil {
			return err
		}
		var baseLock *lock.File
		if lockData != nil {
			if baseLock, err = lock.Parse(lockData); err != nil {
				return fmt.Errorf("%s at %s: %w", old.Output.Lock, from, err)
			}
		}
		st = arch.CheckStorage(old, baseLock, m)
	}
	to := path
	if o.from == "" {
		to = "working tree"
	}
	out := arch.Diff(prev, next, st, from, to)
	if out == "" {
		fmt.Fprintf(w, "`%s`: no architectural changes (%s → %s).\n", m.Table.Name, from, to)
		return nil
	}
	fmt.Fprint(w, out)
	return nil
}

// gitShow returns a file's content at a git ref, or nil if the file doesn't exist there.
func gitShow(ref, path string) ([]byte, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	cmd := exec.Command("git", "-C", dir, "show", ref+":./"+base)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "does not exist") || strings.Contains(msg, "exists on disk, but not in") {
			return nil, nil
		}
		return nil, fmt.Errorf("git show %s:%s: %s", ref, path, strings.TrimSpace(msg))
	}
	return out, nil
}

func vetCmd(patterns []string, o options, stdout, stderr io.Writer) int {
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	res, err := vet.Run(wd, patterns, o.tests)
	if err != nil {
		fmt.Fprintf(stderr, "dynago vet: %v\n", err)
		return 1
	}
	for _, c := range res.Unmarked {
		why := "declare the read or write in the schema, or mark the call //dynago:raw <reason>"
		if c.Marked {
			why = "the //dynago:raw mark needs a reason"
		}
		fmt.Fprintf(stderr, "%s: DynamoDB call outside generated code: %s: %s\n", c.Pos, c.Name, why)
	}
	if o.list {
		for _, c := range res.Marked {
			fmt.Fprintf(stdout, "%s: %s: %s\n", c.Pos, c.Name, c.Reason)
		}
	}
	if len(res.Unmarked) > 0 {
		fmt.Fprintf(stderr, "dynago vet: %d DynamoDB calls bypass the schema\n", len(res.Unmarked))
		return 1
	}
	return 0
}

// goImportPath returns the import path of the Go package in dir, from the nearest go.mod above it.
func goImportPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for root := abs; ; root = filepath.Dir(root) {
		data, err := os.ReadFile(filepath.Join(root, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if mod, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					rel, err := filepath.Rel(root, abs)
					if err != nil {
						return "", err
					}
					return strings.TrimSuffix(strings.Trim(mod, `"`)+"/"+filepath.ToSlash(rel), "/."), nil
				}
			}
			return "", fmt.Errorf("%s has no module line", filepath.Join(root, "go.mod"))
		}
		if filepath.Dir(root) == root {
			return "", fmt.Errorf("no go.mod above %s", abs)
		}
	}
}
