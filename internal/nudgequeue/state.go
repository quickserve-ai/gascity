// Package nudgequeue manages the persisted deferred-nudge queue.
package nudgequeue

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// wakeSocketPathLimit caps the canonical socket path length below the
// platform sockaddr_un limit (108 bytes on Linux, 104 on macOS). Matches
// the controllerSocketPathLimit pattern in cmd/gc/controller.go.
const wakeSocketPathLimit = 100

// Reference links a queued nudge back to the object that produced it.
type Reference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Item is a persisted deferred nudge.
type Item struct {
	ID                string `json:"id"`
	BeadID            string `json:"bead_id,omitempty"`
	Agent             string `json:"agent"`
	SessionID         string `json:"session_id,omitempty"`
	ContinuationEpoch string `json:"continuation_epoch,omitempty"`
	Source            string `json:"source"`
	// Sender is the self-reported identity of the enqueuing process
	// (GC_AGENT / GC_ALIAS / BEADS_ACTOR / BD_ACTOR at enqueue time). It is
	// honest-reporting provenance, not authenticated: treat it as a trace
	// aid, never as authority (ga-txbsqo).
	Sender        string `json:"sender,omitempty"`
	SenderSession string `json:"sender_session,omitempty"`

	Message       string     `json:"message"`
	Reference     *Reference `json:"reference,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	DeliverAfter  time.Time  `json:"deliver_after"`
	ExpiresAt     time.Time  `json:"expires_at"`
	Attempts      int        `json:"attempts,omitempty"`
	LastAttemptAt time.Time  `json:"last_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	ClaimedAt     time.Time  `json:"claimed_at,omitempty"`
	LeaseUntil    time.Time  `json:"lease_until,omitempty"`
	DeadAt        time.Time  `json:"dead_at,omitempty"`

	// unknown carries JSON fields written by a newer gc build than this one.
	// state.json is round-tripped by whichever gc process runs a maintenance
	// pass; without this overlay an old-schema process silently strips every
	// field it does not know, city-wide, during every deployment window
	// (ga-aj9auz). Populated only when such fields exist.
	unknown map[string]json.RawMessage
}

// State is the persisted nudge queue snapshot.
type State struct {
	Pending  []Item `json:"pending,omitempty"`
	InFlight []Item `json:"in_flight,omitempty"`
	Dead     []Item `json:"dead,omitempty"`

	// unknown preserves top-level keys from newer schemas across round-trips
	// by older binaries; see Item.unknown (ga-aj9auz).
	unknown map[string]json.RawMessage
}

// itemAlias and stateAlias strip the custom JSON methods so the standard
// struct encoding can be reused inside them without recursion.
type (
	itemAlias  Item
	stateAlias State
)

// knownJSONKeys derives the set of JSON keys a struct type owns from its
// field tags. Derived, not hand-maintained: a hand-kept list would recreate
// the original hazard the first time a field was added without updating it.
func knownJSONKeys(t reflect.Type) map[string]struct{} {
	keys := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		keys[name] = struct{}{}
	}
	return keys
}

var (
	itemKnownKeys  = sync.OnceValue(func() map[string]struct{} { return knownJSONKeys(reflect.TypeOf(Item{})) })
	stateKnownKeys = sync.OnceValue(func() map[string]struct{} { return knownJSONKeys(reflect.TypeOf(State{})) })
)

// unknownJSONFields returns the keys in data not claimed by known.
func unknownJSONFields(data []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for k := range known {
		delete(raw, k)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
}

// mergeUnknownJSONFields re-attaches preserved unknown fields to an encoded
// object. Known fields win on any collision.
func mergeUnknownJSONFields(encoded []byte, unknown map[string]json.RawMessage) ([]byte, error) {
	if len(unknown) == 0 {
		return encoded, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &merged); err != nil {
		return nil, err
	}
	for k, v := range unknown {
		if _, ok := merged[k]; !ok {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

// UnmarshalJSON decodes an item and retains any fields this build does not
// know so a later MarshalJSON round-trips them verbatim.
func (it *Item) UnmarshalJSON(data []byte) error {
	var known itemAlias
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	unknown, err := unknownJSONFields(data, itemKnownKeys())
	if err != nil {
		return err
	}
	*it = Item(known)
	it.unknown = unknown
	return nil
}

// MarshalJSON encodes the item's known fields and re-attaches preserved
// unknown fields from newer schemas.
func (it Item) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(itemAlias(it))
	if err != nil {
		return nil, err
	}
	return mergeUnknownJSONFields(encoded, it.unknown)
}

// UnmarshalJSON decodes the snapshot and retains unknown top-level keys.
func (s *State) UnmarshalJSON(data []byte) error {
	var known stateAlias
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	unknown, err := unknownJSONFields(data, stateKnownKeys())
	if err != nil {
		return err
	}
	*s = State(known)
	s.unknown = unknown
	return nil
}

// MarshalJSON encodes the snapshot and re-attaches preserved top-level keys.
func (s State) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(stateAlias(s))
	if err != nil {
		return nil, err
	}
	return mergeUnknownJSONFields(encoded, s.unknown)
}

// SortState orders items deterministically inside each queue bucket.
func SortState(state *State) {
	sort.SliceStable(state.Pending, func(i, j int) bool {
		if !state.Pending[i].DeliverAfter.Equal(state.Pending[j].DeliverAfter) {
			return state.Pending[i].DeliverAfter.Before(state.Pending[j].DeliverAfter)
		}
		if !state.Pending[i].CreatedAt.Equal(state.Pending[j].CreatedAt) {
			return state.Pending[i].CreatedAt.Before(state.Pending[j].CreatedAt)
		}
		return state.Pending[i].ID < state.Pending[j].ID
	})
	sort.SliceStable(state.InFlight, func(i, j int) bool {
		if !state.InFlight[i].LeaseUntil.Equal(state.InFlight[j].LeaseUntil) {
			return state.InFlight[i].LeaseUntil.Before(state.InFlight[j].LeaseUntil)
		}
		if !state.InFlight[i].ClaimedAt.Equal(state.InFlight[j].ClaimedAt) {
			return state.InFlight[i].ClaimedAt.Before(state.InFlight[j].ClaimedAt)
		}
		return state.InFlight[i].ID < state.InFlight[j].ID
	})
	sort.SliceStable(state.Dead, func(i, j int) bool {
		if !state.Dead[i].DeadAt.Equal(state.Dead[j].DeadAt) {
			return state.Dead[i].DeadAt.Before(state.Dead[j].DeadAt)
		}
		if !state.Dead[i].CreatedAt.Equal(state.Dead[j].CreatedAt) {
			return state.Dead[i].CreatedAt.Before(state.Dead[j].CreatedAt)
		}
		return state.Dead[i].ID < state.Dead[j].ID
	})
}

// WithState locks, loads, mutates, and atomically rewrites the queue state.
func WithState(cityPath string, fn func(*State) error) error {
	dir := filepath.Dir(StatePath(cityPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating nudge queue dir: %w", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening nudge queue lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking nudge queue: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	state, err := LoadState(cityPath)
	if err != nil {
		return err
	}
	if err := fn(&state); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal nudge queue: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, StatePath(cityPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write nudge queue: %w", err)
	}
	return nil
}

// LoadState reads the persisted queue state from disk.
func LoadState(cityPath string) (State, error) {
	data, err := os.ReadFile(StatePath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read nudge queue: %w", err)
	}
	if len(data) == 0 {
		return State{}, nil
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse nudge queue: %w", err)
	}
	SortState(&state)
	return state, nil
}

// StatePath returns the persisted queue state path for a city.
func StatePath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "state.json")
}

// LockPath returns the queue state lock path for a city.
func LockPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "state.lock")
}

// WakeSocketPath returns the path to the supervisor nudge-dispatcher wake
// socket. Producers connect to this path after enqueue to trigger immediate
// dispatch; the supervisor listens on it when daemon.nudge_dispatcher is
// "supervisor".
//
// Preserves the legacy `<city>/.gc/runtime/nudges/wake.sock` location for
// short city paths but falls back to a deterministic short temp-path
// when the legacy pathname is too close to the platform sockaddr_un
// limit. Mirrors the controllerSocketPath pattern in cmd/gc/controller.go.
func WakeSocketPath(cityPath string) string {
	legacy := citylayout.RuntimePath(cityPath, "nudges", "wake.sock")
	if len(legacy) <= wakeSocketPathLimit {
		return legacy
	}
	canonical, err := filepath.Abs(cityPath)
	if err != nil {
		canonical = cityPath
	}
	canonical = filepath.Clean(canonical)
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join("/tmp", "gascity-nudge", fmt.Sprintf("%x.sock", sum[:16]))
}
