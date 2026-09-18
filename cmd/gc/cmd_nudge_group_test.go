package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// `gc nudge <target> <message>` looks like a send but the nudge group only
// inspects and drains deferred nudges. It must fail loudly and point callers
// at `gc session nudge` instead of printing group help and exiting 0.
func TestNudgeUnknownSubcommandFailsAndPointsAtSessionNudge(t *testing.T) {
	clearGCEnv(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"nudge", "katya", "check deploy status"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc nudge katya MESSAGE exited 0; want non-zero\nstdout=%q\nstderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "katya"`) {
		t.Errorf("stderr = %q, want unknown subcommand \"katya\"", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc session nudge") {
		t.Errorf("stderr = %q, want pointer to gc session nudge", stderr.String())
	}
	if strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("stdout = %q, want no group help on an unknown subcommand", stdout.String())
	}
}

func TestNudgeMissingSubcommandFails(t *testing.T) {
	clearGCEnv(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"nudge"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc nudge exited 0; want non-zero\nstdout=%q\nstderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing subcommand") {
		t.Errorf("stderr = %q, want 'missing subcommand'", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc session nudge") {
		t.Errorf("stderr = %q, want pointer to gc session nudge", stderr.String())
	}
}

func TestNudgeExplicitHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{
		{"nudge", "--help"},
		{"nudge", "-h"},
		{"help", "nudge"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			clearGCEnv(t)
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("gc %s exited %d; want 0\nstderr=%q", strings.Join(args, " "), code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Inspect and deliver deferred nudges") {
				t.Errorf("stdout = %q, want nudge group help", stdout.String())
			}
		})
	}
}

func TestNudgeSubcommandsStillResolve(t *testing.T) {
	root := newRootCmd(io.Discard, io.Discard)
	for _, name := range []string{"status", "drain", "poll"} {
		cmd, _, err := root.Find([]string{"nudge", name, "some-session"})
		if err != nil {
			t.Fatalf("Find(nudge %s): %v", name, err)
		}
		if cmd.Name() != name || cmd.Parent() == nil || cmd.Parent().Name() != "nudge" {
			t.Errorf("Find(nudge %s) resolved to %q; want nudge %s", name, cmd.CommandPath(), name)
		}
	}
}

func TestNudgeStatusStillRunsItsOwnValidation(t *testing.T) {
	clearGCEnv(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"nudge", "status"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc nudge status with no session exited 0; want non-zero")
	}
	if !strings.Contains(stderr.String(), "gc nudge status: session not specified") {
		t.Errorf("stderr = %q, want status subcommand's own error", stderr.String())
	}
	if strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr = %q, status must not be treated as unknown", stderr.String())
	}
}
