package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// deployFreeze is the machine-scoped gated-candidate marker (ga-rfdkxp).
// A gater (e.g. a frozen, identity-verified candidate awaiting operator
// approval) writes it to say "the next gc deploy on this machine is spoken
// for". Every deploy path funnels through `gc supervisor install`, so the
// check there means a routine carry-HEAD deploy cannot silently supersede a
// gated candidate — it must acknowledge the freeze by bead id, which leaves
// a durable, attributable record.
type deployFreeze struct {
	Bead     string `json:"bead"`
	SHA256   string `json:"sha256,omitempty"`
	Commit   string `json:"commit,omitempty"`
	FrozenBy string `json:"frozen_by,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Created  string `json:"created,omitempty"`
}

// deployFreezePath is the well-known marker location. Machine-scoped, not
// city-scoped: the frozen artifact is the machine's gc binary.
func deployFreezePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gc", "deploy-freeze.json")
}

// newSupervisorCheckFreezeCmd exposes checkDeployFreeze as a standalone,
// hidden entrypoint so non-Go install paths (the Makefile's `make install`)
// run the exact same guard instead of a shell reimplementation — a drifting
// shell duplicate accepted malformed markers and ignored archive failures
// (ga-rfdkxp review, 2026-09-08).
func newSupervisorCheckFreezeCmd(stdout, stderr io.Writer) *cobra.Command {
	var ack string
	cmd := &cobra.Command{
		Use:    "check-freeze",
		Short:  "Run the deploy-freeze gate (block on a standing freeze; --acknowledge-freeze supersedes)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if checkDeployFreeze(deployFreezePath(), ack, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ack, "acknowledge-freeze", "",
		"consciously supersede a standing deploy-freeze marker by its bead id (ga-rfdkxp)")
	return cmd
}

// checkDeployFreeze returns 0 when the install may proceed. A standing
// freeze blocks (exit 1) unless ack names the freeze's bead exactly; a
// marker that exists but cannot be read or parsed fails closed. On an
// acknowledged proceed the marker is renamed to a timestamped superseded
// record so the gate is consciously released, never silently bypassed.
func checkDeployFreeze(path, ack string, stdout, stderr io.Writer) int {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: deploy freeze marker %s exists but is unreadable (%v) — failing closed. Fix the marker or remove it with the gater's consent.\n", path, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	var fz deployFreeze
	if err := json.Unmarshal(data, &fz); err != nil || fz.Bead == "" {
		fmt.Fprintf(stderr, "gc supervisor install: deploy freeze marker %s is malformed (need JSON with a \"bead\" field) — failing closed. Repair it or remove it with the gater's consent.\n", path) //nolint:errcheck // best-effort stderr
		return 1
	}
	if ack == "" {
		fmt.Fprintf(stderr, "gc supervisor install: BLOCKED — a gated deploy candidate is frozen on this machine.\n"+
			"  bead: %s  frozen_by: %s  commit: %s\n  reason: %s\n"+
			"A routine deploy must not silently supersede a gated candidate (ga-rfdkxp).\n"+
			"Either install the frozen candidate per its bead, or consciously supersede:\n"+
			"  gc supervisor install --acknowledge-freeze %s\n"+
			"then stamp the supersede decision on the bead.\n",
			fz.Bead, fz.FrozenBy, fz.Commit, fz.Reason, fz.Bead) //nolint:errcheck // best-effort stderr
		return 1
	}
	if ack != fz.Bead {
		fmt.Fprintf(stderr, "gc supervisor install: --acknowledge-freeze %q does not match the standing freeze (bead %s) — refusing.\n", ack, fz.Bead) //nolint:errcheck // best-effort stderr
		return 1
	}
	superseded := path + ".superseded-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(path, superseded); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: could not record freeze supersede (%v) — refusing rather than proceeding without the durable record.\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "gc supervisor install: freeze %s consciously superseded; record kept at %s.\n"+
		"STAMP THE BEAD NOW: gc bd comment %s \"deploy freeze superseded by <who>: <why>\"\n",
		fz.Bead, superseded, fz.Bead) //nolint:errcheck // best-effort stdout
	return 0
}
