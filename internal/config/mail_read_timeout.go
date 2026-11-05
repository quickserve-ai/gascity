package config

import (
	"fmt"
	"strings"
	"time"
)

// Mail read timing (pl-lzd). A mail read has two clocks: the client's budget
// for the whole request, and the server's deadline for the store read behind
// it. The server's deadline is derived from the client's budget, a fixed
// margin shorter, so the server's typed store_slow answer reaches the client
// before the client gives up on the transport. One key, [mail] read_timeout,
// sets the budget; the deadline follows it.
const (
	// DefaultMailReadTimeout is the client's budget for one mail read when
	// [mail] read_timeout is unset.
	DefaultMailReadTimeout = 30 * time.Second
	// MailReadDeadlineMargin is how much sooner than the client's budget the
	// server gives up on the store, leaving time for its store_slow problem
	// detail to travel back.
	MailReadDeadlineMargin = 5 * time.Second
	// MailReadTimeoutCeiling is the largest accepted [mail] read_timeout. It
	// equals the API client's overall request timeout, which would cut a
	// longer mail read before either mail clock fired.
	MailReadTimeoutCeiling = 60 * time.Second
)

// EffectiveReadTimeout returns the client's budget for one mail read: the
// [mail] read_timeout value, or DefaultMailReadTimeout when it is unset.
// Config load refuses an invalid value (ValidateMailReadTimeout), so the
// default fallback here only covers a config that was never validated.
func (m MailConfig) EffectiveReadTimeout() time.Duration {
	raw := strings.TrimSpace(m.ReadTimeout)
	if raw == "" {
		return DefaultMailReadTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= MailReadDeadlineMargin || d > MailReadTimeoutCeiling {
		return DefaultMailReadTimeout
	}
	return d
}

// ReadDeadline returns the server's deadline for a mail store read: the
// client's budget minus MailReadDeadlineMargin. It is always positive and
// always shorter than EffectiveReadTimeout.
func (m MailConfig) ReadDeadline() time.Duration {
	return m.EffectiveReadTimeout() - MailReadDeadlineMargin
}

// ValidateMailReadTimeout refuses a [mail] read_timeout that does not parse,
// that leaves the server no deadline (at or under the margin), or that exceeds
// the API client's request ceiling. An empty value is valid and means the
// default.
func ValidateMailReadTimeout(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	raw := strings.TrimSpace(cfg.Mail.ReadTimeout)
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%s: [mail] read_timeout = %q is not a valid duration: %w", source, raw, err)
	}
	if d <= MailReadDeadlineMargin {
		return fmt.Errorf("%s: [mail] read_timeout = %q leaves the server no read deadline: it must exceed %s, the margin the server keeps so its store_slow answer arrives before the client gives up",
			source, raw, MailReadDeadlineMargin)
	}
	if d > MailReadTimeoutCeiling {
		return fmt.Errorf("%s: [mail] read_timeout = %q exceeds %s, the API client's overall request timeout, which would cut the read before the mail clocks fire",
			source, raw, MailReadTimeoutCeiling)
	}
	return nil
}
