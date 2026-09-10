package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreSetMetadataBatchRetriesLockWaitTimeout pins that a MySQL
// 1205 (lock wait timeout) from the checked write is retried from a fresh
// read like a 1213. The pinned library retries both inside UpdateIssueChecked
// for permanent rows, but routes wisps through a bare transaction with no
// retry of its own, so on the server backend a 1205 on a wisp reaches this
// classifier — and an unclassified 1205 failed the stamp immediately.
func TestNativeDoltStoreSetMetadataBatchRetriesLockWaitTimeout(t *testing.T) {
	getCalls := 0
	checkedCalls := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			getCalls++
			return &beadslib.Issue{ID: "gc-wisp", Metadata: json.RawMessage(`{"existing":"kept"}`), RowVersion: int64(getCalls)}, nil
		},
		updateIssueChecked: func(context.Context, string, map[string]interface{}, string, beadslib.UpdateIssueOptions) error {
			checkedCalls++
			if checkedCalls == 1 {
				return errors.New("Error 1205 (HY000): Lock wait timeout exceeded; try restarting transaction")
			}
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.SetMetadataBatch("gc-wisp", map[string]string{"requested": "written"}); err != nil {
		t.Fatalf("SetMetadataBatch after one lock-wait timeout: %v, want nil (retried from a fresh read)", err)
	}
	if getCalls != 2 || checkedCalls != 2 {
		t.Fatalf("calls = GetIssue:%d UpdateIssueChecked:%d, want 2 each (one timeout, one retry)", getCalls, checkedCalls)
	}
}

func TestNativeDoltSerializationConflictClassifiesLockWaitTimeout(t *testing.T) {
	if !isNativeDoltSerializationConflict(errors.New("Error 1205 (HY000): Lock wait timeout exceeded; try restarting transaction")) {
		t.Fatal("MySQL 1205 (lock wait timeout) not classified as a retryable serialization conflict")
	}
}
