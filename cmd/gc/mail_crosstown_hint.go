package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
)

// crossTownRecipientHint explains a LOCAL send refusal whose recipient looks
// like a seat in another town (ga-5toacq). It returns "" when the recipient does
// not look foreign.
//
// Local mail never crosses a town boundary. Before this hint, the only thing the
// sender saw was "session not found", which reads as a typo, so the sender
// stopped and the message was lost with no record on either side. The hint names
// the hub leg and the context names this client actually holds, so the next
// command is copyable.
//
// It runs only on the path that has already refused. It changes no resolution.
func crossTownRecipientHint(cfg *config.City, recipient, contextsPath string) string {
	// Without a loaded config nothing is known about this city's rigs or dirs,
	// so "not this city" cannot be claimed. Say nothing rather than misdirect.
	if cfg == nil {
		return ""
	}
	cityName := cfg.EffectiveCityName()
	prefix, foreign := mail.ForeignTownPrefix(recipient, cityName, cfg.LocalAddressPrefixes())
	if !foreign {
		return ""
	}
	names := clientContextNames(contextsPath)
	if len(names) == 0 {
		names = []string{"<hub context>"}
	}
	here := "this city"
	if cityName != "" {
		here = fmt.Sprintf("this city (%s)", cityName)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gc mail send: %q is not %s or one of its rigs. If it is a seat in another town, "+
		"local mail cannot reach it: cross-town agent mail goes through the hub, addressed to a mailbox "+
		"that town accepts (its cross-town routing address, with a \"[for <rig>/<name>]\" subject). "+
		"One command per context this client holds:", prefix, here)
	// One complete command per line. Joining the names with "|" would read as a
	// choice but paste as a shell pipeline.
	for _, name := range names {
		fmt.Fprintf(&b, "\n  gc mail send --context %s <%s routing address> -s \"[for <rig>/<name>] subject\" -m \"body\"", name, prefix)
	}
	return b.String()
}

// clientContextNames lists the named remote cities in the contexts registry.
// A missing or unreadable registry yields none; the hint then prints a
// placeholder rather than failing the refusal it is explaining.
func clientContextNames(path string) []string {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	file, err := clientcontext.Load(path)
	if err != nil || file == nil {
		return nil
	}
	names := make([]string, 0, len(file.Contexts))
	for _, c := range file.Contexts {
		if name := strings.TrimSpace(c.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}
