// Command dynago generates typed DynamoDB access code, documentation and infrastructure from a
// schema file, and reports the cost and risks of the design.
//
//	dynago generate [-check] [-new-history] schema.dynago.yaml...
//	dynago check [-prices wru,rru,gb] [-new-history] schema.dynago.yaml...
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/gen/docs"
	"github.com/nicklanng/dynago/internal/gen/gocode"
	"github.com/nicklanng/dynago/internal/gen/infra"
	"github.com/nicklanng/dynago/internal/lock"
	"github.com/nicklanng/dynago/internal/schema"
)

const usage = `dynago turns a schema file into typed DynamoDB access code, docs and infrastructure.

Usage:
  dynago generate [-check] [-prices wru,rru,gb] [-new-history] <schema.dynago.yaml>...
      Write the Go store, the model document, Terraform, the CreateTable JSON and the lock file
      next to each schema. With -check, write nothing and fail if any output is out of date.

  dynago check [-prices wru,rru,gb] [-new-history] <schema.dynago.yaml>...
      Validate each schema and print its cost and risk report. Fails on errors.

Flags may come before or after the schema files.

  -prices        on-demand prices: $ per million WRU, $ per million RRU, $ per GB-month
                 (default 0.625,0.125,0.25, us-east-1)
  -new-history   accept a schema whose history the lock file doesn't have (entities above
                 version 1, or a table above generation 1), starting the history there. Only right
                 for a new table: otherwise restore the lock file instead.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
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
	checkOnly := fs.Bool("check", false, "fail if generated files are out of date instead of writing them")
	pricesFlag := fs.String("prices", "", "on-demand prices: wru,rru,gb")
	newHistory := fs.Bool("new-history", false, "start a lock history the lock file doesn't have")
	switch cmd {
	case "generate", "check":
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
	if len(files) == 0 {
		fmt.Fprintf(stderr, "dynago %s: no schema files given\n", cmd)
		return 2
	}
	prices := cost.DefaultPrices
	if *pricesFlag != "" {
		p, err := parsePrices(*pricesFlag)
		if err != nil {
			fmt.Fprintf(stderr, "dynago: -prices: %v\n", err)
			return 2
		}
		prices = p
	}
	status := 0
	lockOpts := lock.Options{NewHistory: *newHistory}
	for _, path := range files {
		var err error
		if cmd == "generate" {
			err = generate(path, prices, lockOpts, *checkOnly, stdout, stderr)
		} else {
			err = check(path, prices, lockOpts, stdout)
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

// output is one generated file.
type output struct {
	path string
	data []byte
}

// build loads a schema and renders every output, without writing anything.
func build(path string, prices cost.Prices, lockOpts lock.Options) (*schema.Model, *cost.Report, []lock.Note, []output, error) {
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
	next, notes, err := lock.Apply(m, prev, lockOpts)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	report := cost.Analyze(m, prices)

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
		{filepath.Join(dir, m.Output.Docs), docs.Generate(m, report, source)},
		{filepath.Join(dir, m.Output.Terraform), tf},
		{filepath.Join(dir, m.Output.TableJSON), tableJSON},
		{filepath.Join(dir, m.Output.Lock), lockData},
	}
	if migrateCmd != nil {
		outs = append(outs, output{filepath.Join(dir, m.Output.MigrateCmd, "main.go"), migrateCmd})
	}
	return m, report, notes, outs, nil
}

func generate(path string, prices cost.Prices, lockOpts lock.Options, checkOnly bool, stdout, stderr io.Writer) error {
	_, report, notes, outs, err := build(path, prices, lockOpts)
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
	if report.HasErrors() {
		printFindings(stderr, path, report, cost.Error)
		return fmt.Errorf("%s: the design has errors; nothing was written", path)
	}
	var stale []string
	for _, o := range outs {
		current, err := os.ReadFile(o.path)
		if err == nil && bytes.Equal(current, o.data) {
			continue
		}
		if checkOnly {
			stale = append(stale, o.path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(o.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(o.path, o.data, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", o.path)
	}
	if len(stale) > 0 {
		return fmt.Errorf("%s: generated files are out of date (run dynago generate): %s", path, strings.Join(stale, ", "))
	}
	return nil
}

func check(path string, prices cost.Prices, lockOpts lock.Options, w io.Writer) error {
	m, report, notes, _, err := build(path, prices, lockOpts)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: table %s, %d entities, %d GSIs\n\n", path, m.Table.Name, len(m.Entities), len(m.GSIs))
	for _, er := range report.Entities {
		e := er.Entity
		fmt.Fprintf(w, "%s (v%d)  item %s / %s", e.Name, e.Version, cost.Human(er.Item.P50), cost.Human(er.Item.P99))
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
	for _, n := range notes {
		fmt.Fprintf(w, "change: %s %s\n", n.Entity, n.Message)
	}
	printFindings(w, "", report, "")
	fmt.Fprintf(w, "\nestimated total: $%.2f/month at declared volumes\n", report.MonthlyUSD)
	if report.HasErrors() {
		return fmt.Errorf("%s: the design has errors", path)
	}
	return nil
}

func printFindings(w io.Writer, prefix string, r *cost.Report, only cost.Severity) {
	for _, f := range r.Findings {
		if only != "" && f.Severity != only {
			continue
		}
		p := ""
		if prefix != "" {
			p = prefix + ": "
		}
		fmt.Fprintf(w, "%s%s: %s: %s\n", p, f.Severity, f.Subject, f.Message)
	}
}

func units(u cost.Units) string {
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
	if u.P50 == u.P99 {
		return f(u.P50)
	}
	return f(u.P50) + "/" + f(u.P99)
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
