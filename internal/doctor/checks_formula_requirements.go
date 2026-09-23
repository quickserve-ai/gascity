package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/rollout"
)

// FormulaRequirementsCheck reports formula compiler requirement migration and
// compatibility diagnostics across the visible city and rig formula layers.
type FormulaRequirementsCheck struct {
	cfg *config.City
	// rolloutFlags, when set, replaces the rollout gates resolved from cfg;
	// tests inject rollout.ForTest values through it.
	rolloutFlags *rollout.Flags
	// compile compiles a formula by its resolver key the way dispatch does. It
	// settles a disabled_reason whose own requirement is satisfiable:
	// expansions and aspects add their requirements only at compile time.
	// v2Enabled is the city's daemon.formula_v2 rollout gate, not whatever the
	// process last applied.
	compile func(name string, searchPaths []string, v2Enabled bool) error
}

// NewFormulaRequirementsCheck creates a formula requirements doctor check.
func NewFormulaRequirementsCheck(cfg *config.City, _ string) *FormulaRequirementsCheck {
	return &FormulaRequirementsCheck{cfg: cfg, compile: compileFormulaForRequirements}
}

func compileFormulaForRequirements(name string, searchPaths []string, v2Enabled bool) error {
	_, err := formula.CompileWithoutRuntimeVarValidationForHost(context.Background(), name, searchPaths, nil, v2Enabled)
	return err
}

// formulaV2 reads the city's daemon.formula_v2 rollout gate. It resolves a
// view holding only that gate's config, so an invalid sibling gate (a [beads]
// mode typo, which doctor's rollout section reports) cannot withhold it.
func (c *FormulaRequirementsCheck) formulaV2() (bool, error) {
	if c.rolloutFlags != nil {
		return c.rolloutFlags.FormulaV2(), nil
	}
	flags, err := rollout.Resolve(&config.City{Daemon: config.DaemonConfig{FormulaV2: c.cfg.Daemon.FormulaV2}}, rollout.ResolveOptions{})
	if err != nil {
		return false, err
	}
	return flags.FormulaV2(), nil
}

// Name returns the check identifier shown by gc doctor.
func (c *FormulaRequirementsCheck) Name() string { return "formula-requirements" }

// Run checks visible formulas for compiler requirement compatibility issues.
func (c *FormulaRequirementsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "city config unavailable"
		return r
	}

	v2, err := c.formulaV2()
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("resolving the daemon.formula_v2 rollout gate: %v", err)
		return r
	}
	issues, disabled := c.collectIssues(v2)
	slices.Sort(disabled)
	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "formula compiler requirements are consistent"
		if len(disabled) > 0 {
			r.Message = fmt.Sprintf("formula compiler requirements are consistent (%d intentionally disabled)", len(disabled))
			r.Details = disabled
		}
		return r
	}

	slices.SortFunc(issues, func(a, b formulaRequirementIssue) int {
		return strings.Compare(a.detail(), b.detail())
	})
	for _, issue := range issues {
		r.Details = append(r.Details, issue.detail())
	}
	r.Details = append(r.Details, disabled...)
	errors, warnings := countFormulaRequirementIssues(issues)
	switch {
	case errors > 0:
		r.Status = StatusError
		r.Message = fmt.Sprintf("%d formula requirement error(s), %d warning(s)", errors, warnings)
	case warnings > 0:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%d formula requirement warning(s)", warnings)
	}
	r.FixHint = `replace deprecated contract = "graph.v2" with [requires] formula_compiler = ">=2.0.0"; enable [daemon] formula_v2 or lower requirements; fix invalid requirements and parent/child conflicts`
	return r
}

// CanFix reports whether this check supports automatic remediation.
func (c *FormulaRequirementsCheck) CanFix() bool { return false }

// Fix is a no-op because formula requirement migrations need author review.
func (c *FormulaRequirementsCheck) Fix(_ *CheckContext) error { return nil }

// collectIssues returns the findings, plus one note per formula whose
// unsatisfiable requirement is declared deliberate (disabled_reason) — those
// are reported, never counted as defects (ga-2h3isb).
func (c *FormulaRequirementsCheck) collectIssues(v2 bool) ([]formulaRequirementIssue, []string) {
	var issues []formulaRequirementIssue
	var disabled []string
	seenDisabled := make(map[string]struct{})
	seen := make(map[formulaRequirementIssueKey]struct{})
	addIssue := func(issue formulaRequirementIssue) {
		key := issue.dedupeKey()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		issues = append(issues, issue)
	}
	src := formula.SourceFromEnv()
	for _, scope := range c.formulaScopes() {
		parser := formula.NewParser(scope.paths...).SetSource(src)
		winners := formula.ResolveAllWithSource(src, scope.paths)
		names := make([]string, 0, len(winners))
		for name := range winners {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			path := winners[name]
			f, err := parser.ParseFile(path)
			if err != nil {
				addIssue(formulaRequirementIssue{
					severity: StatusError,
					scope:    scope.name,
					formula:  name,
					path:     path,
					message:  fmt.Sprintf("parse formula: %v", err),
				})
				continue
			}
			if strings.EqualFold(strings.TrimSpace(f.Contract), beadmeta.FormulaContractGraphV2) {
				addIssue(formulaRequirementIssue{
					severity: StatusWarning,
					scope:    scope.name,
					formula:  f.Formula,
					path:     path,
					message:  `deprecated contract = "graph.v2"; use [requires] formula_compiler = ">=2.0.0"`,
				})
			}
			resolved, err := parser.Resolve(f)
			if err != nil {
				addIssue(formulaRequirementIssue{
					severity: StatusError,
					scope:    scope.name,
					formula:  f.Formula,
					path:     path,
					message:  fmt.Sprintf("resolve formula: %v", err),
				})
				continue
			}
			reason := ""
			if resolved.Requires != nil {
				reason = strings.TrimSpace(resolved.Requires.DisabledReason)
			}
			hostErr := formula.ValidateHostRequirements(resolved, v2)
			var disabledReasons []string
			intentionallyDisabled := false
			if formula.IsUnsatisfiedRequirement(hostErr) {
				disabledReasons, intentionallyDisabled = coveringDisabledReasons(parser, name, f, v2)
			}
			// A deliberately unsatisfiable requirement also fails the explicit
			// graph-declaration rule (">=999.0.0" does not accept the graph
			// compiler), so that derivative error is not reported for it.
			if !intentionallyDisabled {
				if err := formula.ValidateExplicitGraphCompilerRequirement(resolved); err != nil {
					addIssue(formulaRequirementIssue{
						severity: StatusError,
						scope:    scope.name,
						formula:  resolved.Formula,
						path:     path,
						message:  err.Error(),
					})
				}
			}
			switch {
			case intentionallyDisabled:
				note := fmt.Sprintf("intentionally disabled %s formula %q (%s): %s", scope.name, resolved.Formula, path, strings.Join(disabledReasons, "; "))
				if _, ok := seenDisabled[note]; !ok {
					seenDisabled[note] = struct{}{}
					disabled = append(disabled, note)
				}
			case hostErr != nil:
				addIssue(formulaRequirementIssue{
					severity: StatusError,
					scope:    scope.name,
					formula:  resolved.Formula,
					path:     path,
					message:  hostErr.Error(),
				})
			case reason != "":
				// Its own requirement is satisfiable, but an expansion or
				// aspect it composes may add one that is not: only a compile
				// can say whether dispatch would refuse it. Compile resolves by
				// the resolver (filename) key, which may differ from the
				// formula field, so compile the discovered candidate's key.
				var compileErr error
				if c.compile != nil {
					compileErr = c.compile(name, scope.paths, v2)
				}
				// A composed mismatch is excused only by a disabled_reason on
				// the root's OWN file (f, parsed unresolved from the file its
				// resolver key names), the formula that composes it. Resolve
				// fills reason from the first parent that has one, and that
				// parent's own requirement is satisfiable here, so an inherited
				// reason says nothing about this composition (Codex r7, #134).
				ownReason := ""
				if f.Requires != nil {
					ownReason = strings.TrimSpace(f.Requires.DisabledReason)
				}
				switch {
				case formula.IsUnsatisfiedRequirement(compileErr) && ownReason != "":
					note := fmt.Sprintf("intentionally disabled %s formula %q (%s), by a composed requirement: %s (disabled_reason declared on %q in %s)", scope.name, resolved.Formula, path, ownReason, name, path)
					if _, ok := seenDisabled[note]; !ok {
						seenDisabled[note] = struct{}{}
						disabled = append(disabled, note)
					}
				case formula.IsUnsatisfiedRequirement(compileErr):
					addIssue(formulaRequirementIssue{
						severity: StatusError,
						scope:    scope.name,
						formula:  resolved.Formula,
						path:     path,
						message:  fmt.Sprintf("%v; the inherited disabled_reason %q does not cover a composed requirement: declare disabled_reason on the formula that composes it (%s)", compileErr, reason, path),
					})
				case compileErr != nil:
					addIssue(formulaRequirementIssue{
						severity: StatusWarning,
						scope:    scope.name,
						formula:  resolved.Formula,
						path:     path,
						message:  fmt.Sprintf("declares disabled_reason %q; its own requirement is satisfiable here, and compiling it to check composed requirements failed: %v", reason, compileErr),
					})
				default:
					addIssue(formulaRequirementIssue{
						severity: StatusWarning,
						scope:    scope.name,
						formula:  resolved.Formula,
						path:     path,
						message:  fmt.Sprintf("declares disabled_reason %q but it compiles here, so the formula is DISPATCHABLE; make the requirement unsatisfiable or remove the marker", reason),
					})
				}
			}
		}
	}
	return issues, disabled
}

// coveringDisabledReasons reports whether every unmet compiler requirement of
// f is declared deliberate by the formula that states it. Resolve merges the
// constraints of all parents but keeps only the first disabled_reason, so one
// parent's stale marker could otherwise excuse another parent's genuine
// requirement. Each formula in the extends tree is instead checked on its OWN
// constraints, and an unmet one is covered only by a reason on that formula.
// Nodes are keyed by resolver key (name for f, the extends entries for its
// parents), never by the formula field, which two distinct files may share.
func coveringDisabledReasons(parser *formula.Parser, name string, f *formula.Formula, v2 bool) ([]string, bool) {
	var reasons []string
	visited := make(map[string]struct{})
	var covered func(key string, n *formula.Formula) bool
	covered = func(key string, n *formula.Formula) bool {
		if _, ok := visited[key]; ok {
			return true
		}
		visited[key] = struct{}{}
		if err := formula.ValidateHostRequirements(n, v2); err != nil {
			if !formula.IsUnsatisfiedRequirement(err) {
				return false
			}
			reason := ""
			if n.Requires != nil {
				reason = strings.TrimSpace(n.Requires.DisabledReason)
			}
			if reason == "" {
				return false
			}
			if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
		for _, parentKey := range n.Extends {
			parent, err := parser.LoadByName(parentKey)
			if err != nil || !covered(parentKey, parent) {
				return false
			}
		}
		return true
	}
	if !covered(name, f) {
		return nil, false
	}
	return reasons, len(reasons) > 0
}

func (c *FormulaRequirementsCheck) formulaScopes() []formulaRequirementScope {
	var scopes []formulaRequirementScope
	if len(c.cfg.FormulaLayers.City) > 0 {
		scopes = append(scopes, formulaRequirementScope{name: "city", paths: c.cfg.FormulaLayers.City})
	}
	rigNames := make([]string, 0, len(c.cfg.FormulaLayers.Rigs))
	for rigName := range c.cfg.FormulaLayers.Rigs {
		rigNames = append(rigNames, rigName)
	}
	slices.Sort(rigNames)
	for _, rigName := range rigNames {
		paths := c.cfg.FormulaLayers.Rigs[rigName]
		if len(paths) == 0 {
			continue
		}
		scopes = append(scopes, formulaRequirementScope{name: "rig:" + rigName, paths: paths})
	}
	return scopes
}

type formulaRequirementScope struct {
	name  string
	paths []string
}

type formulaRequirementIssue struct {
	severity CheckStatus
	scope    string
	formula  string
	path     string
	message  string
}

type formulaRequirementIssueKey struct {
	severity CheckStatus
	formula  string
	path     string
	message  string
}

func (i formulaRequirementIssue) dedupeKey() formulaRequirementIssueKey {
	return formulaRequirementIssueKey{
		severity: i.severity,
		formula:  i.formula,
		path:     pathutil.NormalizePathForCompare(i.path),
		message:  i.message,
	}
}

func (i formulaRequirementIssue) detail() string {
	severity := "warning"
	if i.severity == StatusError {
		severity = "error"
	}
	path := i.path
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return fmt.Sprintf("%s %s formula %q (%s): %s", severity, i.scope, i.formula, path, i.message)
}

func countFormulaRequirementIssues(issues []formulaRequirementIssue) (errors, warnings int) {
	for _, issue := range issues {
		if issue.severity == StatusError {
			errors++
		} else {
			warnings++
		}
	}
	return errors, warnings
}
