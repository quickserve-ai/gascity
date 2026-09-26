package mail

import "errors"

// DeliveryUnconfirmedError names a message whose write the store reported as
// successful but whose persistence could not be read back either way: the
// verification lookup itself did not complete. The message may well have
// landed. It carries the message ID so a caller can tell the operator exactly
// what to check, and so a retrying caller can avoid sending a duplicate
// (ga-0ejdbv). Providers wrap it under their own sentinel.
type DeliveryUnconfirmedError struct {
	ID    string
	Cause error
}

func (e *DeliveryUnconfirmedError) Error() string {
	if e.Cause == nil {
		return e.ID
	}
	return e.ID + ": " + e.Cause.Error()
}

func (e *DeliveryUnconfirmedError) Unwrap() error { return e.Cause }

// UnconfirmedMessageID returns the ID of a message whose delivery err reports
// as unconfirmed, and false for any other error (including an unconfirmed
// write that returned no ID at all).
func UnconfirmedMessageID(err error) (string, bool) {
	var u *DeliveryUnconfirmedError
	if errors.As(err, &u) && u.ID != "" {
		return u.ID, true
	}
	return "", false
}
