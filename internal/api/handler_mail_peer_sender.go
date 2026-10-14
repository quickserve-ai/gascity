package api

import "github.com/gastownhall/gascity/internal/mail"

// peerCityMailSender reports whether from is qualified by a city this city's
// [mail.crosscity] roster names as a peer: "<peer city>/<identity>", the
// sender a peer seat's `gc --context <this city> mail send|reply` carries
// (cmd/gc remoteMailSender).
//
// Carry row "Cross-city gc mail send, reply, inbox and mark-read via --context
// (#5386)", read through PROP-027's roster (gc-cxax). Upstream #7766 sends only
// as an identity this city configures, and no peer seat is one, so without
// this the cross-town hub leg is refused. It fails closed: with no roster, and
// for this city's own name, an unknown city, an identity with no city segment
// or an empty remainder, it is false and upstream's 422 stands. The message
// keeps the sender as the peer wrote it, so a reply resolves back.
//
// Drop when upstream binds a sender to its caller (mc-8e9ci).
func (s *Server) peerCityMailSender(from string) bool {
	kind, _ := s.mailCityRoster().ResolveCityAddress(from)
	return kind == mail.CityAddressForeign
}
