package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The setup-mode path and the normal path used to be two copies of the startup
// sequence, and they drifted: a freshly-onboarded install never got the request
// context, the hardware-decode preference, or the software decoder install, so
// it behaved differently from the same install after one restart. The two paths
// now share one body. This guards that shape: each startup step must have
// exactly one call site in the package, because a second one is how the copies
// came back last time.
func TestStartupStepsHaveASingleCallSite(t *testing.T) {
	steps := []string{
		"applyHWAccelPreference",
		"ensureOpenH264",
		"initSubsystems",
		"runEventLoop",
		"startOnvifSubscribers",
		"SetContext",
		"SetSubsystems",
		"awaitShutdown",
	}

	counts := map[string]int{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				counts[fn.Name]++
			case *ast.SelectorExpr:
				counts[fn.Sel.Name]++
			}
			return true
		})
	}

	for _, step := range steps {
		// awaitShutdown is called once per startup outcome: once from the
		// abandoned-setup path and once from the running NVR.
		want := 1
		if step == "awaitShutdown" {
			want = 2
		}
		if counts[step] != want {
			t.Errorf("%s has %d call sites, want %d: the startup sequence has been forked again, and the copies will drift",
				step, counts[step], want)
		}
	}
}
