package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// The nudge delivery log (ga-ubfc7j) is an append-only JSON-lines record of
// what the queued-nudge path did with each claim it tried to hand a seat:
// delivered, deferred (and why), or failed. It exists so an answer a seat
// recorded -- above all a question-dialog answer -- can be matched against
// the deliveries that reached that seat's pane around the same time. Before
// it, state.json kept only pending and dead items, so nothing said when a
// reminder had been typed into a pane.
//
// It lives beside the queue, at .gc/runtime/nudges/deliveries.jsonl (see
// nudgeDeliveryLogPath). A dedicated file rather than the worker.operation
// event: that event carries no queue item ids or message identity, its
// payload is mirrored field for field by the API's wire type, and the
// poller's worker handle is not guaranteed a recorder.

// nudgeDeliveryRecord is one line of the nudge delivery log.
type nudgeDeliveryRecord struct {
	Time      time.Time `json:"ts"`
	Agent     string    `json:"agent"`
	Session   string    `json:"session,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	// ItemIDs are the queue items this delivery carried, in order.
	ItemIDs []string `json:"item_ids"`
	// MessageSHA256 is the hex SHA-256 of the exact text handed to the
	// runtime (the formatted reminder block), so a transcript's injected
	// text can be matched to a delivery without the log holding the text.
	MessageSHA256 string `json:"message_sha256"`
	// Outcome is one of the nudgeDeliveryOutcome* values.
	Outcome string `json:"outcome"`
	// Reason is the deferral reason (tmux.NudgeDeferReason*) for a deferral
	// or a withheld submit, or the error text for a failure.
	Reason string `json:"reason,omitempty"`
}

const (
	nudgeDeliveryOutcomeDelivered           = "delivered"
	nudgeDeliveryOutcomeDeliveredUnobserved = "delivered_unobserved"
	nudgeDeliveryOutcomeDeferred            = "deferred"
	// nudgeDeliveryOutcomeSubmitWithheld: the text reached the composer but
	// the guard withheld the Enter (a prompt appeared after typing).
	nudgeDeliveryOutcomeSubmitWithheld = "submit_withheld"
	nudgeDeliveryOutcomeFailed         = "failed"
	nudgeDeliveryOutcomeNotDelivered   = "not_delivered"
)

// nudgeDeliveryLogMaxBytes bounds the live log; past it the file is rotated
// to deliveries.jsonl.1 (one generation kept).
const nudgeDeliveryLogMaxBytes = 8 << 20

var (
	nudgeDeliveryLogMu sync.Mutex
	// lastNudgeDeferral remembers, per log and session, the last deferral
	// written, so a dialog left open does not add a line every poll pass (2s):
	// a repeat of the same reason for the same items is not rewritten. Any
	// other outcome for the session clears it.
	lastNudgeDeferral = map[string]string{}
)

func nudgeDeliveryLogPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "deliveries.jsonl")
}

func nudgeMessageSHA256(message string) string {
	sum := sha256.Sum256([]byte(message))
	return hex.EncodeToString(sum[:])
}

// nudgeDeliverySeat names the seat a delivery went to, the way a reader
// matching an answer to a delivery knows it: its alias or configured name.
// agentKey, the queue's own key, prefers the session id, which the record
// already carries in session_id.
func nudgeDeliverySeat(t nudgeTarget) string {
	if t.alias != "" {
		return t.alias
	}
	if qn := t.agent.QualifiedName(); qn != "" {
		return qn
	}
	if t.identity != "" {
		return t.identity
	}
	return t.agentKey()
}

// recordNudgeDelivery appends one delivery record for a queued-nudge claim.
func recordNudgeDelivery(target nudgeTarget, items []queuedNudge, message, outcome, reason string) error {
	return appendNudgeDeliveryRecord(target.cityPath, nudgeDeliveryRecord{
		Time:          time.Now().UTC(),
		Agent:         nudgeDeliverySeat(target),
		Session:       target.sessionName,
		SessionID:     target.sessionID,
		ItemIDs:       queuedNudgeIDs(items),
		MessageSHA256: nudgeMessageSHA256(message),
		Outcome:       outcome,
		Reason:        reason,
	})
}

// appendNudgeDeliveryRecord appends rec as one JSON line. One write per record
// on an O_APPEND descriptor keeps lines whole when several gc processes (the
// supervisor's dispatcher, per-session pollers) append at once.
func appendNudgeDeliveryRecord(cityPath string, rec nudgeDeliveryRecord) error {
	path := nudgeDeliveryLogPath(cityPath)
	dedupeKey := path + "\x00" + rec.Session + "\x00" + rec.Agent
	nudgeDeliveryLogMu.Lock()
	defer nudgeDeliveryLogMu.Unlock()
	sig := ""
	if rec.Outcome == nudgeDeliveryOutcomeDeferred {
		sig = rec.Reason + "\x00" + strings.Join(rec.ItemIDs, ",")
		if lastNudgeDeferral[dedupeKey] == sig {
			return nil
		}
	}
	// The dedupe entry moves only once the line is written, so a failed
	// write is retried on the next pass instead of silently suppressed.
	if err := writeNudgeDeliveryLine(path, rec); err != nil {
		return err
	}
	if sig != "" {
		lastNudgeDeferral[dedupeKey] = sig
	} else {
		delete(lastNudgeDeferral, dedupeKey)
	}
	return nil
}

// writeNudgeDeliveryLine appends rec to the log at path, rotating first when
// it is over nudgeDeliveryLogMaxBytes.
func writeNudgeDeliveryLine(path string, rec nudgeDeliveryRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding nudge delivery record: %w", err)
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("nudge delivery log: %w", err)
	}
	// The mutex above serializes this process only. Several gc processes
	// append here, so rotation and the append run under an flock on a sibling
	// lock file: without it two processes can both see an oversized log and
	// both rotate, the second renaming the fresh log over .1 and deleting the
	// previous generation of delivery evidence.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("nudge delivery log lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck // closing releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("nudge delivery log lock: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= nudgeDeliveryLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("nudge delivery log: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("nudge delivery log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("nudge delivery log: %w", err)
	}
	return nil
}

// readNudgeDeliveryRecords returns every record in the live delivery log,
// oldest first. A missing log is an empty history.
func readNudgeDeliveryRecords(cityPath string) ([]nudgeDeliveryRecord, error) {
	f, err := os.Open(nudgeDeliveryLogPath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []nudgeDeliveryRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec nudgeDeliveryRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}
