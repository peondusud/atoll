package disks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

// crashPointsOfTheCode gives every point the code passes to a Machine's Checkpoint, each once, in byte order: the text
// a call gives itself, or, when it passes a parameter of its function on, the text each call of that function gives
// there, as ops.setMetadata takes its point. Anything else fails t, as a point the test cannot read.
func crashPointsOfTheCode(t *testing.T, root string) []string {
	t.Helper()
	type passedOn struct {
		fn    string
		index int
	}
	var files []*ast.File
	set := token.NewFileSet()
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(set, path, nil, parser.SkipObjectResolution)
			if err == nil {
				files = append(files, f)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	literal := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(lit.Value)
		return text, err == nil
	}
	points := map[string]bool{}
	var passed []passedOn
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, name := callee(n)
				if name != "Checkpoint" || len(call.Args) != 1 {
					return true
				}
				if text, ok := literal(call.Args[0]); ok {
					points[text] = true
					return true
				}
				param, _ := call.Args[0].(*ast.Ident)
				index := -1
				if param != nil {
					index = slices.Index(paramNames(fn), param.Name)
				}
				if index < 0 {
					t.Errorf("%s: a Checkpoint whose point is neither a text nor a parameter of %s", set.Position(call.Pos()), fn.Name.Name)
					return true
				}
				passed = append(passed, passedOn{fn.Name.Name, index})
				return true
			})
		}
	}
	for _, on := range passed {
		calls := 0
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, name := callee(n)
				if name != on.fn || len(call.Args) <= on.index {
					return true
				}
				calls++
				if text, ok := literal(call.Args[on.index]); ok {
					points[text] = true
				} else {
					t.Errorf("%s: %s gives a point that is no text", set.Position(call.Pos()), on.fn)
				}
				return true
			})
		}
		if calls == 0 {
			t.Errorf("%s passes its point on, and nothing calls it", on.fn)
		}
	}
	out := make([]string, 0, len(points))
	for p := range points {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// callee gives a node that is a call, and the name of what it calls, a function's or a method's; "" for anything else.
func callee(n ast.Node) (*ast.CallExpr, string) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return nil, ""
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return call, fun.Sel.Name
	case *ast.Ident:
		return call, fun.Name
	}
	return call, ""
}

// paramNames gives the names of a function's parameters, in order, one for each.
func paramNames(fn *ast.FuncDecl) []string {
	var out []string
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			out = append(out, "")
		}
		for _, n := range field.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// crashPointsOfTheTable gives the points the table of conformance/scenarios/README.md names, in its order: the first
// column of each row below "| Point | Reached |", which may name several.
func crashPointsOfTheTable(t *testing.T) []string {
	t.Helper()
	text := string(vectors.RepoFile(t, "conformance/scenarios/README.md"))
	_, table, found := strings.Cut(text, "\n| Point | Reached |\n|---|---|\n")
	if !found {
		t.Fatal("no table of crash points")
	}
	named := regexp.MustCompile("`([^`]+)`")
	var out []string
	for line := range strings.Lines(table) {
		if !strings.HasPrefix(line, "| ") {
			break
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(line, "| "), " |")
		for _, m := range named.FindAllStringSubmatch(first, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// Every point a conformance build may stop at, crash_at, is in the table of the scenarios' README, which the Rust
// version follows, and the table names no other: xattrs-set, unlinked and the view's points of a move were in the code
// alone.
func TestCrashPointsAreTheTable(t *testing.T) {
	code := crashPointsOfTheCode(t, filepath.Join("..", ".."))
	table := crashPointsOfTheTable(t)
	listed := slices.Clone(table)
	slices.Sort(listed)
	if len(slices.Compact(slices.Clone(listed))) != len(listed) {
		t.Errorf("the table names a point twice: %v", table)
	}
	for _, p := range code {
		if !slices.Contains(listed, p) {
			t.Errorf("%s: a point of the code that the table of conformance/scenarios/README.md does not name", p)
		}
	}
	for _, p := range listed {
		if !slices.Contains(code, p) {
			t.Errorf("%s: a point of the table that no code reaches", p)
		}
	}
	if len(code) < 20 {
		t.Errorf("only %d points found in the code: %v", len(code), code)
	}
}
