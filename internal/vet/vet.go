// Package vet finds DynamoDB calls made outside generated code: requests through the AWS SDK's
// DynamoDB client, guregu/dynamo, or dynago's runtime, which bypass the schema's declared reads
// and writes, and function values that would (`retry(c.PutItem)`). A call that must stay can be
// marked `//dynago:raw <reason>`: at the end of its line or its statement's last line, alone above
// its statement, or in its function's doc comment; the reason is required and listed.
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
	"GetOne": true, "GetMany": true, "GetBatch": true, "BatchWrite": true, "Query": true, "Scan": true, "Run": true, "UpdateFields": true,
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

// check finds the DynamoDB calls in one file, and the method values and function values that
// would make them (`retry(c.PutItem)`).
func check(fset *token.FileSet, info *types.Info, f *ast.File) []Call {
	// Directives by line: a trailing one marks its own line; one alone on its line marks the line
	// after its comment group, so explanation may follow it.
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
				above[fset.Position(cg.End()).Line+1] = reason
			} else {
				trailing[p.Line] = reason
			}
		}
	}
	line := func(p token.Pos) int { return fset.Position(p).Line }
	var out []Call
	// called holds the callee expressions of calls, which are reported as calls, not as values.
	called := map[ast.Expr]bool{}
	ast.PreorderStack(f, nil, func(n ast.Node, stack []ast.Node) bool {
		var name string
		var at token.Pos
		var ok bool
		var start, end int // the lines the call or value spans, for marks
		switch n := n.(type) {
		case *ast.CallExpr:
			called[calleeExpr(n.Fun)] = true
			if name, at, ok = callee(info, n.Fun); !ok {
				return true
			}
			// Marks match by where the chain starts (calls chained on one receiver all start at
			// the receiver) and where the outermost call of the chain ends, so a mark above a
			// chain spread over lines, or after its closing `})`, covers all of it.
			start, end = line(n.Pos()), line(chainEnd(n, stack).End())
		case *ast.SelectorExpr:
			if called[n] {
				return true
			}
			if name, ok = request(info, n.Sel); !ok {
				return true
			}
			at, start, end = n.Sel.Pos(), line(n.Pos()), line(n.End())
		default:
			return true
		}
		pos := fset.Position(at)
		c := Call{Pos: pos, Name: name}
		mark := func(reason string) { c.Marked, c.Reason = true, reason }
		if reason, ok := funcMark(stack); ok {
			mark(reason)
		}
		// Above the call or its statement; trailing on any line from its start to its name, on its
		// last line, or on its statement's last line.
		lines := []int{start}
		stmt := enclosingStmt(stack)
		if stmt != nil {
			lines = append(lines, line(stmt.Pos()))
		}
		for _, l := range lines {
			if reason, ok := above[l]; ok {
				mark(reason)
			}
		}
		lines = []int{end}
		for l := start; l <= pos.Line; l++ {
			lines = append(lines, l)
		}
		if simpleStmt(stmt) {
			lines = append(lines, line(stmt.End()))
		}
		for _, l := range lines {
			if reason, ok := trailing[l]; ok {
				mark(reason)
			}
		}
		out = append(out, c)
		return true
	})
	return out
}

// calleeExpr strips parentheses and generic instantiation from a call's function.
func calleeExpr(fun ast.Expr) ast.Expr {
	for {
		switch f := ast.Unparen(fun).(type) {
		case *ast.IndexExpr:
			fun = f.X
		case *ast.IndexListExpr:
			fun = f.X
		default:
			return f
		}
	}
}

// chainEnd returns the outermost call of the chain a call is the receiver of: for
// `db.Table("x").Get("PK", "a").All(ctx, &out)` and any of its calls, the call to All.
func chainEnd(call *ast.CallExpr, stack []ast.Node) *ast.CallExpr {
	var cur ast.Expr = call
	for i := len(stack) - 1; i >= 1; i -= 2 {
		sel, ok := stack[i].(*ast.SelectorExpr)
		if !ok || ast.Unparen(sel.X) != cur {
			break
		}
		next, ok := stack[i-1].(*ast.CallExpr)
		if !ok || calleeExpr(next.Fun) != sel {
			break
		}
		call, cur = next, next
	}
	return call
}

// funcMark returns the //dynago:raw reason in the doc comment of the function declaration
// enclosing a node.
func funcMark(stack []ast.Node) (string, bool) {
	for i := len(stack) - 1; i >= 0; i-- {
		fd, ok := stack[i].(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Doc == nil {
			return "", false
		}
		var reason string
		var found bool
		for _, c := range fd.Doc.List {
			if r, ok := directive(c.Text); ok {
				reason, found = r, true
			}
		}
		return reason, found
	}
	return "", false
}

// enclosingStmt returns the innermost statement enclosing a node, or nil outside any.
func enclosingStmt(stack []ast.Node) ast.Stmt {
	for i := len(stack) - 1; i >= 0; i-- {
		switch s := stack[i].(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return nil
		case ast.Stmt:
			return s
		}
	}
	return nil
}

// simpleStmt reports whether a statement holds no block, so a mark at the end of its last line is
// about it, not about a block's closing brace.
func simpleStmt(s ast.Stmt) bool {
	switch s.(type) {
	case *ast.ExprStmt, *ast.AssignStmt, *ast.ReturnStmt, *ast.DeclStmt, *ast.GoStmt,
		*ast.DeferStmt, *ast.SendStmt, *ast.IncDecStmt:
		return true
	}
	return false
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
