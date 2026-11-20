package main

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// openAPIMux is the router with every optional route switched on.
func openAPIMux() *http.ServeMux {
	return buildMux(&Config{StatsToken: "t"}, nil, nil, nil)
}

// registeredPatterns returns every "METHOD /path" pattern passed as a string
// literal to Handle or HandleFunc in the package's non-test source.
func registeredPatterns(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	var out []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if p, ok := handlePattern(n); ok {
				out = append(out, p)
			}
			return true
		})
	}
	return out
}

func handlePattern(n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// specPath turns a mux pattern's path into the OpenAPI path that documents it:
// "/{$}" is "/", and a prefix pattern "/static/" is "/static/{path}".
func specPath(p string) string {
	switch {
	case p == "/{$}":
		return "/"
	case strings.HasSuffix(p, "/"):
		return p + "{path}"
	}
	return p
}

// specOperations reads "METHOD /path" pairs from the paths section of
// docs/openapi.yaml: path keys are indented two spaces, methods four.
func specOperations(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pathRe := regexp.MustCompile(`^  (/[^:]*):\s*$`)
	opRe := regexp.MustCompile(`^    (get|put|post|delete|patch|head|options|trace):`)
	var out []string
	inPaths, cur := false, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, " ") && line != "" {
			inPaths = line == "paths:"
			continue
		}
		if !inPaths {
			continue
		}
		if m := pathRe.FindStringSubmatch(line); m != nil {
			cur = m[1]
		} else if m := opRe.FindStringSubmatch(line); m != nil {
			out = append(out, strings.ToUpper(m[1])+" "+cur)
		}
	}
	return out
}

// TestOpenAPIMatchesRoutes keeps docs/openapi.yaml and the router in step: the
// spec documents exactly the routes the code registers, and the router really
// serves each of them.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	mux := openAPIMux()
	code := map[string]bool{}
	for _, p := range registeredPatterns(t) {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			t.Errorf("%q has no method", p)
			continue
		}
		code[method+" "+specPath(path)] = true
		url := strings.NewReplacer("{$}", "", "{id}", "1", "{name}", "x").Replace(path)
		if strings.HasSuffix(url, "/") && url != "/" {
			url += "x"
		}
		req := httptest.NewRequest(method, url, nil)
		if _, got := mux.Handler(req); got != p {
			t.Errorf("router serves %s %s with %q, want %q", method, url, got, p)
		}
	}
	spec := map[string]bool{}
	for _, op := range specOperations(t) {
		spec[op] = true
	}
	if len(code) == 0 || len(spec) == 0 {
		t.Fatalf("found %d routes in code and %d in the spec", len(code), len(spec))
	}
	for _, k := range sortedKeys(code) {
		if !spec[k] {
			t.Errorf("route %s is not in docs/openapi.yaml", k)
		}
	}
	for _, k := range sortedKeys(spec) {
		if !code[k] {
			t.Errorf("docs/openapi.yaml documents %s, which the code does not register", k)
		}
	}
	if !t.Failed() {
		t.Logf("%d operations match", len(spec))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
