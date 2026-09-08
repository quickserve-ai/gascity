package doctor

import (
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const orderExecTargetName = "order-exec-target"

// OrderExecTargetCheck verifies that every enabled exec order's command can
// actually launch: when the command's first token is an absolute path, that
// path must exist and be executable. The dispatcher runs orders via
// `sh -c <command>`, so a script that lost its exec bit fails with
// "Permission denied" (exit 126) on every tick — and the only trace is a
// bare "failed" in order history plus a line in the supervisor log. The
// worktree-reaper-patrol order failed that way for 2.5 days after a rewrite
// recreated its script 0644 (ga-swawpo); this check turns that silence into
// a doctor error, and --fix restores the exec bit.
//
// Commands whose first token is not an absolute path (bare names resolved
// via PATH, shell syntax) are skipped rather than guessed at: the
// supervisor's PATH is not this process's PATH, and a false "missing"
// verdict is worse than staying silent.
type OrderExecTargetCheck struct {
	cfg      *config.City
	cityPath string
}

// NewOrderExecTargetCheck creates the exec-target launchability check.
func NewOrderExecTargetCheck(cfg *config.City, cityPath string) *OrderExecTargetCheck {
	return &OrderExecTargetCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderExecTargetCheck) Name() string { return orderExecTargetName }

// CanFix reports that missing exec bits can be restored automatically.
func (c *OrderExecTargetCheck) CanFix() bool { return true }

// WarmupEligible opts this check into `gc start`'s warm-up scan: a broken
// exec target means scheduled automation is already failing silently.
func (c *OrderExecTargetCheck) WarmupEligible() bool { return true }

// Run stats each enabled exec order's absolute-path target.
func (c *OrderExecTargetCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{Name: c.Name()}
	targets, scanErr := c.scanTargets(ctx)
	if scanErr != nil {
		result.Status = StatusError
		result.Message = scanErr.Error()
		return result
	}
	if targets == nil {
		result.Status = StatusOK
		result.Message = "no city config loaded"
		return result
	}

	var problems []string
	checked := 0
	for _, t := range targets {
		checked++
		info, err := os.Stat(t.path)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf(
				"%s: exec target %s: %v", t.order, t.path, err))
		case info.IsDir():
			problems = append(problems, fmt.Sprintf(
				"%s: exec target %s is a directory", t.order, t.path))
		case info.Mode()&0o111 == 0:
			problems = append(problems, fmt.Sprintf(
				"%s: exec target %s is not executable (sh -c fails with exit 126; chmod +x)",
				t.order, t.path))
		}
	}

	if len(problems) == 0 {
		result.Status = StatusOK
		result.Message = fmt.Sprintf("%d exec order target(s) launchable", checked)
		return result
	}
	result.Status = StatusError
	result.Message = fmt.Sprintf(
		"%d of %d exec order target(s) cannot launch: %s",
		len(problems), checked, problems[0])
	result.Details = problems
	result.FixHint = "chmod +x the listed scripts (gc doctor --fix restores missing exec bits; a rewrite via most editors and tools recreates files 0644)"
	return result
}

// Fix restores the exec bit on targets that exist as regular files but are
// not executable. Missing files and directories are not fixable here.
func (c *OrderExecTargetCheck) Fix(ctx *CheckContext) error {
	targets, scanErr := c.scanTargets(ctx)
	if scanErr != nil || targets == nil {
		return scanErr
	}
	var firstErr error
	for _, t := range targets {
		info, err := os.Stat(t.path)
		if err != nil || info.IsDir() || info.Mode()&0o111 != 0 {
			continue
		}
		if err := os.Chmod(t.path, info.Mode()|0o111); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("chmod +x %s: %w", t.path, err)
		}
	}
	return firstErr
}

type orderExecTarget struct {
	order string
	path  string
}

// scanTargets enumerates enabled exec orders and resolves each command's
// first token, keeping only absolute paths. A nil slice with nil error
// means no config was loaded.
func (c *OrderExecTargetCheck) scanTargets(ctx *CheckContext) ([]orderExecTarget, error) {
	if c.cfg == nil {
		return nil, nil
	}
	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		return nil, fmt.Errorf("city path unavailable")
	}
	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		return nil, fmt.Errorf("scan orders: %v", err)
	}
	targets := make([]orderExecTarget, 0, len(allOrders))
	for _, order := range allOrders {
		if !order.IsExec() {
			continue
		}
		path, ok := execCommandAbsTarget(order.Exec)
		if !ok {
			continue
		}
		targets = append(targets, orderExecTarget{order: orderDisplayName(order), path: path})
	}
	return targets, nil
}

// execCommandAbsTarget extracts the command's first token when it is a
// plain absolute path. Quoted or shell-syntax commands are skipped —
// this check verifies the common `exec = "/abs/path/script"` shape, not
// arbitrary shell.
func execCommandAbsTarget(command string) (string, bool) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	fields := strings.Fields(trimmed)
	first := fields[0]
	if strings.ContainsAny(first, "\"'`$\\;|&<>()") {
		return "", false
	}
	return first, true
}
