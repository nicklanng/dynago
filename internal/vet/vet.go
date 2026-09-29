// Package vet finds DynamoDB calls made outside generated code: requests through the AWS SDK's
// DynamoDB client, guregu/dynamo, or dynago's runtime, which bypass the schema's declared reads
// and writes. A call that must stay can be marked `//dynago:raw <reason>`, on its line, the line
// above, or its function's doc comment; the reason is required and listed.
package vet

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Directive marks a raw call as deliberate: `//dynago:raw the export job reads every item`.
const Directive = "//dynago:raw"

// Call is one DynamoDB call outside generated code.
type Call struct {
	Pos token.Position
	// Name is the function or method called: "(*dynamo.Table).Put".
	Name string
	// Reason is the //dynago:raw reason, or "" if the call isn't marked.
	Reason string
	// Marked is true when the call carries the directive (with or without a reason).
	Marked bool
}

// Result is what vet found.
type Result struct {
	// Unmarked calls fail vet; marked ones are listed.
	Unmarked, Marked []Call
}

// packages whose methods make DynamoDB requests.
var requestPackages = map[string]bool{
	"github.com/guregu/dynamo":                      true,
	"github.com/guregu/dynamo/v2":                   true,
	"github.com/aws/aws-sdk-go-v2/service/dynamodb": true,
	"github.com/aws/aws-sdk-go/service/dynamodb":    true,
}

// runtimeCalls are dynago's runtime functions that make requests for generated code.
var runtimeCalls = map[string]bool{
	"GetOne": true, "GetMany": true, "Query": true, "Scan": true, "Run": true, "UpdateFields": true,
	"DeleteIfExists": true, "CheckRequirement": true, "ConsumeRequirement": true, "CheckAbsent": true,
	"CheckCounter": true, "PutOp": true, "CreateOp": true, "UpdateOp": true, "DeleteOp": true, "CheckOp": true,
}

const runtimePkg = "github.com/nicklanng/dynago"

// Run loads the packages matching patterns (as go build would, from dir) and finds DynamoDB calls
// outside generated files. Tests are included when tests is true.
func Run(dir string, patterns []string, tests bool) (*Result, error) {
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:   dir,
		Tests: tests,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	var errs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	})
	if len(errs) > 0 {
		return nil, fmt.Errorf("loading packages: %s", strings.Join(errs, "; "))
	}
	res := &Result{}
	seen := map[token.Position]bool{}
	for _, p := range pkgs {
		if p.PkgPath == runtimePkg || p.PkgPath == runtimePkg+"/dynagotest" {
			continue // dynago's runtime, which generated code calls
		}
		for _, f := range p.Syntax {
			if ast.IsGenerated(f) {
				continue
			}
			for _, c := range check(p.Fset, p.TypesInfo, f) {
				if seen[c.Pos] {
					continue // a package and its test variant share files
				}
				seen[c.Pos] = true
				if dir != "" {
					if rel, err := filepath.Rel(dir, c.Pos.Filename); err == nil {
						c.Pos.Filename = rel
					}
				}
				if c.Marked && c.Reason != "" {
					res.Marked = append(res.Marked, c)
				} else {
					res.Unmarked = append(res.Unmarked, c)
				}
			}
		}
	}
	for _, l := range [][]Call{res.Unmarked, res.Marked} {
		sort.Slice(l, func(i, j int) bool {
			if l[i].Pos.Filename != l[j].Pos.Filename {
				return l[i].Pos.Filename < l[j].Pos.Filename
			}
			return l[i].Pos.Offset < l[j].Pos.Offset
		})
	}
	return res, nil
}

// check finds the DynamoDB calls in one file.
func check(fset *token.FileSet, info *types.Info, f *ast.File) []Call {
	// Directives by line: a trailing one marks its own line; one alone on its line marks the next.
	var src []string
	if data, err := os.ReadFile(fset.Position(f.Pos()).Filename); err == nil {
		src = strings.Split(string(data), "\n")
	}
	trailing, above := map[int]string{}, map[int]string{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			reason, ok := directive(c.Text)
			if !ok {
				continue
			}
			p := fset.Position(c.Pos())
			if p.Line-1 < len(src) && strings.HasPrefix(strings.TrimSpace(src[p.Line-1]), "//") {
				above[p.Line+1] = reason
			} else {
				trailing[p.Line] = reason
			}
		}
	}
	var out []Call
	var funcMark *string
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			var mark *string
			if n.Doc != nil {
				for _, c := range n.Doc.List {
					if reason, ok := directive(c.Text); ok {
						mark = &reason
					}
				}
			}
			prev := funcMark
			funcMark = mark
			if n.Body != nil {
				ast.Inspect(n.Body, visit)
			}
			funcMark = prev
			return false
		case *ast.CallExpr:
			name, at, ok := callee(info, n.Fun)
			if !ok {
				return true
			}
			// Report the called name's position (calls chained on one receiver all start at the
			// receiver), but match marks by where the chain starts, so a mark above a chain that
			// spans lines covers all of it.
			pos := fset.Position(at)
			start := fset.Position(n.Pos()).Line
			c := Call{Pos: pos, Name: name}
			if funcMark != nil {
				c.Marked, c.Reason = true, *funcMark
			}
			if reason, ok := above[start]; ok {
				c.Marked, c.Reason = true, reason
			}
			for line := start; line <= pos.Line; line++ {
				if reason, ok := trailing[line]; ok {
					c.Marked, c.Reason = true, reason
				}
			}
			out = append(out, c)
		}
		return true
	}
	ast.Inspect(f, visit)
	return out
}

func directive(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, Directive)
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// callee names the function a call expression calls, and where its name is, if it makes
// DynamoDB requests.
func callee(info *types.Info, fun ast.Expr) (string, token.Pos, bool) {
	var id *ast.Ident
	switch f := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		id = f.Sel
	case *ast.Ident:
		id = f
	case *ast.IndexExpr: // generic instantiation
		return callee(info, f.X)
	default:
		return "", 0, false
	}
	name, ok := request(info, id)
	return name, id.Pos(), ok
}

// request names the function an identifier refers to, if calling it makes DynamoDB requests.
func request(info *types.Info, id *ast.Ident) (string, bool) {
	fn, ok := info.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return "", false
	}
	path := fn.Pkg().Path()
	sig, _ := fn.Type().(*types.Signature)
	method := sig != nil && sig.Recv() != nil
	if method && types.IsInterface(sig.Recv().Type()) && takesRequestInput(sig) {
		// An interface over the client, such as SDK v1's dynamodbiface.DynamoDBAPI or one of your
		// own: its methods take the SDK's request inputs.
		return methodName(fn, sig), true
	}
	switch {
	case path == runtimePkg:
		if method || !runtimeCalls[fn.Name()] {
			return "", false
		}
		return "dynago." + fn.Name(), true
	case !requestPackages[path]:
		return "", false
	case method && strings.Contains(path, "aws-sdk-go"):
		// The AWS SDK's requests are methods of its client; other methods (on options, types) aren't.
		if n := recvName(sig); (n != "Client" && n != "DynamoDB") || fn.Name() == "Options" {
			return "", false
		}
		return methodName(fn, sig), true
	case method:
		return methodName(fn, sig), true
	case strings.HasPrefix(fn.Name(), "New") && (strings.HasSuffix(fn.Name(), "Paginator") || strings.HasSuffix(fn.Name(), "Waiter")):
		return fn.Pkg().Name() + "." + fn.Name(), true
	}
	return "", false
}

// takesRequestInput reports whether a function takes one of the AWS SDK's DynamoDB request inputs
// (*dynamodb.GetItemInput and the like).
func takesRequestInput(sig *types.Signature) bool {
	for i := 0; i < sig.Params().Len(); i++ {
		t := sig.Params().At(i).Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		n, ok := t.(*types.Named)
		if !ok || n.Obj().Pkg() == nil {
			continue
		}
		if strings.Contains(n.Obj().Pkg().Path(), "aws-sdk-go") && strings.HasSuffix(n.Obj().Pkg().Path(), "/dynamodb") && strings.HasSuffix(n.Obj().Name(), "Input") {
			return true
		}
	}
	return false
}

func recvName(sig *types.Signature) string {
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return n.Obj().Name()
	}
	return ""
}

func methodName(fn *types.Func, sig *types.Signature) string {
	t := sig.Recv().Type()
	ptr := ""
	if p, ok := t.(*types.Pointer); ok {
		t, ptr = p.Elem(), "*"
	}
	name := t.String()
	if n, ok := t.(*types.Named); ok {
		name = n.Obj().Pkg().Name() + "." + n.Obj().Name()
	}
	return fmt.Sprintf("(%s%s).%s", ptr, name, fn.Name())
}
