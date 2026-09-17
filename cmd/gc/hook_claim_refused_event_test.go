package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// Distinctive token strings, so a leak into the event log is unambiguous. They
// are credentials in production; no byte of either may reach an event.
const (
	refusalRuntimeToken = "rt-secret-4f1c9e2a7b6d5c3e"
	refusalBeadToken    = "bead-secret-9d8e7c6b5a4f3e2d"
)

// newRefusalSessionBead creates a session bead carrying the given state,
// instance token, and generation (the bead's runtime epoch).
func newRefusalSessionBead(t *testing.T, cityDir string, state session.State, instanceToken, generation string) string {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   session.BeadType,
		Labels: []string{"gc:session", "agent:worker-1"},
		Metadata: map[string]string{
			"session_name":   "worker-1",
			"template":       "worker",
			"state":          string(state),
			"instance_token": instanceToken,
			"generation":     generation,
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return bead.ID
}

// readRefusedEvents returns every hook.claim.refused event in the city's event
// log, plus the log's raw bytes for leak assertions. A missing log is empty.
func readRefusedEvents(t *testing.T, cityDir string) ([]events.Event, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		t.Fatalf("reading events log: %v", err)
	}
	var refused []events.Event
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var e events.Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("events log line is not an event: %v\n%s", err, line)
		}
		if e.Type == events.HookClaimRefused {
			refused = append(refused, e)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning events log: %v", err)
	}
	return refused, raw
}

// requireStaleDrainRecord asserts the refusal's exit code and --json drain
// record are exactly what they were before the event existed: exit 1 without
// --drain-ack, action=drain, the given reason, nothing acknowledged, no bead.
func requireStaleDrainRecord(t *testing.T, code int, stdout, stderr *bytes.Buffer, reason string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	want := hookClaimJSONResult{SchemaVersion: "1", OK: true, Command: hookClaimCommandName, Action: "drain", Reason: reason}
	var got hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != want.SchemaVersion || got.OK != want.OK || got.Command != want.Command ||
		got.Action != want.Action || got.Reason != want.Reason || got.DrainAcknowledged ||
		got.BeadID != "" || got.Assignee != "" || got.DeclinedForeign != 0 {
		t.Fatalf("drain record = %+v, want %+v", got, want)
	}
	if n := strings.Count(stdout.String(), "\n"); n != 1 {
		t.Fatalf("stdout carries %d lines, want exactly the one drain record: %q", n, stdout.String())
	}
}

// requireNoTokenLeak asserts no raw token appears anywhere in the event log.
func requireNoTokenLeak(t *testing.T, raw []byte, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if token != "" && bytes.Contains(raw, []byte(token)) {
			t.Fatalf("event log carries raw instance token %q:\n%s", token, raw)
		}
	}
}

func refusalFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:8]
}

// TestHookCommandClaimStaleSessionRecordsRefusedEvent drives every stale arm of
// the identity fence through the real command and proves each one records
// exactly one hook.claim.refused event naming WHICH check refused and what the
// bead said, while the exit code and drain record stay as they were and no raw
// token reaches the log (ga-cwu447).
func TestHookCommandClaimStaleSessionRecordsRefusedEvent(t *testing.T) {
	type setup struct {
		sessionID    string
		runtimeToken string
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, cityDir string) setup
		// want is checked field-by-field; SessionID is filled from setup.
		want        events.HookClaimRefusedPayload
		wantMatched *bool
	}{
		{
			name: "superseded token",
			setup: func(t *testing.T, cityDir string) setup {
				id := newRefusalSessionBead(t, cityDir, session.StateActive, refusalBeadToken, "3")
				return setup{sessionID: id, runtimeToken: refusalRuntimeToken}
			},
			want: events.HookClaimRefusedPayload{
				Detail:                  events.HookClaimRefusedDetailTokenSuperseded,
				State:                   string(session.StateActive),
				RuntimeTokenFingerprint: refusalFingerprint(refusalRuntimeToken),
				BeadTokenFingerprint:    refusalFingerprint(refusalBeadToken),
				RuntimeEpoch:            "2",
				BeadEpoch:               "3",
			},
			wantMatched: boolPtrForRefusalTest(false),
		},
		{
			name: "bead token missing",
			setup: func(t *testing.T, cityDir string) setup {
				id := newRefusalSessionBead(t, cityDir, session.StateActive, "", "2")
				return setup{sessionID: id, runtimeToken: refusalRuntimeToken}
			},
			want: events.HookClaimRefusedPayload{
				Detail:                  events.HookClaimRefusedDetailBeadTokenMissing,
				State:                   string(session.StateActive),
				RuntimeTokenFingerprint: refusalFingerprint(refusalRuntimeToken),
				RuntimeEpoch:            "2",
				BeadEpoch:               "2",
			},
			wantMatched: boolPtrForRefusalTest(false),
		},
		{
			name: "legacy bead with no generation",
			setup: func(t *testing.T, cityDir string) setup {
				id := newRefusalSessionBead(t, cityDir, session.StateActive, refusalBeadToken, "")
				return setup{sessionID: id, runtimeToken: refusalRuntimeToken}
			},
			want: events.HookClaimRefusedPayload{
				Detail:                  events.HookClaimRefusedDetailTokenSuperseded,
				State:                   string(session.StateActive),
				RuntimeTokenFingerprint: refusalFingerprint(refusalRuntimeToken),
				BeadTokenFingerprint:    refusalFingerprint(refusalBeadToken),
				RuntimeEpoch:            "2",
				// Normalized as a start normalizes it, not the raw empty string.
				BeadEpoch: "1",
			},
			wantMatched: boolPtrForRefusalTest(false),
		},
		{
			name: "dormant state",
			setup: func(t *testing.T, cityDir string) setup {
				id := newRefusalSessionBead(t, cityDir, session.StateFailedCreate, refusalRuntimeToken, "2")
				return setup{sessionID: id, runtimeToken: refusalRuntimeToken}
			},
			want: events.HookClaimRefusedPayload{
				Detail:                  events.HookClaimRefusedDetailStateNotEligible,
				State:                   string(session.StateFailedCreate),
				RuntimeTokenFingerprint: refusalFingerprint(refusalRuntimeToken),
				BeadTokenFingerprint:    refusalFingerprint(refusalRuntimeToken),
				RuntimeEpoch:            "2",
				BeadEpoch:               "2",
			},
			wantMatched: boolPtrForRefusalTest(true),
		},
		{
			name: "closed",
			setup: func(t *testing.T, cityDir string) setup {
				id := newRefusalSessionBead(t, cityDir, session.StateActive, refusalRuntimeToken, "2")
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatalf("openCityStoreAt: %v", err)
				}
				if err := store.Close(id); err != nil {
					t.Fatalf("closing session bead: %v", err)
				}
				return setup{sessionID: id, runtimeToken: refusalRuntimeToken}
			},
			want: events.HookClaimRefusedPayload{
				Detail:                  events.HookClaimRefusedDetailSessionClosed,
				State:                   string(session.StateActive),
				RuntimeTokenFingerprint: refusalFingerprint(refusalRuntimeToken),
				BeadTokenFingerprint:    refusalFingerprint(refusalRuntimeToken),
				RuntimeEpoch:            "2",
				BeadEpoch:               "2",
			},
			wantMatched: boolPtrForRefusalTest(true),
		},
		{
			name: "session bead not found",
			setup: func(_ *testing.T, _ string) setup {
				return setup{sessionID: "worker-1-vanished", runtimeToken: refusalRuntimeToken}
			},
			want: events.HookClaimRefusedPayload{
				Detail:                  events.HookClaimRefusedDetailSessionBeadNotFound,
				RuntimeTokenFingerprint: refusalFingerprint(refusalRuntimeToken),
				RuntimeEpoch:            "2",
			},
		},
		{
			name: "not a session bead",
			setup: func(t *testing.T, cityDir string) setup {
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatalf("openCityStoreAt: %v", err)
				}
				task, err := store.Create(beads.Bead{Title: "just a task", Type: "task"})
				if err != nil {
					t.Fatalf("create task bead: %v", err)
				}
				return setup{sessionID: task.ID, runtimeToken: refusalRuntimeToken}
			},
			want: events.HookClaimRefusedPayload{
				Detail:                  events.HookClaimRefusedDetailNotSessionBead,
				RuntimeTokenFingerprint: refusalFingerprint(refusalRuntimeToken),
				RuntimeEpoch:            "2",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			s := tc.setup(t, cityDir)
			queryMarker := installFenceWorkQueryProbe(t)
			setFenceClaimEnv(t, cityDir, s.sessionID, s.runtimeToken)
			t.Setenv("GC_RUNTIME_EPOCH", "2")

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

			requireStaleDrainRecord(t, code, &stdout, &stderr, hookClaimReasonStaleSession)
			if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
				t.Fatalf("work query ran for a refused session; stat error = %v", err)
			}

			refused, raw := readRefusedEvents(t, cityDir)
			if len(refused) != 1 {
				t.Fatalf("recorded %d hook.claim.refused events, want exactly 1; log:\n%s", len(refused), raw)
			}
			requireNoTokenLeak(t, raw, refusalRuntimeToken, refusalBeadToken)

			e := refused[0]
			if e.Subject != "worker" || e.SessionID != s.sessionID || e.Actor != "worker-1" {
				t.Fatalf("envelope subject/session/actor = %q/%q/%q, want worker/%q/worker-1", e.Subject, e.SessionID, e.Actor, s.sessionID)
			}
			if !strings.HasPrefix(e.Message, "stale session: ") {
				t.Fatalf("message = %q, want a stale-session message", e.Message)
			}
			var got events.HookClaimRefusedPayload
			if err := json.Unmarshal(e.Payload, &got); err != nil {
				t.Fatalf("payload does not decode: %v\n%s", err, e.Payload)
			}
			want := tc.want
			want.Reason = hookClaimReasonStaleSession
			want.SessionID = s.sessionID
			want.SessionName = "worker-1"
			want.Template = "worker"
			want.Agent = "worker-1"
			gotMatched, wantMatched := got.TokenMatched, tc.wantMatched
			got.TokenMatched, want.TokenMatched = nil, nil
			if got != want {
				t.Fatalf("payload =\n  %+v\nwant\n  %+v", got, want)
			}
			switch {
			case wantMatched == nil && gotMatched != nil:
				t.Fatalf("token_matched = %v, want absent (no bead was read)", *gotMatched)
			case wantMatched != nil && gotMatched == nil:
				t.Fatalf("token_matched absent, want %v", *wantMatched)
			case wantMatched != nil && *gotMatched != *wantMatched:
				t.Fatalf("token_matched = %v, want %v", *gotMatched, *wantMatched)
			}
		})
	}
}

// TestHookCommandClaimMissingSessionRegistrationRecordsRefusedEvent proves the
// unregistered-pool-runtime refusal records exactly one event with its own
// reason and detail and no bead-derived fields, its drain unchanged.
func TestHookCommandClaimMissingSessionRegistrationRecordsRefusedEvent(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	t.Setenv("GC_CITY", cityDir)
	installFenceWorkQueryProbe(t)
	setFenceClaimEnvMissingSessionID(t)
	t.Setenv("GC_RUNTIME_EPOCH", "4")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	requireStaleDrainRecord(t, code, &stdout, &stderr, hookClaimReasonMissingSessionRegistration)
	refused, raw := readRefusedEvents(t, cityDir)
	if len(refused) != 1 {
		t.Fatalf("recorded %d hook.claim.refused events, want exactly 1; log:\n%s", len(refused), raw)
	}
	e := refused[0]
	if e.Subject != "worker" || e.SessionID != "" {
		t.Fatalf("envelope subject/session = %q/%q, want worker/empty", e.Subject, e.SessionID)
	}
	var got events.HookClaimRefusedPayload
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatalf("payload does not decode: %v\n%s", err, e.Payload)
	}
	want := events.HookClaimRefusedPayload{
		Reason:       hookClaimReasonMissingSessionRegistration,
		Detail:       events.HookClaimRefusedDetailSessionIDUnset,
		SessionName:  "worker-1",
		Template:     "worker",
		Agent:        "worker-1",
		RuntimeEpoch: "4",
	}
	if got.TokenMatched != nil {
		t.Fatalf("token_matched = %v, want absent: no bead exists to compare against", *got.TokenMatched)
	}
	if got != want {
		t.Fatalf("payload =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestHookCommandClaimNonRefusalsRecordNoRefusedEvent proves the event is scoped
// to identity refusals: an idle no_work drain, the token-less compatibility
// escape hatch, and a session-store fault the fence fails open on all record
// nothing. no_work in particular is every idle poll of every seat.
//
// Each case proves POSITIVELY which fence path it took — the classifier seam
// records the verdict it returned, or records that it was never consulted — and
// that the command went on past the fence into the work query, so a case that
// silently exited early cannot pass for a non-refusal.
func TestHookCommandClaimNonRefusalsRecordNoRefusedEvent(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, cityDir string) (queryMarker string)
		// wantVerdicts is what the fence classifier must have returned; empty
		// means the fence must NOT have consulted it at all.
		wantVerdicts []hookClaimSessionVerdict
		wantReason   string
	}{
		{
			name: "eligible session drains no_work",
			setup: func(t *testing.T, cityDir string) string {
				id := newRefusalSessionBead(t, cityDir, session.StateActive, refusalRuntimeToken, "2")
				marker := installFenceWorkQueryProbe(t)
				setFenceClaimEnv(t, cityDir, id, refusalRuntimeToken)
				return marker
			},
			wantVerdicts: []hookClaimSessionVerdict{hookClaimSessionEligible},
			wantReason:   hookClaimReasonNoWork,
		},
		{
			name: "token-less runtime skips the fence",
			setup: func(t *testing.T, cityDir string) string {
				id := newRefusalSessionBead(t, cityDir, session.StateFailedCreate, refusalBeadToken, "2")
				marker := installFenceWorkQueryProbe(t)
				setFenceClaimEnv(t, cityDir, id, "")
				return marker
			},
			wantReason: hookClaimReasonNoWork,
		},
		{
			name: "session store fault fails open",
			setup: func(t *testing.T, cityDir string) string {
				marker := installFenceWorkQueryProbe(t)
				if err := os.WriteFile(filepath.Join(cityDir, ".gc", "beads.json"), []byte("{ not json"), 0o644); err != nil {
					t.Fatal(err)
				}
				setFenceClaimEnv(t, cityDir, "worker-1", refusalRuntimeToken)
				return marker
			},
			wantVerdicts: []hookClaimSessionVerdict{hookClaimSessionStoreUnavailable},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			queryMarker := tc.setup(t, cityDir)

			var verdicts []hookClaimSessionVerdict
			realClassify := hookClaimClassifySession
			hookClaimClassifySession = func(cityPath string, cfg *config.City, sessionID, instanceToken string) (hookClaimSessionVerdict, string, hookClaimStaleDetail) {
				verdict, reason, detail := realClassify(cityPath, cfg, sessionID, instanceToken)
				verdicts = append(verdicts, verdict)
				return verdict, reason, detail
			}
			t.Cleanup(func() { hookClaimClassifySession = realClassify })

			var stdout, stderr bytes.Buffer
			_ = cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

			if !slices.Equal(verdicts, tc.wantVerdicts) {
				t.Fatalf("fence classifier verdicts = %v, want %v; stderr=%s", verdicts, tc.wantVerdicts, stderr.String())
			}
			if _, err := os.Stat(queryMarker); err != nil {
				t.Fatalf("the command never got past the fence to the work query: %v; stderr=%s", err, stderr.String())
			}
			if tc.wantReason != "" {
				var result hookClaimJSONResult
				if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
					t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
				}
				if result.Action != "drain" || result.Reason != tc.wantReason {
					t.Fatalf("result = %+v, want action=drain reason=%s", result, tc.wantReason)
				}
			}
			refused, raw := readRefusedEvents(t, cityDir)
			if len(refused) != 0 {
				t.Fatalf("recorded %d hook.claim.refused events for a non-refusal, want 0; log:\n%s", len(refused), raw)
			}
		})
	}
}

// TestHookCommandClaimRefusalUnchangedWhenEventLogUnwritable proves recording is
// never load-bearing: with the city's event log unopenable (a directory where
// the file belongs) the recorder falls back to events.Discard, nothing is
// written, and the refusal writes byte-identical stdout, the same stderr, and
// the same exit code as a run whose event is recorded.
func TestHookCommandClaimRefusalUnchangedWhenEventLogUnwritable(t *testing.T) {
	run := func(t *testing.T, breakLog bool) (int, string, string) {
		clearGCEnv(t)
		disableManagedDoltRecoveryForTest(t)
		t.Setenv("GC_BEADS", "file")
		cityDir := writeFenceTestCity(t)
		id := newRefusalSessionBead(t, cityDir, session.StateActive, refusalBeadToken, "3")
		installFenceWorkQueryProbe(t)
		setFenceClaimEnv(t, cityDir, id, refusalRuntimeToken)
		logPath := filepath.Join(cityDir, ".gc", "events.jsonl")
		if breakLog {
			if err := os.MkdirAll(logPath, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		var opened []events.Recorder
		realOpen := hookClaimRefusedRecorder
		hookClaimRefusedRecorder = func(cityPath string) events.Recorder {
			rec := realOpen(cityPath)
			opened = append(opened, rec)
			return rec
		}
		t.Cleanup(func() { hookClaimRefusedRecorder = realOpen })

		var stdout, stderr bytes.Buffer
		code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
		requireStaleDrainRecord(t, code, &stdout, &stderr, hookClaimReasonStaleSession)

		if len(opened) != 1 {
			t.Fatalf("event recorder opened %d times, want exactly 1", len(opened))
		}
		if breakLog {
			if opened[0] != events.Discard {
				t.Fatalf("recorder = %T, want the events.Discard fallback for an unopenable log", opened[0])
			}
			entries, err := os.ReadDir(logPath)
			if err != nil {
				t.Fatalf("the directory standing in for the log is gone or unreadable: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("an unopenable log still received writes: %v", entries)
			}
		} else {
			if _, ok := opened[0].(*events.FileRecorder); !ok {
				t.Fatalf("control recorder = %T, want *events.FileRecorder", opened[0])
			}
			if refused, raw := readRefusedEvents(t, cityDir); len(refused) != 1 {
				t.Fatalf("control run recorded %d events, want 1; log:\n%s", len(refused), raw)
			}
		}
		return code, stdout.String(), strings.ReplaceAll(stderr.String(), id, "<session>")
	}

	var (
		okCode, brokenCode     int
		okStdout, brokenStdout string
		okStderr, brokenStderr string
	)
	t.Run("recorded", func(t *testing.T) { okCode, okStdout, okStderr = run(t, false) })
	t.Run("log unwritable", func(t *testing.T) { brokenCode, brokenStdout, brokenStderr = run(t, true) })

	if okCode != brokenCode || okStdout != brokenStdout || okStderr != brokenStderr {
		t.Fatalf("refusal changed with the event log unwritable:\n code %d vs %d\n stdout %q vs %q\n stderr %q vs %q",
			okCode, brokenCode, okStdout, brokenStdout, okStderr, brokenStderr)
	}
}

// TestHookCommandClaimRefusalRecordsBeforeDrainAck pins the ordering the emitter
// depends on: for both identity refusals, hook.claim.refused is recorded BEFORE
// the --drain-ack that lets the controller stop the seat. Recorded after it, the
// event would race the teardown of the very seat it describes.
func TestHookCommandClaimRefusalRecordsBeforeDrainAck(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T, cityDir string)
		wantReason string
	}{
		{
			name: "stale session",
			setup: func(t *testing.T, cityDir string) {
				id := newRefusalSessionBead(t, cityDir, session.StateActive, refusalBeadToken, "3")
				installFenceWorkQueryProbe(t)
				setFenceClaimEnv(t, cityDir, id, refusalRuntimeToken)
			},
			wantReason: hookClaimReasonStaleSession,
		},
		{
			name: "missing session registration",
			setup: func(t *testing.T, cityDir string) {
				t.Setenv("GC_CITY", cityDir)
				installFenceWorkQueryProbe(t)
				setFenceClaimEnvMissingSessionID(t)
			},
			wantReason: hookClaimReasonMissingSessionRegistration,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			tc.setup(t, cityDir)

			var order []string
			realEmit, realAck := hookEmitClaimRefused, hookClaimFenceDrainAck
			hookEmitClaimRefused = func(_, _ string, payload events.HookClaimRefusedPayload) {
				order = append(order, "emit:"+payload.Reason)
			}
			// Never the real ack: it would signal a controller.
			hookClaimFenceDrainAck = func(io.Writer) error {
				order = append(order, "drain-ack")
				return nil
			}
			t.Cleanup(func() { hookEmitClaimRefused, hookClaimFenceDrainAck = realEmit, realAck })

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true, DrainAck: true}, &stdout, &stderr)

			want := []string{"emit:" + tc.wantReason, "drain-ack"}
			if !slices.Equal(order, want) {
				t.Fatalf("call order = %v, want %v (the event must be recorded before the drain-ack)", order, want)
			}
			if code != 0 {
				t.Fatalf("code = %d, want 0 for an acknowledged drain; stderr=%s", code, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
			}
			if result.Action != "drain" || result.Reason != tc.wantReason || !result.DrainAcknowledged {
				t.Fatalf("result = %+v, want an acknowledged %s drain", result, tc.wantReason)
			}
		})
	}
}

// TestHookCommandClaimMissingRegistrationNeverCreditsTheOperator proves a
// refusing pool runtime with no alias, agent, session id or BEADS_ACTOR is not
// recorded under eventActor's "human" fallback.
func TestHookCommandClaimMissingRegistrationNeverCreditsTheOperator(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	t.Setenv("GC_CITY", cityDir)
	installFenceWorkQueryProbe(t)
	setFenceClaimEnvMissingSessionID(t)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("BEADS_ACTOR", "")
	if got := eventActor(); got != "human" {
		t.Fatalf("precondition: eventActor() = %q, want the \"human\" fallback this test guards against", got)
	}

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	requireStaleDrainRecord(t, code, &stdout, &stderr, hookClaimReasonMissingSessionRegistration)
	refused, raw := readRefusedEvents(t, cityDir)
	if len(refused) != 1 {
		t.Fatalf("recorded %d hook.claim.refused events, want exactly 1; log:\n%s", len(refused), raw)
	}
	if refused[0].Actor != "gc-hook:worker" {
		t.Fatalf("actor = %q, want gc-hook:worker", refused[0].Actor)
	}
}

// TestHookClaimRefusedActor covers the actor substitution without a command run.
func TestHookClaimRefusedActor(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("BEADS_ACTOR", "")
	if got := hookClaimRefusedActor(" worker "); got != "gc-hook:worker" {
		t.Fatalf("actor = %q, want gc-hook:worker", got)
	}
	if got := hookClaimRefusedActor(""); got != "gc-hook" {
		t.Fatalf("actor with no template = %q, want gc-hook", got)
	}
	t.Setenv("GC_ALIAS", "worker-1")
	if got := hookClaimRefusedActor("worker"); got != "worker-1" {
		t.Fatalf("actor = %q, want the runtime's own alias", got)
	}
}

// TestHookClaimRuntimeGenerationMatchesTheStartPath pins bead_epoch's
// normalization to the one a start applies when it stamps GC_RUNTIME_EPOCH, so a
// legacy bead with no generation reads "1" beside a runtime_epoch of "1".
func TestHookClaimRuntimeGenerationMatchesTheStartPath(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"", "1"},
		{"0", "1"},
		{"-2", "1"},
		{"abc", "1"},
		{" 3", "1"}, // the start path parses the untrimmed value
		{"1", "1"},
		{"4", "4"},
	}
	for _, tc := range cases {
		if got := hookClaimRuntimeGeneration(tc.raw); got != tc.want {
			t.Errorf("hookClaimRuntimeGeneration(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestHookClaimRefusedPayloadNeverCarriesRawTokens pins the security contract at
// the assembly seam: whatever the tokens, the marshaled payload holds only their
// 8-hex-char SHA-256 prefixes.
func TestHookClaimRefusedPayloadNeverCarriesRawTokens(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_NAME", "worker-1")
	t.Setenv("GC_AGENT", "rig/worker-1")
	t.Setenv("GC_RUNTIME_EPOCH", "7")

	_, _, detail := hookClaimSessionEligibility(session.Info{
		ID:            "s-1",
		MetadataState: string(session.StateActive),
		InstanceToken: refusalBeadToken,
		Generation:    "8",
	}, refusalRuntimeToken)
	payload := hookClaimRefusedPayload(hookClaimReasonStaleSession, "s-1", refusalRuntimeToken, detail)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	requireNoTokenLeak(t, body, refusalRuntimeToken, refusalBeadToken)
	if payload.RuntimeTokenFingerprint != refusalFingerprint(refusalRuntimeToken) ||
		payload.BeadTokenFingerprint != refusalFingerprint(refusalBeadToken) {
		t.Fatalf("fingerprints = %q/%q, want the 8-char sha256 prefixes", payload.RuntimeTokenFingerprint, payload.BeadTokenFingerprint)
	}
	if len(payload.RuntimeTokenFingerprint) != 8 {
		t.Fatalf("fingerprint %q is %d chars, want 8", payload.RuntimeTokenFingerprint, len(payload.RuntimeTokenFingerprint))
	}
	if payload.Agent != "rig/worker-1" || payload.RuntimeEpoch != "7" || payload.BeadEpoch != "8" {
		t.Fatalf("payload = %+v, want agent from GC_AGENT, runtime epoch 7, bead epoch 8", payload)
	}
	if hookClaimTokenFingerprint("") != "" || hookClaimTokenFingerprint("   ") != "" {
		t.Fatalf("an empty token must have no fingerprint")
	}
}

// TestEmitHookClaimRefusedIgnoresEmptyCityPath proves the emitter never falls
// back to a working-directory-relative event log.
func TestEmitHookClaimRefusedIgnoresEmptyCityPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	emitHookClaimRefused("", "stale session: test", events.HookClaimRefusedPayload{Reason: hookClaimReasonStaleSession})
	if _, err := os.Stat(filepath.Join(dir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("empty city path wrote under the working directory; stat error = %v", err)
	}
}

func boolPtrForRefusalTest(v bool) *bool { return &v }
