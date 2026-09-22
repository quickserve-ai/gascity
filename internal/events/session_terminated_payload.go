package events

// SessionTerminatedPayload is the typed payload for session.terminated events.
// Field names are snake_case to match the rest of the event log, and every
// field the handoff ratio needs is present so a reader never has to join back
// to the session bead to bucket a row.
//
// FACTS ONLY. The row carries no numerator/denominator judgement: the bucket
// rules are the reader's versioned classifier (ga-fbzz9u), so a rule change is
// re-run over all history instead of mixing rule populations mid-series
// (katya, PR #106 finding 5).
//
// EventID is the ending's identity, and (session_id, event_id) is the dedup
// key. One ending retried N times writes N rows with one event_id.
//
// At and RequestedAt are RFC3339. RequestedAt is set only when the ending was
// asked for before it happened (a handoff's intent, a drain request), and the
// gap between the two is the latency the ratio's timer reads.
type SessionTerminatedPayload struct {
	Kind        string `json:"kind"`
	Actor       string `json:"actor,omitempty"`
	Reason      string `json:"reason,omitempty"`
	At          string `json:"at"`
	RequestedAt string `json:"requested_at,omitempty"`
	SessionName string `json:"session_name"`
	SessionID   string `json:"session_id,omitempty"`
	EventID     string `json:"event_id"`
}

// IsEventPayload marks SessionTerminatedPayload as an events.Payload variant.
func (SessionTerminatedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionTerminated, SessionTerminatedPayload{})
}
