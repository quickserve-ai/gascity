package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/promptmeta"
)

const (
	canonicalPromptTemplateSuffix = ".template.md"
	legacyPromptTemplateSuffix    = ".md.tmpl"
)

// PromptContext holds template data for prompt rendering.
type PromptContext struct {
	CityRoot                string
	AgentName               string // qualified: "rig/polecat-1" or "mayor"
	TemplateName            string // config name: "polecat" (template) or "mayor" (named backing template)
	BindingName             string
	BindingPrefix           string
	RigName                 string
	RigRoot                 string
	WorkDir                 string
	IssuePrefix             string
	Branch                  string
	DefaultBranch           string // e.g. "main" — from git symbolic-ref origin/HEAD
	WorkQuery               string // command to find available work (from Agent.EffectiveWorkQuery)
	AssignedInProgressQuery string // command to find assigned in-progress work (from Agent.EffectiveAssignedInProgressQuery)
	AssignedReadyQuery      string // command to find pre-assigned ready work (from Agent.EffectiveAssignedReadyQuery)
	RoutedPoolQuery         string // command to find unassigned routed pool work (from Agent.EffectiveRoutedPoolQuery)
	SlingQuery              string // command template to route work to this agent (from Agent.EffectiveSlingQuery)
	// ProviderKey is the resolved provider name for this agent (e.g. "claude",
	// "codex", or a custom provider name from the city's [providers] section).
	// Templates can branch on this via {{ .ProviderKey }} or feed it to
	// {{ templateFirst }} for per-provider fragment selection.
	ProviderKey string
	// ProviderDisplayName is the human-readable name for the resolved provider
	// (e.g. "Claude Code", "Codex CLI"). Resolved from city providers, then
	// builtins, then the builtin family of a custom provider; falls back to
	// ProviderKey when nothing else matches.
	ProviderDisplayName string
	// InstructionsFile is the filename the resolved provider reads for project
	// instructions (e.g. "CLAUDE.md" for claude, "AGENTS.md" for codex/kiro).
	// Resolved from city providers, then builtins, then the builtin family of a
	// custom provider; defaults to "AGENTS.md" when no provider is configured.
	// Templates use {{ .InstructionsFile }} as a provider-aware fallback when
	// pack-specific guidance (e.g. quality-gate commands) is missing or empty.
	InstructionsFile string
	Env              map[string]string // from Agent.Env — custom vars
}

// PromptRenderResult holds the rendered text plus the version and rendered
// content SHA introduced by issue #1256 (1e).
//
// Version comes from the template's `version` frontmatter field — a human
// label that surfaces in dashboards and `gc analyze` output. SHA is the
// SHA-256 of the rendered text (after text/template substitution); two
// runs with the same Version but diverging SHAs reveal an unbumped
// template edit.
type PromptRenderResult struct {
	Text    string
	Version string
	SHA     string
	// MissingFragments lists configured fragment names that resolved to no
	// registered template. The render itself is best-effort (missing
	// fragments are skipped with a stderr warning); strict callers use this
	// to fail loud instead.
	MissingFragments []string
}

// renderPrompt reads a prompt template file and renders it with the given
// context. cityName is used internally by template functions (e.g. session)
// but not exposed as a template variable. sessionTemplate is the custom
// session naming template (empty = default). packDirs are the ordered
// pack directories; each may contain prompts/shared/ subdirectories
// loaded as cross-pack shared templates (lower priority than the
// sibling shared/ dir). injectFragments are named templates to append to
// the output after rendering. Returns empty string if templatePath is empty
// or the file doesn't exist. On parse or execute error, logs a warning to
// stderr and returns the raw text (graceful fallback).
func renderPrompt(fs fsys.FS, cityPath, cityName, templatePath string, ctx PromptContext, sessionTemplate string, stderr io.Writer, packDirs []string, injectFragments []string, store beads.Store) string {
	return renderPromptWithMeta(fs, cityPath, cityName, templatePath, ctx, sessionTemplate, stderr, packDirs, injectFragments, store).Text
}

// renderPromptWithMeta is renderPrompt's variant that additionally returns
// the template's frontmatter version and the SHA of the rendered output.
// Callers persisting prompt provenance (session metadata, WorkerOperation
// payloads) should use this entry point.
func renderPromptWithMeta(fs fsys.FS, cityPath, cityName, templatePath string, ctx PromptContext, sessionTemplate string, stderr io.Writer, packDirs []string, injectFragments []string, store beads.Store) PromptRenderResult {
	if templatePath == "" {
		return PromptRenderResult{}
	}
	sourcePath := promptTemplateSourcePath(cityPath, templatePath)
	data, err := fs.ReadFile(sourcePath)
	if err != nil {
		return PromptRenderResult{}
	}
	raw := string(data)
	fm, body := promptmeta.Parse(raw)

	// Canonical prompt templates use .template.md. Legacy .md.tmpl files
	// remain supported temporarily for compatibility; plain .md files skip
	// template execution but still strip frontmatter before hashing/returning.
	if !isPromptTemplatePath(templatePath) {
		return PromptRenderResult{
			Text:    body,
			Version: fm.Version,
			SHA:     promptmeta.SHA(body),
		}
	}

	// templateFirst (registered via promptFuncMap) needs to call tmpl.Lookup
	// at execute time. The closure captures &tmpl by reference so the func
	// observes the parsed template (with all fragments registered) rather
	// than nil at funcmap-construction time.
	var tmpl *template.Template
	tmpl = template.New("prompt").
		Funcs(promptFuncMap(cityName, sessionTemplate, store, func() *template.Template { return tmpl })).
		Option("missingkey=zero")

	loadPromptTemplateSources(fs, tmpl, cityPath, sourcePath, packDirs, stderr)

	// Parse main template last — its body becomes the "prompt" template.
	// Frontmatter is stripped before parsing so it doesn't appear in
	// rendered output.
	tmpl, err = tmpl.Parse(body)
	if err != nil {
		fmt.Fprintf(stderr, "gc: prompt template %q: %v\n", templatePath, err) //nolint:errcheck // best-effort stderr
		return PromptRenderResult{
			Text:    body,
			Version: fm.Version,
			SHA:     promptmeta.SHA(body),
		}
	}

	td := buildTemplateData(ctx)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, td); err != nil {
		fmt.Fprintf(stderr, "gc: prompt template %q: %v\n", templatePath, err) //nolint:errcheck // best-effort stderr
		return PromptRenderResult{
			Text:    body,
			Version: fm.Version,
			SHA:     promptmeta.SHA(body),
		}
	}

	// Append injected fragments.
	var missingFragments []string
	for _, name := range injectFragments {
		frag := tmpl.Lookup(name)
		if frag == nil {
			fmt.Fprintf(stderr, "gc: inject_fragment %q: template not found\n", name) //nolint:errcheck // best-effort stderr
			missingFragments = append(missingFragments, name)
			continue
		}
		var fbuf bytes.Buffer
		if err := frag.Execute(&fbuf, td); err != nil {
			fmt.Fprintf(stderr, "gc: inject_fragment %q: %v\n", name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		buf.WriteString("\n\n")
		buf.Write(fbuf.Bytes())
	}

	rendered := buf.String()
	return PromptRenderResult{
		Text:             rendered,
		Version:          fm.Version,
		SHA:              promptmeta.SHA(rendered),
		MissingFragments: missingFragments,
	}
}

// loadPromptTemplateSources loads every shared-template and fragment source
// that renderPromptWithMeta makes available to the prompt at sourcePath, in
// ascending precedence order: imported pack dirs, the source prompt's own
// pack root, the city root, the sibling shared/ dir, and the per-agent
// template-fragments/ dir. Pack-level sources also register each fragment
// under a "<pack-name>/<name>" qualified alias (pack.toml name, directory
// base name as fallback) so configs can disambiguate fragments with the
// same name across packs.
func loadPromptTemplateSources(fs fsys.FS, tmpl *template.Template, cityPath, sourcePath string, packDirs []string, stderr io.Writer) {
	// Load shared templates from pack dirs (lower priority).
	// Each pack directory may contain prompts/shared/ and/or
	// template-fragments/ subdirectories.
	loadedPackFragmentRoots := make(map[string]struct{}, len(packDirs)+1)
	for _, dir := range packDirs {
		cleanDir := filepath.Clean(dir)
		loadedPackFragmentRoots[cleanDir] = struct{}{}
		ns := promptPackNamespace(fs, cleanDir)
		sharedDir := filepath.Join(dir, "prompts", "shared")
		loadSharedTemplates(fs, tmpl, sharedDir, ns, stderr)
		// V2: template-fragments/ at pack level.
		fragDir := filepath.Join(dir, "template-fragments")
		loadSharedTemplates(fs, tmpl, fragDir, ns, stderr)
	}
	if sourcePackRoot := promptSourcePackRoot(cityPath, sourcePath); sourcePackRoot != "" {
		if _, ok := loadedPackFragmentRoots[sourcePackRoot]; !ok {
			ns := promptPackNamespace(fs, sourcePackRoot)
			loadSharedTemplates(fs, tmpl, filepath.Join(sourcePackRoot, "prompts", "shared"), ns, stderr)
			loadSharedTemplates(fs, tmpl, filepath.Join(sourcePackRoot, "template-fragments"), ns, stderr)
		}
	}

	// Load shared templates from the city root itself. cfg.PackDirs is
	// populated only from imported packs, so a root city pack with no
	// [imports.*] blocks would otherwise silently ignore its own
	// prompts/shared/ and template-fragments/ directories. Loaded after
	// imported-pack fragments (so city-root wins on name collision with
	// imports) but before sibling shared/ and per-agent fragments below
	// (which keep their existing higher precedence).
	loadSharedTemplates(fs, tmpl, filepath.Join(cityPath, "prompts", "shared"), "", stderr)
	loadSharedTemplates(fs, tmpl, filepath.Join(cityPath, "template-fragments"), "", stderr)

	// Load shared templates from sibling shared/ directory (highest priority —
	// wins on name collision with cross-pack templates).
	sharedDir := filepath.Join(filepath.Dir(sourcePath), "shared")
	loadSharedTemplates(fs, tmpl, sharedDir, "", stderr)

	// V2: per-agent template-fragments/ (if the prompt lives in agents/<name>/).
	// Load from agents/<name>/template-fragments/ so per-agent fragments
	// are available alongside pack-level ones.
	agentFragDir := filepath.Join(filepath.Dir(sourcePath), "template-fragments")
	loadSharedTemplates(fs, tmpl, agentFragDir, "", stderr)
}

// buildPromptTemplateSet constructs the full template set for templatePath —
// the same construction renderPromptWithMeta uses (all shared/fragment
// sources plus the parsed main body as "prompt") — without executing
// anything. Returns nil when the prompt is absent, unreadable, not a
// template, or fails to parse: those states have their own checks.
func buildPromptTemplateSet(fs fsys.FS, cityPath, cityName, templatePath, sessionTemplate string, packDirs []string) *template.Template {
	if templatePath == "" || !isPromptTemplatePath(templatePath) {
		return nil
	}
	sourcePath := promptTemplateSourcePath(cityPath, templatePath)
	data, err := fs.ReadFile(sourcePath)
	if err != nil {
		return nil
	}
	_, body := promptmeta.Parse(string(data))
	var tmpl *template.Template
	tmpl = template.New("prompt").
		Funcs(promptFuncMap(cityName, sessionTemplate, nil, func() *template.Template { return tmpl })).
		Option("missingkey=zero")
	loadPromptTemplateSources(fs, tmpl, cityPath, sourcePath, packDirs, io.Discard)
	parsed, err := tmpl.Parse(body)
	if err != nil {
		return nil
	}
	return parsed
}

// unresolvedPromptFragments reports which of the configured fragment names
// would fail to resolve when rendering templatePath. Used by strict callers
// (gc prime --strict) to fail loud before render.
func unresolvedPromptFragments(fs fsys.FS, cityPath, cityName, templatePath, sessionTemplate string, packDirs, fragments []string) []string {
	if len(fragments) == 0 {
		return nil
	}
	tmpl := buildPromptTemplateSet(fs, cityPath, cityName, templatePath, sessionTemplate, packDirs)
	if tmpl == nil {
		return nil
	}
	var missing []string
	for _, name := range fragments {
		if tmpl.Lookup(name) == nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// promptTemplateUnknownVariables statically checks templatePath's body and
// its configured fragments for dot-rooted data references outside known
// (the buildTemplateData key set for the agent). Unresolvable fragment
// names are skipped — unresolvedPromptFragments owns that report.
func promptTemplateUnknownVariables(fs fsys.FS, cityPath, cityName, templatePath, sessionTemplate string, packDirs, fragments []string, known map[string]struct{}) []templateVarIssue {
	tmpl := buildPromptTemplateSet(fs, cityPath, cityName, templatePath, sessionTemplate, packDirs)
	if tmpl == nil {
		return nil
	}
	roots := make([]string, 0, len(fragments)+1)
	roots = append(roots, "prompt")
	for _, name := range fragments {
		if tmpl.Lookup(name) != nil {
			roots = append(roots, name)
		}
	}
	return unknownTemplateVariables(tmpl, roots, known)
}

func promptTemplateSourcePath(cityPath, templatePath string) string {
	if filepath.IsAbs(templatePath) {
		return templatePath
	}
	return filepath.Join(cityPath, templatePath)
}

func promptSourcePackRoot(cityPath, sourcePath string) string {
	cleanCityPath := filepath.Clean(cityPath)
	cleanSourcePath := filepath.Clean(sourcePath)
	agentDir := filepath.Dir(cleanSourcePath)
	agentsDir := filepath.Dir(agentDir)
	if filepath.Base(agentsDir) != "agents" {
		return ""
	}
	packRoot := filepath.Clean(filepath.Dir(agentsDir))
	if packRoot == cleanCityPath {
		return ""
	}
	return packRoot
}

func isCanonicalPromptTemplatePath(path string) bool {
	return strings.HasSuffix(path, canonicalPromptTemplateSuffix)
}

func isLegacyPromptTemplatePath(path string) bool {
	return strings.HasSuffix(path, legacyPromptTemplateSuffix)
}

func isPromptTemplatePath(path string) bool {
	return isCanonicalPromptTemplatePath(path) || isLegacyPromptTemplatePath(path)
}

func sharedTemplateFileNames(entries []os.DirEntry) []string {
	legacy := make([]string, 0, len(entries))
	canonical := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch name := e.Name(); {
		case isLegacyPromptTemplatePath(name):
			legacy = append(legacy, name)
		case isCanonicalPromptTemplatePath(name):
			canonical = append(canonical, name)
		}
	}
	sort.Strings(legacy)
	sort.Strings(canonical)
	return append(legacy, canonical...)
}

// loadSharedTemplates loads supported prompt-template files from a shared
// directory into the given template. Canonical .template.md files override
// legacy .md.tmpl files with the same definitions. namespace, when non-empty,
// additionally registers each template under "<namespace>/<name>".
func loadSharedTemplates(fs fsys.FS, tmpl *template.Template, dir, namespace string, stderr io.Writer) {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, name := range sharedTemplateFileNames(entries) {
		if sdata, err := fs.ReadFile(filepath.Join(dir, name)); err == nil {
			if err := registerSharedTemplateContent(tmpl, name, string(sdata), namespace); err != nil {
				fmt.Fprintf(stderr, "gc: shared template %q: %v\n", name, err) //nolint:errcheck // best-effort stderr
			}
		}
	}
}

// registerSharedTemplateContent registers one shared-template file's content
// into tmpl. Files built from {{define}} blocks register those names as-is
// (the historical contract). Files with no {{define}} blocks are raw
// fragments: the whole body (frontmatter stripped) registers under the
// file's base name, so `template-fragments/foo.template.md` is referenceable
// as fragment "foo" without boilerplate. Either style also registers under
// "<namespace>/<name>" when namespace is non-empty, letting configs
// disambiguate same-named fragments across packs.
func registerSharedTemplateContent(tmpl *template.Template, fileName, content, namespace string) error {
	defined, err := templateDefinedNames(content)
	if err != nil {
		return err
	}
	if len(defined) == 0 {
		base := promptTemplateBaseName(fileName)
		_, body := promptmeta.Parse(content)
		if _, err := tmpl.New(base).Parse(body); err != nil {
			return err
		}
		if namespace != "" {
			if _, err := tmpl.New(namespace + "/" + base).Parse(body); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := tmpl.Parse(content); err != nil {
		return err
	}
	if namespace != "" {
		for _, dn := range defined {
			t := tmpl.Lookup(dn)
			if t == nil || t.Tree == nil {
				continue
			}
			if _, err := tmpl.AddParseTree(namespace+"/"+dn, t.Tree); err != nil {
				return err
			}
		}
	}
	return nil
}

// templateDefinedNames probe-parses content and returns the names of the
// {{define}}/{{block}} templates it declares, without touching any live
// template set.
func templateDefinedNames(content string) ([]string, error) {
	probe := template.New("__gc_probe__").
		Funcs(promptFuncMap("", "", nil, func() *template.Template { return nil }))
	parsed, err := probe.Parse(content)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range parsed.Templates() {
		if t.Name() != "__gc_probe__" {
			names = append(names, t.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// promptPackNamespace returns the fragment namespace for a pack directory:
// [pack].name from its pack.toml, falling back to the directory base name
// (which matches the pack name for local packs and registry subpacks).
func promptPackNamespace(fs fsys.FS, dir string) string {
	fallback := filepath.Base(filepath.Clean(dir))
	data, err := fs.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		return fallback
	}
	var pc struct {
		Pack struct {
			Name string `toml:"name"`
		} `toml:"pack"`
	}
	if _, err := toml.Decode(string(data), &pc); err != nil || pc.Pack.Name == "" {
		return fallback
	}
	return pc.Pack.Name
}

// promptTemplateBaseName strips the canonical or legacy prompt-template
// suffix from a file name, yielding the fragment name a raw fragment file
// registers under.
func promptTemplateBaseName(fileName string) string {
	if strings.HasSuffix(fileName, canonicalPromptTemplateSuffix) {
		return strings.TrimSuffix(fileName, canonicalPromptTemplateSuffix)
	}
	if strings.HasSuffix(fileName, legacyPromptTemplateSuffix) {
		return strings.TrimSuffix(fileName, legacyPromptTemplateSuffix)
	}
	return fileName
}

// mergeFragmentLists combines global and per-agent fragment lists.
func mergeFragmentLists(global, perAgent []string) []string {
	if len(global) == 0 && len(perAgent) == 0 {
		return nil
	}
	merged := make([]string, 0, len(global)+len(perAgent))
	seen := make(map[string]struct{}, len(global)+len(perAgent))
	merged = append(merged, global...)
	for _, name := range global {
		seen[name] = struct{}{}
	}
	for _, name := range perAgent {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	return merged
}

// effectivePromptFragments applies the runtime fragment layering contract.
func effectivePromptFragments(global, inject, appendFragments, inherited, defaults []string) []string {
	fragments := mergeFragmentLists(global, inject)
	fragments = mergeFragmentLists(fragments, appendFragments)
	fragments = mergeFragmentLists(fragments, inherited)
	return mergeFragmentLists(fragments, defaults)
}

// buildTemplateData merges Env (lower priority) with SDK fields (higher
// priority) into a single map for template execution.
func buildTemplateData(ctx PromptContext) map[string]string {
	m := make(map[string]string, len(ctx.Env)+19)
	for k, v := range ctx.Env {
		m[k] = v
	}
	// SDK fields override Env.
	m["CityRoot"] = ctx.CityRoot
	m["AgentName"] = ctx.AgentName
	m["TemplateName"] = ctx.TemplateName
	m["BindingName"] = ctx.BindingName
	m["BindingPrefix"] = ctx.BindingPrefix
	m["RigName"] = ctx.RigName
	m["RigRoot"] = ctx.RigRoot
	m["WorkDir"] = ctx.WorkDir
	m["IssuePrefix"] = ctx.IssuePrefix
	m["Branch"] = ctx.Branch
	m["DefaultBranch"] = ctx.DefaultBranch
	m["WorkQuery"] = ctx.WorkQuery
	m["AssignedInProgressQuery"] = ctx.AssignedInProgressQuery
	m["AssignedReadyQuery"] = ctx.AssignedReadyQuery
	m["RoutedPoolQuery"] = ctx.RoutedPoolQuery
	m["SlingQuery"] = ctx.SlingQuery
	m["ProviderKey"] = ctx.ProviderKey
	m["ProviderDisplayName"] = ctx.ProviderDisplayName
	m["InstructionsFile"] = ctx.InstructionsFile
	return m
}

// templateVarIssue reports a template referencing a top-level data key
// outside the known set (SDK fields + agent env vars). Because prompt data
// is a map rendered with missingkey=zero, such references silently render
// empty at runtime — the {{ .Rig }} class of defect.
type templateVarIssue struct {
	TemplateName string // named template containing the reference
	Field        string // the unknown top-level key, e.g. "Rig"
	Location     string // text/template ErrorContext, "template:line:col"
}

// unknownTemplateVariables statically walks the parse trees of the named
// root templates, following {{template}} calls transitively, and returns
// dot-rooted field references ({{ .Foo }}, {{ $.Foo }}) whose top-level key
// is not in known. Bodies of {{range}}/{{with}} re-bind dot and are walked
// without dot-rooted checking (their pipelines are still checked), so
// element-relative fields never false-positive. One issue per
// (template, field) pair.
func unknownTemplateVariables(tmpl *template.Template, roots []string, known map[string]struct{}) []templateVarIssue {
	type queued struct {
		name string
		// dotRooted: dot at template entry is the top-level data map.
		dotRooted bool
	}
	seen := make(map[string]bool) // name -> already walked with dotRooted=true
	reported := make(map[string]struct{})
	var out []templateVarIssue

	queue := make([]queued, 0, len(roots))
	for _, r := range roots {
		queue = append(queue, queued{name: r, dotRooted: true})
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if walked, ok := seen[item.name]; ok && (walked || !item.dotRooted) {
			continue
		}
		seen[item.name] = item.dotRooted
		t := tmpl.Lookup(item.name)
		if t == nil || t.Tree == nil || t.Tree.Root == nil {
			continue
		}
		tree := t.Tree

		report := func(field string, node parse.Node) {
			key := item.name + "\x00" + field
			if _, dup := reported[key]; dup {
				return
			}
			reported[key] = struct{}{}
			location, _ := tree.ErrorContext(node)
			out = append(out, templateVarIssue{TemplateName: item.name, Field: field, Location: location})
		}

		var walkNode func(node parse.Node, dotIsRoot bool)
		var walkPipe func(pipe *parse.PipeNode, dotIsRoot bool)

		walkArg := func(arg parse.Node, dotIsRoot bool) {
			switch n := arg.(type) {
			case *parse.FieldNode:
				if dotIsRoot && len(n.Ident) > 0 {
					if _, ok := known[n.Ident[0]]; !ok {
						report(n.Ident[0], n)
					}
				}
			case *parse.VariableNode:
				// $ is bound to the template's entry data for the whole
				// body, independent of range/with dot re-binding.
				if item.dotRooted && len(n.Ident) > 1 && n.Ident[0] == "$" {
					if _, ok := known[n.Ident[1]]; !ok {
						report(n.Ident[1], n)
					}
				}
			case *parse.PipeNode:
				walkPipe(n, dotIsRoot)
			case *parse.ChainNode:
				if sub, ok := n.Node.(*parse.PipeNode); ok {
					walkPipe(sub, dotIsRoot)
				}
			}
		}

		walkPipe = func(pipe *parse.PipeNode, dotIsRoot bool) {
			if pipe == nil {
				return
			}
			for _, cmd := range pipe.Cmds {
				for _, arg := range cmd.Args {
					walkArg(arg, dotIsRoot)
				}
			}
		}

		pipeIsDot := func(pipe *parse.PipeNode) bool {
			if pipe == nil || len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
				return false
			}
			_, isDot := pipe.Cmds[0].Args[0].(*parse.DotNode)
			return isDot
		}

		walkNode = func(node parse.Node, dotIsRoot bool) {
			switch n := node.(type) {
			case *parse.ListNode:
				if n == nil {
					return
				}
				for _, child := range n.Nodes {
					walkNode(child, dotIsRoot)
				}
			case *parse.ActionNode:
				walkPipe(n.Pipe, dotIsRoot)
			case *parse.IfNode:
				walkPipe(n.Pipe, dotIsRoot)
				walkNode(n.List, dotIsRoot)
				if n.ElseList != nil {
					walkNode(n.ElseList, dotIsRoot)
				}
			case *parse.RangeNode:
				walkPipe(n.Pipe, dotIsRoot)
				walkNode(n.List, false)
				if n.ElseList != nil {
					walkNode(n.ElseList, dotIsRoot)
				}
			case *parse.WithNode:
				walkPipe(n.Pipe, dotIsRoot)
				walkNode(n.List, false)
				if n.ElseList != nil {
					walkNode(n.ElseList, dotIsRoot)
				}
			case *parse.TemplateNode:
				walkPipe(n.Pipe, dotIsRoot)
				queue = append(queue, queued{
					name:      n.Name,
					dotRooted: dotIsRoot && pipeIsDot(n.Pipe),
				})
			}
		}

		walkNode(tree.Root, item.dotRooted)
	}
	return out
}

// findRigPrefix returns the effective bead ID prefix for the named rig.
// Returns empty string if rigName is empty or not found.
func findRigPrefix(rigName string, rigs []config.Rig) string {
	for i := range rigs {
		if rigs[i].Name == rigName {
			return rigs[i].EffectivePrefix()
		}
	}
	return ""
}

// defaultBranchFor returns the default branch for the repo at dir.
// Returns "main" on any error (best-effort).
func defaultBranchFor(dir string) string {
	if dir == "" {
		return "main"
	}
	g := git.New(dir)
	branch, _ := g.DefaultBranch()
	return branch
}

// defaultBranchForRig returns the rig's recorded DefaultBranch when set,
// falling back to a runtime probe of dir. Use this in prompt/template
// rendering so polecats and the refinery target the rig's true mainline
// even when origin/HEAD is unset on the local clone.
func defaultBranchForRig(rigName string, rigs []config.Rig, dir string) string {
	if rigName != "" {
		for i := range rigs {
			if rigs[i].Name == rigName {
				if branch := rigs[i].EffectiveDefaultBranch(); branch != "" {
					return branch
				}
				break
			}
		}
	}
	return defaultBranchFor(dir)
}

// promptFuncMap returns template functions available in prompt templates.
// sessionTemplate is the custom session naming template (empty = default).
// store is used by the "session" function to look up bead-derived session
// names; nil falls back to legacy naming. parentTmpl is a getter for the
// template being rendered; it is invoked at execute time (not at funcmap
// construction time) so functions can look up fragments parsed after the
// funcmap was wired.
func promptFuncMap(cityName, sessionTemplate string, store beads.Store, parentTmpl func() *template.Template) template.FuncMap {
	return template.FuncMap{
		"cmd": func() string {
			return filepath.Base(os.Args[0])
		},
		"session": func(agentName string) string {
			return lookupSessionNameOrLegacy(store, cityName, agentName, sessionTemplate)
		},
		"basename": func(qualifiedName string) string {
			_, name := config.ParseQualifiedName(qualifiedName)
			return name
		},
		// templateFirst executes the first registered template fragment whose
		// name matches one of the provided candidates, using `data` as the
		// template context. Returns "" when no candidate is registered (silent
		// fallback — pass a guaranteed-present "default" name last to enforce
		// a match). Empty candidate names are skipped.
		//
		// Typical use:
		//   {{ templateFirst . (printf "slash-note-%s" .ProviderKey) "slash-note-default" }}
		"templateFirst": func(data any, names ...string) (string, error) {
			t := parentTmpl()
			if t == nil {
				return "", nil
			}
			for _, name := range names {
				if name == "" {
					continue
				}
				frag := t.Lookup(name)
				if frag == nil {
					continue
				}
				var buf bytes.Buffer
				if err := frag.Execute(&buf, data); err != nil {
					return "", err
				}
				return buf.String(), nil
			}
			return "", nil
		},
	}
}

// providerInfoForAgent returns the resolved provider key and human-readable
// display name for an agent, without performing PATH lookups (which the full
// config.ResolveProvider performs and which are inappropriate for prompt
// rendering). Resolution chain: agent.Provider > workspace.Provider. Returns
// empty strings when no provider name is configured.
func providerInfoForAgent(a *config.Agent, ws *config.Workspace, cityProviders map[string]config.ProviderSpec) (key, displayName string) {
	if a == nil {
		return "", ""
	}
	name := a.Provider
	if name == "" && ws != nil {
		name = ws.Provider
	}
	if name == "" {
		return "", ""
	}
	return name, providerDisplayNameFor(name, cityProviders)
}

// instructionsFileForAgent returns the project-instructions filename the
// resolved provider expects (e.g. "CLAUDE.md", "AGENTS.md"). It mirrors the
// resolution chain used by providerInfoForAgent (agent.Provider >
// workspace.Provider) and looks the filename up via the same precedence as
// config.ResolveProvider (city providers > builtin spec > builtin family).
// Returns "AGENTS.md" — the same default config.resolveProvider uses — when no
// provider is configured or the resolved spec leaves InstructionsFile empty.
func instructionsFileForAgent(a *config.Agent, ws *config.Workspace, cityProviders map[string]config.ProviderSpec) string {
	const defaultInstructionsFile = "AGENTS.md"
	if a == nil {
		return defaultInstructionsFile
	}
	name := a.Provider
	if name == "" && ws != nil {
		name = ws.Provider
	}
	if name == "" {
		return defaultInstructionsFile
	}
	if spec, ok := cityProviders[name]; ok && spec.InstructionsFile != "" {
		return spec.InstructionsFile
	}
	if spec, ok := config.BuiltinProviders()[name]; ok && spec.InstructionsFile != "" {
		return spec.InstructionsFile
	}
	if family := config.BuiltinFamily(name, cityProviders); family != "" && family != name {
		if spec, ok := config.BuiltinProviders()[family]; ok && spec.InstructionsFile != "" {
			return spec.InstructionsFile
		}
	}
	return defaultInstructionsFile
}

// providerDisplayNameFor returns the human-readable name for a provider.
// Resolution: city providers (explicit DisplayName) > builtin spec for the
// raw name > builtin spec for the BuiltinFamily ancestor > the name itself.
func providerDisplayNameFor(name string, cityProviders map[string]config.ProviderSpec) string {
	if name == "" {
		return ""
	}
	if spec, ok := cityProviders[name]; ok && spec.DisplayName != "" {
		return spec.DisplayName
	}
	if spec, ok := config.BuiltinProviders()[name]; ok && spec.DisplayName != "" {
		return spec.DisplayName
	}
	if family := config.BuiltinFamily(name, cityProviders); family != "" && family != name {
		if spec, ok := config.BuiltinProviders()[family]; ok && spec.DisplayName != "" {
			return spec.DisplayName
		}
	}
	return name
}
