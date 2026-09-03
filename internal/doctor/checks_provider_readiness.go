package doctor

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ProviderReadinessCheck flags agents whose RESOLVED provider declares no
// readiness signal at all — neither a ready_prompt_prefix nor a positive
// ready_delay_ms.
//
// This is the config-level half of the ga-8ouxd guard (the builtin table has
// its own unit test). The builtin fix alone is not enough: agent-level
// overrides are pointer-typed, so `ready_delay_ms = 0` on an agent patch
// legitimately parses and zeroes out the only signal a prefix-less provider
// has. A session on such a composition never leaves state=creating — gc
// cannot observe it become ready, LAST_ACTIVE is never set, and the
// reconciler retries the start forever. That failure is silent at config
// time, which is exactly why it survived for weeks on omp; this check makes
// it a doctor finding instead.
type ProviderReadinessCheck struct {
	cfg *config.City
	// lookPath is a seam for tests; production uses exec.LookPath.
	lookPath config.LookPathFunc
}

// NewProviderReadinessCheck creates the provider readiness doctor check.
func NewProviderReadinessCheck(cfg *config.City) *ProviderReadinessCheck {
	return &ProviderReadinessCheck{cfg: cfg, lookPath: exec.LookPath}
}

// Name returns the check identifier shown by gc doctor.
func (c *ProviderReadinessCheck) Name() string { return "provider-readiness" }

// Run resolves every agent's effective provider and reports the ones that
// would strand their sessions in state=creating.
func (c *ProviderReadinessCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Status: StatusOK}
	if c.cfg == nil {
		r.Message = "city config unavailable"
		return r
	}

	offenders := map[string][]string{} // provider label -> agents
	for i := range c.cfg.Agents {
		agent := &c.cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		// The start_command escape hatch is the operator writing their own
		// lifecycle (typically a shell loop, e.g. the control dispatchers):
		// there is no provider TUI whose welcome screen could eat the first
		// prompt, which is the actual damage mechanism here — without a
		// signal the runtime SKIPS the ready wait, the initial prompt lands
		// on a not-yet-ready TUI and is lost, and the session idles forever
		// at the welcome screen, reading as stuck in state=creating.
		if strings.TrimSpace(agent.StartCommand) != "" {
			continue
		}
		resolved, err := config.ResolveProvider(agent, &c.cfg.Workspace, c.cfg.Providers, c.lookPath)
		if err != nil || resolved == nil {
			// An unresolvable provider is a different defect with its own
			// reporting; flagging it here too would double-count.
			continue
		}
		// Bounded one-shot commands do their work and exit; "ready" does
		// not apply to them, only to interactive sessions gc must observe.
		if runtime.Lifecycle(resolved.Lifecycle) == runtime.LifecycleOneShot {
			continue
		}
		if strings.TrimSpace(resolved.ReadyPromptPrefix) == "" && resolved.ReadyDelayMs <= 0 {
			label := strings.TrimSpace(agent.Provider)
			if label == "" {
				label = strings.TrimSpace(c.cfg.Workspace.Provider)
			}
			if label == "" {
				label = resolved.Command
			}
			offenders[label] = append(offenders[label], agent.QualifiedName())
		}
	}

	if len(offenders) == 0 {
		r.Message = "every agent's resolved provider declares a readiness signal"
		return r
	}

	labels := make([]string, 0, len(offenders))
	for label := range offenders {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	total := 0
	for _, label := range labels {
		agents := offenders[label]
		sort.Strings(agents)
		total += len(agents)
		r.Details = append(r.Details, fmt.Sprintf("provider %q resolves with neither ready_prompt_prefix nor ready_delay_ms for: %s", label, strings.Join(agents, ", ")))
	}
	r.Status = StatusError
	r.Message = fmt.Sprintf("%d agent(s) resolve to a provider with NO readiness signal — their sessions will never leave state=creating (ga-8ouxd)", total)
	r.FixHint = "set ready_prompt_prefix or a positive ready_delay_ms on the provider (or drop the agent-level ready_delay_ms = 0 override)"
	return r
}

// CanFix reports that this check has no automated fix.
func (c *ProviderReadinessCheck) CanFix() bool { return false }

// Fix is a no-op; the fix is a config decision.
func (c *ProviderReadinessCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns true: a signal-less provider composition strands
// every session it spawns, which is exactly the class worth catching at
// `gc start` before the reconciler begins retrying doomed creates.
func (c *ProviderReadinessCheck) WarmupEligible() bool { return true }
