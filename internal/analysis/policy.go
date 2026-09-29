package analysis

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PolicyFile is the name dynago looks for, from a schema's directory upwards.
const PolicyFile = "dynago.policy.yaml"

// Policy is a team's rules for every schema in a repository: which findings fail a build, rule
// severities, limits stricter than DynamoDB's, and what schemas must declare. Schemas record
// their own exceptions (accept); the policy holds what applies to all of them.
type Policy struct {
	// Path is the file the policy came from, or "" for the default.
	Path string
	// FailOn is the least severe open finding that fails check and generate.
	FailOn Severity
	// Rules overrides rule severities: error, warning, note or off.
	Rules   map[string]string
	Limits  Limits
	Require Require
}

// Limits are thresholds stricter than DynamoDB's own. Zero means no limit.
type Limits struct {
	// ItemSize is the largest p99 item size, in bytes.
	ItemSize int
	// PartitionSize is the largest estimated partition size, in bytes.
	PartitionSize float64
	// TransactionItems is the most items one write may touch.
	TransactionItems int
	// GSIs is the most global secondary indexes per table.
	GSIs int
	// IndexesPerEntity is the most indexes (GSIs and copies) per entity.
	IndexesPerEntity int
}

// Require lists what every schema must declare.
type Require struct {
	Volumes   bool
	Rates     bool
	Freshness bool
}

// DefaultPolicy fails on errors only, with every rule at its default severity.
func DefaultPolicy() *Policy { return &Policy{FailOn: Error, Rules: map[string]string{}} }

type rawPolicy struct {
	FailOn  string            `yaml:"fail_on"`
	Rules   map[string]string `yaml:"rules"`
	Limits  rawLimits         `yaml:"limits"`
	Require rawRequire        `yaml:"require"`
}

type rawLimits struct {
	ItemSize         string `yaml:"item_size"`
	PartitionSize    string `yaml:"partition_size"`
	TransactionItems int    `yaml:"transaction_items"`
	GSIs             int    `yaml:"gsis"`
	IndexesPerEntity int    `yaml:"indexes_per_entity"`
}

type rawRequire struct {
	Volumes   bool `yaml:"volumes"`
	Rates     bool `yaml:"rates"`
	Freshness bool `yaml:"freshness"`
}

// FindPolicy looks for dynago.policy.yaml in dir and its parents, up to the repository's root (the
// first directory with a .git) or, outside a repository, the module's (the first with a go.mod).
// It returns "" if there is none.
func FindPolicy(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(abs, PolicyFile)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if root(abs) {
			return "", nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", nil
		}
		abs = parent
	}
}

// root reports whether dir is where the search for a policy stops: a repository's root, or a module's
// root outside any repository.
func root(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return false
	}
	// A module inside a repository keeps looking up to the repository's root.
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return false
		}
		if filepath.Dir(d) == d {
			return true
		}
	}
}

// LoadPolicy reads a policy file.
func LoadPolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParsePolicy(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p.Path = path
	return p, nil
}

// ParsePolicy parses policy YAML.
func ParsePolicy(data []byte) (*Policy, error) {
	var raw rawPolicy
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if err := checkKeys(&root, reflect.TypeOf(raw), ""); err != nil {
		return nil, err
	}
	if err := root.Decode(&raw); err != nil && len(root.Content) > 0 {
		return nil, err
	}
	p := DefaultPolicy()
	var errs []error
	switch raw.FailOn {
	case "":
	case "error", "warning", "note":
		p.FailOn = Severity(raw.FailOn)
	default:
		errs = append(errs, fmt.Errorf("fail_on: %q must be error, warning or note", raw.FailOn))
	}
	for id, sev := range raw.Rules {
		rule := RuleByID(id)
		switch {
		case rule == nil:
			errs = append(errs, fmt.Errorf("rules: there is no rule %q", id))
		case rule.Hard:
			errs = append(errs, fmt.Errorf("rules: %s reports a DynamoDB limit, which a policy can't change", id))
		case sev != "error" && sev != "warning" && sev != "note" && sev != "off":
			errs = append(errs, fmt.Errorf("rules: %s: %q must be error, warning, note or off", id, sev))
		default:
			p.Rules[id] = sev
		}
	}
	if raw.Limits.ItemSize != "" {
		n, err := ParseBytes(raw.Limits.ItemSize)
		if err != nil {
			errs = append(errs, fmt.Errorf("limits.item_size: %w", err))
		}
		p.Limits.ItemSize = int(n)
	}
	if raw.Limits.PartitionSize != "" {
		n, err := ParseBytes(raw.Limits.PartitionSize)
		if err != nil {
			errs = append(errs, fmt.Errorf("limits.partition_size: %w", err))
		}
		p.Limits.PartitionSize = n
	}
	if raw.Limits.TransactionItems < 0 || raw.Limits.GSIs < 0 || raw.Limits.IndexesPerEntity < 0 {
		errs = append(errs, errors.New("limits: counts must be positive"))
	}
	p.Limits.TransactionItems, p.Limits.GSIs, p.Limits.IndexesPerEntity = raw.Limits.TransactionItems, raw.Limits.GSIs, raw.Limits.IndexesPerEntity
	p.Require = Require(raw.Require)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return p, nil
}

// checkKeys rejects keys the policy doesn't define, so a typo isn't silently ignored.
func checkKeys(n *yaml.Node, t reflect.Type, where string) error {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if t.Kind() != reflect.Struct || n.Kind != yaml.MappingNode {
		return nil
	}
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fields[strings.Split(f.Tag.Get("yaml"), ",")[0]] = f.Type
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		ft, ok := fields[k]
		if !ok {
			var known []string
			for name := range fields {
				known = append(known, name)
			}
			sort.Strings(known)
			return fmt.Errorf("line %d: unknown key %q%s (expected one of: %s)", n.Content[i].Line, k, where, strings.Join(known, ", "))
		}
		if err := checkKeys(n.Content[i+1], ft, " in "+k); err != nil {
			return err
		}
	}
	return nil
}

// ParseBytes parses a size such as 400KB, 5GB or 1024 (bytes). Units are binary: 1KB is 1024 bytes.
func ParseBytes(s string) (float64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := 1.0
	for _, u := range []struct {
		suffix string
		mult   float64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size (e.g. 64KB, 5GB)", s)
	}
	return n * mult, nil
}
