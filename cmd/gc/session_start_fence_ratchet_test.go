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
			scanFenceFile(fset, rel, file, &findings, seen)
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

// runtimeCreatingMethods are the method names of the runtime-creating calls:
// runtime.Provider.Start, runtime.RelaunchProvider.Relaunch and the worker
// handle's StartResolved.
var runtimeCreatingMethods = map[string]bool{"Start": true, "Relaunch": true, "StartResolved": true}

// isRuntimeCreatingCall reports a provider-shaped call: a runtime-creating
// method with three arguments (ctx, name-or-command, runtime config).
func isRuntimeCreatingCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && runtimeCreatingMethods[sel.Sel.Name] && len(call.Args) == 3
}

// scanFenceFile checks every function declared in one parsed file.
func scanFenceFile(fset *token.FileSet, rel string, file *ast.File, findings *[]string, seen map[string]bool) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		scanFenceFunc(fset, rel, funcDeclName(fd), fd.Body, findings, seen)
	}
}

// scanFenceFunc checks one function body; nested function literals are
// scanned as their own innermost functions, so a closure — including one
// launched by `go` — is covered only by a bracket inside its own body.
//
// Three shapes are findings unless allowlisted:
//   - a runtime-creating call with no bracket opened earlier in the same
//     innermost function;
//   - a runtime-creating call that is the direct operand of a `go` statement,
//     bracket or not: the goroutine can outlive the enclosing bracket;
//   - a runtime-creating method named by a selector that is not in call
//     position (a method value such as `start := sp.Start`, or `sp.Start`
//     passed as an argument): whoever calls it later is outside any bracket
//     this scan can see.
func scanFenceFunc(fset *token.FileSet, rel, fn string, body *ast.BlockStmt, findings *[]string, seen map[string]bool) {
	var brackets []token.Pos
	var calls []*ast.CallExpr
	var methodValues []*ast.SelectorExpr
	callFuns := map[*ast.SelectorExpr]bool{}
	goCalls := map[*ast.CallExpr]bool{}
	// ast.Inspect is pre-order: a GoStmt is visited before its call, and a
	// call before its Fun selector, so both maps are filled before the nodes
	// they classify are reached.
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			scanFenceFunc(fset, rel, fn+"/func", x.Body, findings, seen)
			return false
		case *ast.GoStmt:
			if isRuntimeCreatingCall(x.Call) {
				goCalls[x.Call] = true
			}
		case *ast.CallExpr:
			if sessionStartFenceBrackets[calleeName(x.Fun)] {
				brackets = append(brackets, x.Pos())
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				callFuns[sel] = true
			}
			if isRuntimeCreatingCall(x) {
				calls = append(calls, x)
			}
		case *ast.SelectorExpr:
			if runtimeCreatingMethods[x.Sel.Name] && !callFuns[x] {
				methodValues = append(methodValues, x)
			}
		}
		return true
	})
	report := func(pos token.Pos, method, shape string) {
		keyMethod := method
		if shape != "" {
			keyMethod = method + "@" + shape
		}
		key := rel + "|" + strings.TrimSuffix(fn, "/func") + "|" + keyMethod
		if _, ok := sessionStartFenceAllowlist[key]; ok {
			seen[key] = true
			return
		}
		what := method
		if shape != "" {
			what = method + " (" + shape + ")"
		}
		*findings = append(*findings, fmt.Sprintf("%s: %s in %s", fset.Position(pos), what, fn))
	}
	for _, call := range calls {
		method := call.Fun.(*ast.SelectorExpr).Sel.Name
		if goCalls[call] {
			report(call.Pos(), method, "go")
			continue
		}
		covered := false
		for _, b := range brackets {
			if b < call.Pos() {
				covered = true
				break
			}
		}
		if !covered {
			report(call.Pos(), method, "")
		}
	}
	for _, sel := range methodValues {
		report(sel.Pos(), sel.Sel.Name, "method value")
	}
}

// TestSessionStartFenceRatchet_RejectsEscapingShapes feeds the scanner the two
// shapes Astra r4 found it missed, plus controls it must accept.
func TestSessionStartFenceRatchet_RejectsEscapingShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		src       string
		wantFlags int
	}{
		{
			name:      "method value assigned then called",
			src:       "func f(sp P) error { defer bracketStart(nil, sp)(); start := sp.Start; return start(ctx, \"n\", cfg) }",
			wantFlags: 1,
		},
		{
			name:      "method value passed as an argument",
			src:       "func f(sp P) { defer bracketStart(nil, sp)(); run(sp.Start) }",
			wantFlags: 1,
		},
		{
			name:      "relaunch method value",
			src:       "func f(r R) { g := r.Relaunch; _ = g }",
			wantFlags: 1,
		},
		{
			name:      "go statement inside a bracketed function",
			src:       "func f(sp P) { defer bracketStart(nil, sp)(); go sp.Start(ctx, \"n\", cfg) }",
			wantFlags: 1,
		},
		{
			name:      "go func literal without its own bracket",
			src:       "func f(sp P) { defer bracketStart(nil, sp)(); go func() { _ = sp.Start(ctx, \"n\", cfg) }() }",
			wantFlags: 1,
		},
		{
			name:      "control: go func literal that brackets itself",
			src:       "func f(sp P) { go func() { defer BracketProviderStart(sp)(); _ = sp.Start(ctx, \"n\", cfg) }() }",
			wantFlags: 0,
		},
		{
			name:      "control: bracketed synchronous call",
			src:       "func f(f *F, sp P) error { f.beginStart(); defer f.endStart(); return sp.Relaunch(ctx, \"n\", cfg) }",
			wantFlags: 0,
		},
		{
			name:      "control: unrelated one-argument Start",
			src:       "func f(p Plane) { p.Start(ctx) }",
			wantFlags: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", "package fixture\n\n"+tc.src+"\n", parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			var findings []string
			scanFenceFile(fset, "fixture/fixture.go", file, &findings, map[string]bool{})
			if len(findings) != tc.wantFlags {
				t.Fatalf("findings = %d %q, want %d", len(findings), findings, tc.wantFlags)
			}
		})
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
