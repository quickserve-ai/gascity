package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/notify/claudecloud"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// newSessionBindCloudCmd creates "gc session bind-cloud" — the binding
// surface of the claude-cloud wake transport (claudemsg-bridge-design.md
// §5.2, ga-bjbaui stage 5). The design drafted this as `gc session adopt
// --cloud-id`, but no adopt command exists; binding is its own explicit
// operator/owner action, so it gets its own verb.
func newSessionBindCloudCmd(stdout, stderr io.Writer) *cobra.Command {
	var cloudID, accountDir string
	var clearBinding bool
	cmd := &cobra.Command{
		Use:   "bind-cloud <session-id-or-alias>",
		Short: "Bind a seat to a Claude Code cloud session for claude-cloud wake delivery",
		Long: `Bind a seat's session bead to a Claude Code cloud session so the
claude-cloud wake transport (wake_transport = "claude-cloud" on the agent)
can deliver wake hints into it.

A binding is two facts stamped on the session bead: the cloud session ID
(session_...) and the account lineage directory (CLAUDE_CONFIG_DIR) that
owns the cloud session — sends run under that lineage only, never ambient
auth. Re-running the command rebinds and clears any suspect marker a failed
send left behind (rebinding IS the explicit recovery action the suspect
state waits for). --clear removes the binding and its delivery-state facts.

The binding is inert until the seat's agent config selects
wake_transport = "claude-cloud".`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionBindCloud(args[0], cloudID, accountDir, clearBinding, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().StringVar(&cloudID, "cloud-id", "", "cloud session ID (session_...)")
	cmd.Flags().StringVar(&accountDir, "account-dir", "", "account lineage directory (CLAUDE_CONFIG_DIR) that owns the cloud session")
	cmd.Flags().BoolVar(&clearBinding, "clear", false, "remove the cloud binding and its delivery-state facts")
	return cmd
}

func cmdSessionBindCloud(target, cloudID, accountDir string, clearBinding bool, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "gc session bind-cloud: "+format+"\n", a...) //nolint:errcheck // best-effort stderr
		return 1
	}
	if clearBinding {
		if strings.TrimSpace(cloudID) != "" || strings.TrimSpace(accountDir) != "" {
			return fail("--clear cannot be combined with --cloud-id/--account-dir")
		}
	} else {
		if !claudecloud.ValidSessionID(cloudID) {
			return fail("--cloud-id %q is not a cloud session ID (session_...)", cloudID)
		}
		dir := strings.TrimSpace(accountDir)
		if dir == "" {
			return fail("--account-dir is required: sends run under the binding's declared account lineage, never ambient auth (design §5.1)")
		}
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return fail("--account-dir %q is not an existing directory", dir)
		}
	}

	store, code := openCityStore(stderr, "gc session bind-cloud")
	if store == nil {
		return code
	}
	cityPath, err := resolveCity()
	if err != nil {
		return fail("%v", err)
	}
	cfg, _ := loadCityConfig(cityPath, stderr)
	sessStore := cliSessionStore(store, cfg, cityPath)
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, sessStore, target)
	if err != nil {
		return fail("%v", err)
	}
	front := cliSessionFrontDoor(store, cfg, cityPath)
	info, err := front.Get(sessionID)
	if err != nil {
		return fail("reading session bead %s: %v", sessionID, err)
	}

	if clearBinding {
		// One all-or-nothing write (UpdateMetadataInfo, the single-Update
		// chokepoint) so a failure can never leave a half-cleared binding.
		patch := session.MetadataPatch{
			session.MetadataCloudWakeSessionID:        "",
			session.MetadataCloudWakeAccountDir:       "",
			session.MetadataCloudWakeBindingSuspect:   "",
			session.MetadataCloudWakeBindingSuspectAt: "",
			session.MetadataCloudWakeLastOutcome:      "",
			session.MetadataCloudWakeLastOutcomeAt:    "",
			session.MetadataCloudWakeBoundAt:          "",
		}
		if _, err := front.UpdateMetadataInfo(info, patch); err != nil {
			return fail("clearing cloud binding on %s: %v", sessionID, err)
		}
		fmt.Fprintf(stdout, "Cleared cloud binding on %s\n", sessionID) //nolint:errcheck
		return 0
	}

	// Persist an absolute lineage path: the transport passes it verbatim as
	// the child's CLAUDE_CONFIG_DIR from whatever directory delivery runs
	// in, so a relative path would select a different (or missing) lineage
	// depending on process CWD.
	absDir, err := filepath.Abs(strings.TrimSpace(accountDir))
	if err != nil {
		return fail("resolving --account-dir %q: %v", accountDir, err)
	}
	rebound := strings.TrimSpace(info.CloudWakeSessionID) != "" &&
		strings.TrimSpace(info.CloudWakeSessionID) != strings.TrimSpace(cloudID)
	// The whole transition commits atomically or not at all: a failure can
	// never pair a new cloud ID with the old lineage, or clear the suspect
	// facts without installing the binding they belonged to.
	patch := session.MetadataPatch{
		session.MetadataCloudWakeSessionID:  strings.TrimSpace(cloudID),
		session.MetadataCloudWakeAccountDir: absDir,
		// A (re)bind is the explicit recovery action the suspect state waits
		// for (design §5.2: suspect is never auto-cleared, never auto-
		// deleted). Delivery-state facts describe the previous binding, so
		// they reset with it.
		session.MetadataCloudWakeBindingSuspect:   "",
		session.MetadataCloudWakeBindingSuspectAt: "",
		session.MetadataCloudWakeLastOutcome:      "",
		session.MetadataCloudWakeLastOutcomeAt:    "",
		session.MetadataCloudWakeBoundAt:          time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := front.UpdateMetadataInfo(info, patch); err != nil {
		return fail("stamping cloud binding on %s: %v", sessionID, err)
	}

	verb := "Bound"
	if rebound {
		verb = "Rebound"
	}
	fmt.Fprintf(stdout, "%s %s to cloud session %s (lineage %s)\n", verb, sessionID, strings.TrimSpace(cloudID), absDir) //nolint:errcheck
	if agent, ok := bindCloudAgentForInfo(cfg, info); ok && strings.TrimSpace(agent.WakeTransport) != config.WakeTransportClaudeCloud {
		fmt.Fprintf(stdout, "note: agent %q does not select wake_transport = %q — the binding is inert until it does\n", agent.QualifiedName(), config.WakeTransportClaudeCloud) //nolint:errcheck
	}
	return 0
}

// bindCloudAgentForInfo finds the configured agent backing a session, for the
// inert-binding hint. Best-effort: an unresolvable agent just omits the hint.
func bindCloudAgentForInfo(cfg *config.City, info session.Info) (config.Agent, bool) {
	if cfg == nil {
		return config.Agent{}, false
	}
	name := strings.TrimSpace(info.AgentName)
	if name == "" {
		name = strings.TrimSpace(info.Template)
	}
	if name == "" {
		return config.Agent{}, false
	}
	for i := range cfg.Agents {
		a := cfg.Agents[i]
		if a.QualifiedName() == name || a.Name == name {
			return a, true
		}
	}
	return config.Agent{}, false
}
