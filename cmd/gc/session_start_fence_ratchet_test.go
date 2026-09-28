package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sessionStartFenceAllowlist names every runtime-creating call site that is
// NOT lexically inside a session-start fence bracket, keyed
// "path|enclosing function|method", with the reason it is safe. A new entry
// needs a reason a reviewer can check; an entry the tree no longer contains
// fails the test too, so the list only shrinks with the code.
var sessionStartFenceAllowlist = map[string]string{
	"cmd/gc/status_provider.go|(*statusProvider).Start|Start":                        "forwarding wrapper used only by `gc status` (newStatusSessionProviderForCityWithSnapshot), a separate CLI process; the controller never holds it",
	"cmd/gc/status_provider.go|(*statusProvider).Relaunch|Relaunch":                  "forwarding wrapper used only by `gc status`, a separate CLI process",
	"cmd/gc/cmd_session.go|(*attachmentCachingProvider).Relaunch|Relaunch":           "forwarding wrapper built only by `gc session list`'s fallback (doSessionListFallback), a separate CLI process",
	"cmd/gc/session_lifecycle_parallel.go|startPreparedStartCandidate|StartResolved": "worker-handle start; reached only through runPreparedStartCandidate, whose every caller is runFencedPreparedStartCandidate (bracketed); session handles also land in Manager.startRuntime (hooked)",
	"internal/worker/runtime_handle.go|(*RuntimeHandle).StartResolved|Start":         "runtime-only worker handle (no session bead, so no Manager); in-process it is started only by startPreparedStartCandidate, which runFencedPreparedStartCandidate brackets; the API server's wakes use session-backed handles",
}

// sessionStartFenceBrackets are the calls that open a session-start fence
// bracket. A runtime-creating call is covered when one of them appears
// earlier in the same innermost function.
var sessionStartFenceBrackets = map[string]bool{
	"beginStart":           true, // cmd/gc sessionStartFence
	"BeginStart":           true,
	"bracketStart":         true, // internal/session Manager.startRuntime
	"BracketProviderStart": true,
}

// TestSessionStartFenceRatchet keeps every in-process runtime creation inside
// the worktree reaper's session-start fence (ga-yuiof4 item 3). It scans the
// non-test Go files of the packages that create runtimes in the controller
// process for provider-shaped calls — Start/Relaunch/StartResolved with three
// arguments (ctx, name-or-command, runtime config), the runtime.Provider,
// runtime.RelaunchProvider and worker-handle signatures — and fails on any that
// is neither bracketed nor allowlisted with a reason.
//
// One-argument handle.Start(ctx) calls are not scanned: a worker handle's
// Start always ends in one of the scanned three-argument sites (a session
// handle in Manager.startRuntime, a runtime handle in RuntimeHandle.StartResolved).
func TestSessionStartFenceRatchet(t *testing.T) {
	root := filepath.Join("..", "..")
	var findings []string
	seen := map[string]bool{}
	for _, dir := range []string{"cmd/gc", "internal/session", "internal/api", "internal/worker"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel := dir + "/" + name
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				scanFenceFunc(fset, rel, funcDeclName(fd), fd.Body, &findings, seen)
			}
		}
	}
	var stale []string
	for key := range sessionStartFenceAllowlist {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(findings)
	sort.Strings(stale)
	if len(findings) > 0 {
		t.Errorf("runtime-creating call(s) outside every session-start fence bracket (ga-yuiof4 item 3). Bracket the call (sessionStartFence.beginStart/endStart, session.BracketProviderStart, or route it through a Manager), or allowlist it in sessionStartFenceAllowlist with a checkable reason:\n  %s", strings.Join(findings, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("sessionStartFenceAllowlist entries no longer present in the tree; remove them:\n  %s", strings.Join(stale, "\n  "))
	}
}

// scanFenceFunc checks one function body; nested function literals are
// scanned as their own innermost functions.
func scanFenceFunc(fset *token.FileSet, rel, fn string, body *ast.BlockStmt, findings *[]string, seen map[string]bool) {
	var brackets []token.Pos
	var calls []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			scanFenceFunc(fset, rel, fn+"/func", x.Body, findings, seen)
			return false
		case *ast.CallExpr:
			if sessionStartFenceBrackets[calleeName(x.Fun)] {
				brackets = append(brackets, x.Pos())
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && len(x.Args) == 3 {
				switch sel.Sel.Name {
				case "Start", "Relaunch", "StartResolved":
					calls = append(calls, x)
				}
			}
		}
		return true
	})
	for _, call := range calls {
		method := call.Fun.(*ast.SelectorExpr).Sel.Name
		covered := false
		for _, b := range brackets {
			if b < call.Pos() {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		key := rel + "|" + strings.TrimSuffix(fn, "/func") + "|" + method
		if _, ok := sessionStartFenceAllowlist[key]; ok {
			seen[key] = true
			continue
		}
		*findings = append(*findings, fmt.Sprintf("%s: %s in %s", fset.Position(call.Pos()), method, fn))
	}
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.CallExpr:
		return calleeName(f.Fun)
	}
	return ""
}

func funcDeclName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	star := ""
	if s, ok := recv.(*ast.StarExpr); ok {
		star = "*"
		recv = s.X
	}
	if idx, ok := recv.(*ast.IndexExpr); ok {
		recv = idx.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return "(" + star + id.Name + ")." + fd.Name.Name
	}
	return fd.Name.Name
}
