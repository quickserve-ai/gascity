package events

// SessionNudgedPayload is the typed payload for session.nudged events: one
// nudge delivered LIVE into a running session (ga-qbc7d2).
//
// Injection into a live session is privileged. It can answer a permission
// prompt or an open dialog (ga-qty5j0), so every live delivery must be
// findable afterwards by target and time. The fields mirror what the queued
// path's nudge:<id> wisp already records, so the two paths answer the same
// forensic question.
//
// SENDER IS PROVENANCE, NOT AUTHORITY. Sender and SenderSession are what the
// sending process reported about itself; nothing authenticates them
// (internal/nudgequeue/state.go, ga-txbsqo).
//
// THE TEXT IS NEVER RECORDED. Nudge bodies can carry anything, and the event
// log is widely readable. TextSHA256 (hex) and TextBytes let an investigator
// match a KNOWN payload against the log without the log holding it. For the
// same reason a failure carries only ErrorClass, never the error text.
type SessionNudgedPayload struct {
	TargetAgent string `json:"target_agent"`
	SessionID   string `json:"session_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	Delivery    string `json:"delivery"`
	Outcome     string `json:"outcome"`
	// ErrorClass is a bounded code for a failed attempt (session_gone,
	// timeout, canceled, error), NEVER the provider's error string: some
	// providers format their argv, nudge text included, into errors.
	ErrorClass    string `json:"error_class,omitempty"`
	Sender        string `json:"sender,omitempty"`
	SenderSession string `json:"sender_session,omitempty"`
	Source        string `json:"source,omitempty"`
	TextBytes     int    `json:"text_bytes"`
	TextSHA256    string `json:"text_sha256"`
}

// IsEventPayload marks SessionNudgedPayload as an events.Payload variant.
func (SessionNudgedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionNudged, SessionNudgedPayload{})
}
