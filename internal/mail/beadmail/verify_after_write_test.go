package beadmail

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// verifyStore drives the read-after-write guard: Create delegates to the real
// store, while Get replays a scripted sequence of errors (the last entry
// repeats) so a test can stage loss, an incomplete lookup, or read lag.
type verifyStore struct {
	beads.Store
	getErrs     []error
	calls       int
	verifyCalls int
	created     map[string]bool
}

func (s *verifyStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err == nil {
		if s.created == nil {
			s.created = map[string]bool{}
		}
		s.created[created.ID] = true
	}
	return created, err
}

func (s *verifyStore) Get(id string) (beads.Bead, error) {
	s.calls++
	// Only the verification read of a bead this store just created is scripted.
	// Reply and the handoff paths read OTHER beads first (the message being
	// replied to), and failing those would test the wrong thing.
	if !s.created[id] {
		return s.Store.Get(id)
	}
	i := s.verifyCalls
	s.verifyCalls++
	if len(s.getErrs) > 0 {
		if i >= len(s.getErrs) {
			i = len(s.getErrs) - 1
		}
		if err := s.getErrs[i]; err != nil {
			return beads.Bead{}, err
		}
	}
	return s.Store.Get(id)
}

// fastVerify shrinks the retry backoff so these tests do not sleep.
func fastVerify(t *testing.T) {
	t.Helper()
	saved := messageVerifyBackoff
	messageVerifyBackoff = []time.Duration{time.Microsecond, time.Microsecond}
	t.Cleanup(func() { messageVerifyBackoff = saved })
}

// TestSendFailsLoudlyWhenMessageBeadIsVerifiedAbsent is the bug this guard
// exists for: the store reports a successful create, the row is not there, and
// the old code returned success all the way up to "Sent message <id>" + exit 0.
func TestSendFailsLoudlyWhenMessageBeadIsVerifiedAbsent(t *testing.T) {
	fastVerify(t)
	store := &verifyStore{
		Store:   beads.NewMemStore(),
		getErrs: []error{beads.ErrNotFound},
	}
	p := New(store)

	_, err := p.Send("woodhouse", "katya", "subject", "body")
	if err == nil {
		t.Fatal("Send returned nil error for a message bead that does not exist — this is the silent loss (ga-0ejdbv)")
	}
	if !errors.Is(err, ErrNotPersisted) {
		t.Errorf("err = %v, want ErrNotPersisted", err)
	}
	if errors.Is(err, ErrUnconfirmed) {
		t.Errorf("err = %v, must not also read as unconfirmed — the lookup completed and the row is absent", err)
	}
}

// TestSendReportsUnconfirmedRatherThanLostWhenVerificationCannotComplete is the
// negative control for the outage shape. Under load the wisp verification query
// is exactly what times out. If an incomplete lookup were reported as loss,
// this guard would fail healthy sends fleet-wide at the moment it matters most
// — trading silent loss for a confident false failure.
func TestSendReportsUnconfirmedRatherThanLostWhenVerificationCannotComplete(t *testing.T) {
	fastVerify(t)
	store := &verifyStore{
		Store:   beads.NewMemStore(),
		getErrs: []error{beads.ErrVerifyIndeterminate},
	}
	p := New(store)

	_, err := p.Send("woodhouse", "katya", "subject", "body")
	if !errors.Is(err, ErrUnconfirmed) {
		t.Errorf("err = %v, want ErrUnconfirmed", err)
	}
	if errors.Is(err, ErrNotPersisted) {
		t.Errorf("err = %v, must NOT claim the message was lost — the lookup never completed", err)
	}
}

// TestSendToleratesReadVisibilityLag proves the bounded retry does its job: a
// backing that is briefly behind is not a lost write, and must not fail a send
// that actually landed.
func TestSendToleratesReadVisibilityLag(t *testing.T) {
	fastVerify(t)
	store := &verifyStore{
		Store:   beads.NewMemStore(),
		getErrs: []error{beads.ErrNotFound, nil},
	}
	p := New(store)

	m, err := p.Send("woodhouse", "katya", "subject", "body")
	if err != nil {
		t.Fatalf("Send failed on a message that landed one read late: %v", err)
	}
	if m.ID == "" {
		t.Error("Send returned an empty message ID")
	}
	if store.verifyCalls < 2 {
		t.Errorf("verification read ran %d times, want at least 2 — the retry did not happen", store.verifyCalls)
	}
}

// TestSendSucceedsWhenVerificationSeesTheRow pins the happy path: one
// verification read, no retries, no behaviour change for a healthy send.
func TestSendSucceedsWhenVerificationSeesTheRow(t *testing.T) {
	fastVerify(t)
	baseline := &verifyStore{Store: beads.NewMemStore()}
	t.Setenv("GC_MAIL_VERIFY", "0")
	if _, err := New(baseline).Send("woodhouse", "katya", "subject", "body"); err != nil {
		t.Fatalf("baseline send: %v", err)
	}
	t.Setenv("GC_MAIL_VERIFY", "")

	store := &verifyStore{Store: beads.NewMemStore()}
	p := New(store)

	m, err := p.Send("woodhouse", "katya", "subject", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m.ID == "" {
		t.Error("Send returned an empty message ID")
	}
	// The guard's cost is measured against the same path with it disabled, not
	// asserted from reading the code: exactly one additional read.
	if want := baseline.calls + 1; store.calls != want {
		t.Errorf("Get called %d times, want %d (baseline %d + one verification read)", store.calls, want, baseline.calls)
	}
}

// TestReplyAndHandoffAreGuardedToo pins that the guard covers every
// mail-creating path, because they all funnel through createMessageBead.
func TestReplyAndHandoffAreGuardedToo(t *testing.T) {
	fastVerify(t)
	base := beads.NewMemStore()
	seed := New(base)
	original, err := seed.Send("katya", "woodhouse", "original", "body")
	if err != nil {
		t.Fatalf("seed send: %v", err)
	}

	store := &verifyStore{Store: base, getErrs: []error{beads.ErrNotFound}}
	p := New(store)
	if _, err := p.Reply(original.ID, "woodhouse", "re", "body"); !errors.Is(err, ErrNotPersisted) {
		t.Errorf("Reply err = %v, want ErrNotPersisted", err)
	}
}

// lostWriteStore reports every Create as a success but persists nothing, which
// is the exact shape of the bug: the store says it wrote, the row is not there.
type lostWriteStore struct {
	beads.Store
}

func (s lostWriteStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err != nil {
		return created, err
	}
	// Persist nothing — hand back only what the caller would have believed.
	_ = s.Store.Delete(created.ID)
	return created, nil
}

// TestSendCatchesLostWriteThroughCachingStore is the controller/API plane case.
// CachingStore.Create absorbs the unverified bead into its cache with
// clearDirty, so a verification read through the store's OWN Get is answered by
// that cache and confirms a write that never reached storage. The guard must
// read through the LIVE handle instead. Found by an independent cross-family
// review of the first cut of this fix.
func TestSendCatchesLostWriteThroughCachingStore(t *testing.T) {
	fastVerify(t)
	backing := lostWriteStore{Store: beads.NewMemStore()}
	cached := beads.NewCachingStoreForTest(backing, nil)
	p := New(cached)

	_, err := p.Send("woodhouse", "katya", "subject", "body")
	if err == nil {
		t.Fatal("Send reported success for a lost write — the cache confirmed a row that does not exist")
	}
	if !errors.Is(err, ErrNotPersisted) {
		t.Errorf("err = %v, want ErrNotPersisted", err)
	}
}

// TestSendRejectsCreateWithNoID pins that a create which reports success but
// hands back no ID cannot pass verification by default. An unverifiable write
// is not a verified one.
func TestSendRejectsCreateWithNoID(t *testing.T) {
	fastVerify(t)
	p := New(noIDStore{Store: beads.NewMemStore()})

	if _, err := p.Send("woodhouse", "katya", "subject", "body"); !errors.Is(err, ErrUnconfirmed) {
		t.Errorf("err = %v, want ErrUnconfirmed", err)
	}
}

type noIDStore struct {
	beads.Store
}

func (s noIDStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	created.ID = ""
	return created, err
}
