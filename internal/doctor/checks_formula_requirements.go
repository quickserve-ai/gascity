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
)

// FormulaRequirementsCheck reports formula compiler requirement migration and
// compatibility diagnostics across the visible city and rig formula layers.
type FormulaRequirementsCheck struct {
	cfg *config.City
	// compile compiles a formula by name the way dispatch does. It settles a
	// disabled_reason whose own requirement is satisfiable: expansions and
	// aspects add their requirements only at compile time.
	compile func(name string, searchPaths []string) error
}

// NewFormulaRequirementsCheck creates a formula requirements doctor check.
func NewFormulaRequirementsCheck(cfg *config.City, _ string) *FormulaRequirementsCheck {
	return &FormulaRequirementsCheck{cfg: cfg, compile: compileFormulaForRequirements}
}

func compileFormulaForRequirements(name string, searchPaths []string) error {
	_, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), name, searchPaths, nil)
	return err
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

	issues, disabled := c.collectIssues()
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
func (c *FormulaRequirementsCheck) collectIssues() ([]formulaRequirementIssue, []string) {
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
			ownReason := ""
			if f.Requires != nil {
				ownReason = strings.TrimSpace(f.Requires.DisabledReason)
			}
			v2 := c.cfg.Daemon.FormulaV2Enabled()
			hostErr := formula.ValidateHostRequirements(resolved, v2)
			// A reason inherited through extends excuses only what the parent
			// made unsatisfiable: if the formula's OWN requirement is unmet, it
			// must carry its own reason.
			ownUnexcused := ownReason == "" && formula.IsUnsatisfiedRequirement(formula.ValidateHostRequirements(f, v2))
			intentionallyDisabled := reason != "" && formula.IsUnsatisfiedRequirement(hostErr) && !ownUnexcused
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
				note := fmt.Sprintf("intentionally disabled %s formula %q (%s): %s", scope.name, resolved.Formula, path, reason)
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
				// can say whether dispatch would refuse it.
				var compileErr error
				if c.compile != nil {
					compileErr = c.compile(resolved.Formula, scope.paths)
				}
				switch {
				case formula.IsUnsatisfiedRequirement(compileErr):
					note := fmt.Sprintf("intentionally disabled %s formula %q (%s), by a composed requirement: %s", scope.name, resolved.Formula, path, reason)
					if _, ok := seenDisabled[note]; !ok {
						seenDisabled[note] = struct{}{}
						disabled = append(disabled, note)
					}
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
