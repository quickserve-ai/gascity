//go:build productmetrics_testhook && ((linux && !android) || (darwin && !ios))

package main

import (
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/productmetrics"
	"github.com/gastownhall/gascity/internal/testutil"
)

// TestProductMetricsTesthookRecordLockWaitFollowsFrozenClock pins the
// mechanism behind the "enabled metrics status" flake in
// TestProductMetricsTaggedBinaryProcessContracts (pl-7sm): the tagged binary
// freezes its clock, so RecordOnce's decision window never runs out, but the
// state-lock wait inside that window used a live 50 ms wall-clock deadline.
// Any real delay of 50 ms or more between building that deadline and winning
// flock dropped the "gc help" record, and the status then showed an empty
// queue. Holding state.lock is the control's way to inject that real delay;
// on a loaded CI runner the delay came from scheduling and file-system
// latency instead.
func TestProductMetricsTesthookRecordLockWaitFollowsFrozenClock(t *testing.T) {
	configureProductMetricsTrustedProcessTempRoot(t)

	t.Run("production lock deadline drops the record under a frozen clock", func(t *testing.T) {
		home := productMetricsTesthookLockHome(t)
		options := productMetricsTesthookLockOptions()
		options.WithDeadline = nil
		service, permit := openProductMetricsTesthookLockService(t, options)
		release := holdProductMetricsStateLock(t, home)
		defer release()

		if got := service.RecordOnce(permit, productmetrics.CommandHelp); got != productmetrics.RecordDropped {
			t.Fatalf("RecordOnce with the production lock deadline = %v, want dropped once the real 50 ms lock wait expires", got)
		}
	})

	t.Run("tagged options keep the record past the production budget", func(t *testing.T) {
		home := productMetricsTesthookLockHome(t)
		options := productMetricsTesthookLockOptions()
		tagged := options.WithDeadline
		budgets := make(chan time.Duration, 1)
		options.WithDeadline = func(parent context.Context, remaining time.Duration) (context.Context, context.CancelFunc) {
			budgets <- remaining
			return tagged(parent, remaining)
		}
		service, permit := openProductMetricsTesthookLockService(t, options)
		release := holdProductMetricsStateLock(t, home)
		defer release()

		results := make(chan productmetrics.RecordResult, 1)
		go func() { results <- service.RecordOnce(permit, productmetrics.CommandHelp) }()

		var budget time.Duration
		select {
		case budget = <-budgets:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("RecordOnce never built its state-lock deadline")
		}
		// Hold the lock until the production deadline RecordOnce asked for
		// has fired in real time, then prove the wait outlived it.
		production, cancel := context.WithTimeout(context.Background(), budget)
		<-production.Done()
		cancel()
		select {
		case got := <-results:
			t.Fatalf("RecordOnce returned %v while state.lock was still held past its %v budget, want it still waiting", got, budget)
		default:
		}
		release()

		select {
		case got := <-results:
			if got != productmetrics.RecordStored {
				t.Fatalf("RecordOnce with the tagged lock deadline = %v, want stored", got)
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("RecordOnce did not finish after state.lock was released")
		}
		if events := service.Status(context.Background()).QueueEvents; events != 2 {
			t.Fatalf("queued events = %d, want the seeded event plus the new help record", events)
		}
	})
}

func productMetricsTesthookLockHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	seedPrivateUploaderProcessFixture(t, home, "6ba7b810-9dad-41d1-80b4-00c04fd430c8", time.Now().UTC())
	return home
}

func productMetricsTesthookLockOptions() productmetrics.TesthookOptions {
	options := productMetricsTesthookOptions("https://127.0.0.1:1/v1/command-usage", x509.NewCertPool())
	options.Home = gchome.ResolveReadOnly()
	return options
}

func openProductMetricsTesthookLockService(t *testing.T, options productmetrics.TesthookOptions) (*productmetrics.Service, productmetrics.RecordingPermit) {
	t.Helper()
	service, err := productmetrics.OpenTesthook(options)
	if err != nil {
		t.Fatal(err)
	}
	permit := service.RecordingPermit(productmetrics.InvocationContext{
		Recordable:      true,
		OccurredHourUTC: options.Now().UTC().Truncate(time.Hour).Format(time.RFC3339),
	})
	if !permit.Valid() {
		t.Fatal("tagged recording permit is invalid")
	}
	t.Cleanup(func() { _ = permit.Close() })
	return service, permit
}

// holdProductMetricsStateLock takes state.lock the way a peer process would
// and returns an idempotent release.
func holdProductMetricsStateLock(t *testing.T, home string) func() {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(home, "product-usage", "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
	t.Cleanup(release)
	return release
}
