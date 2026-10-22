package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// ga-0ejdbv review finding 1: the API returned the same bare 500 for a lost
// write and an unconfirmed one, so a client could not tell "re-send" from
// "check first". The unconfirmed error must name the message ID and warn
// against a blind retry; a lost write must not claim to be unconfirmed.
func TestMailWriteErrorKeepsUnconfirmedDistinct(t *testing.T) {
	unconfirmed := fmt.Errorf("beadmail send: message bead could not be confirmed: %w", &mail.DeliveryUnconfirmedError{ID: "gc-42", Cause: errors.New("i/o timeout")})
	msg := mailWriteError(unconfirmed).Error()
	for _, want := range []string{"mail_unconfirmed", "gc-42", "GC_NO_API=1 gc mail peek gc-42", "blind retry may send a duplicate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("unconfirmed error %q missing %q", msg, want)
		}
	}
	lost := mailWriteError(errors.New("beadmail send: message bead was not persisted: gc-43")).Error()
	if strings.Contains(lost, "mail_unconfirmed") {
		t.Errorf("a lost write reported as unconfirmed: %q", lost)
	}
}

// ga-th31cy: a write VERIFIED absent carries the mail-not-persisted code, so a
// remote client can give the local verdict (exit 5, re-send). An error that is
// not the sentinel keeps the generic internal code.
func TestMailWriteErrorCodesAVerifiedLostWrite(t *testing.T) {
	var lost *apierr.ErrorModel
	if !errors.As(mailWriteError(fmt.Errorf("beadmail send: %w: gc-43", beadmail.ErrNotPersisted)), &lost) {
		t.Fatalf("lost write is not an *apierr.ErrorModel")
	}
	if lost.Code != apierr.MailNotPersisted.Code || !strings.Contains(lost.Error(), "gc-43") {
		t.Errorf("lost write code=%q err=%q, want code %q naming gc-43", lost.Code, lost.Error(), apierr.MailNotPersisted.Code)
	}
	var plain *apierr.ErrorModel
	if !errors.As(mailWriteError(errors.New("dolt: connection refused")), &plain) {
		t.Fatalf("server fault is not an *apierr.ErrorModel")
	}
	if plain.Code == apierr.MailNotPersisted.Code {
		t.Errorf("a server fault was coded as a lost write: %+v", plain)
	}
}
