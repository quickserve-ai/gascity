package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/mail"
)

// ga-0ejdbv review finding 1: the API returned the same bare 500 for a lost
// write and an unconfirmed one, so a client could not tell "re-send" from
// "check first". The unconfirmed error must name the message ID and warn
// against a blind retry; a lost write must not claim to be unconfirmed.
func TestMailWriteErrorKeepsUnconfirmedDistinct(t *testing.T) {
	unconfirmed := fmt.Errorf("beadmail send: message bead could not be confirmed: %w", &mail.DeliveryUnconfirmedError{ID: "gc-42", Cause: errors.New("i/o timeout")})
	msg := mailWriteError(unconfirmed).Error()
	for _, want := range []string{"mail_unconfirmed", "gc-42", "gc bd show gc-42", "blind retry may send a duplicate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("unconfirmed error %q missing %q", msg, want)
		}
	}
	lost := mailWriteError(errors.New("beadmail send: message bead was not persisted: gc-43")).Error()
	if strings.Contains(lost, "mail_unconfirmed") {
		t.Errorf("a lost write reported as unconfirmed: %q", lost)
	}
}
