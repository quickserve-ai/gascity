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
	cityName := ""
	var rigNames []string
	if cfg != nil {
		cityName = cfg.EffectiveCityName()
		for _, rig := range cfg.Rigs {
			rigNames = append(rigNames, rig.Name)
		}
	}
	prefix, foreign := mail.ForeignTownPrefix(recipient, cityName, rigNames)
	if !foreign {
		return ""
	}
	ctx := "<hub context>"
	if names := clientContextNames(contextsPath); len(names) > 0 {
		ctx = strings.Join(names, "|")
	}
	here := "this city"
	if cityName != "" {
		here = fmt.Sprintf("this city (%s)", cityName)
	}
	return fmt.Sprintf("gc mail send: %q is not %s or one of its rigs. If it is a seat in another town, "+
		"local mail cannot reach it: cross-town agent mail goes through the hub, e.g. "+
		"gc mail send --context %s <%s's mayor anchor> -s \"[for <rig>/<name>] subject\" -m \"body\" "+
		"(see the cross-town-coordination skill for the anchor names).",
		prefix, here, ctx, prefix)
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
