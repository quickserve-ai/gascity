package config

import "strings"

// SessionNameOptionKey is the schema key for the harness session display
// name. For claude-family providers the resolved schema carries this option
// so every composed launch command includes `--name <gc address>` — the
// harness shows it in the prompt box, the /resume picker, the terminal
// title, and the cross-session agent listing, which makes picker rows and
// peer listings identify the owning seat by its verbatim gc address
// (ga-n0rvsk).
const SessionNameOptionKey = "name"

// sessionNameFlag is the claude CLI flag the option renders to.
const sessionNameFlag = "--name"

// injectSessionNameOption defaults a claude-family resolved provider's
// session display name to the agent's qualified identity. No-ops for other
// families (their CLIs reject the flag) and for escape-hatch resolutions
// whose OptionsSchema was cleared (start_command owns the full command
// line). An explicit agent option_defaults["name"] wins over the derived
// identity; either way the value is synthesized into the schema because the
// options machinery is select-only (v1) — validation is an exact-match
// choice lookup, so a per-seat value can only pass by carrying its own
// choice (see EnsureSessionNameOption).
func injectSessionNameOption(resolved *ResolvedProvider, agent *Agent) {
	if resolved == nil || agent == nil || len(resolved.OptionsSchema) == 0 {
		return
	}
	family := strings.TrimSpace(resolved.BuiltinAncestor)
	if family == "" {
		family = strings.TrimSpace(resolved.Name)
	}
	if family != "claude" {
		return
	}
	value := ""
	if agent.OptionDefaults != nil {
		value = strings.TrimSpace(agent.OptionDefaults[SessionNameOptionKey])
	}
	if value == "" {
		value = strings.TrimSpace(agent.QualifiedName())
	}
	if value == "" {
		return
	}
	EnsureSessionNameOption(resolved, value)
}

// EnsureSessionNameOption makes value the resolved provider's effective
// session name: it guarantees the schema has a "name" option carrying a
// choice for value (appending one when missing) and sets the effective
// default. Exported for start-prep overlays that name a per-instance
// identity (pool siblings) after base resolution.
//
// The OptionsSchema slice (and the name option's Choices) are cloned before
// mutation so a resolved provider shared across pool siblings is never
// aliased — a per-instance name must not leak onto its siblings' schema.
func EnsureSessionNameOption(resolved *ResolvedProvider, value string) {
	value = strings.TrimSpace(value)
	if resolved == nil || value == "" {
		return
	}
	schema := make([]ProviderOption, len(resolved.OptionsSchema))
	copy(schema, resolved.OptionsSchema)
	idx := -1
	for i := range schema {
		if schema[i].Key == SessionNameOptionKey {
			idx = i
			break
		}
	}
	if idx == -1 {
		schema = append(schema, ProviderOption{
			Key:   SessionNameOptionKey,
			Label: "Session name",
			Type:  "select",
		})
		idx = len(schema) - 1
	}
	opt := &schema[idx]
	choices := make([]OptionChoice, len(opt.Choices))
	copy(choices, opt.Choices)
	found := false
	for _, c := range choices {
		if c.Value == value {
			found = true
			break
		}
	}
	if !found {
		choices = append(choices, OptionChoice{
			Value:    value,
			Label:    value,
			FlagArgs: []string{sessionNameFlag, value},
		})
	}
	opt.Choices = choices
	resolved.OptionsSchema = schema

	defaults := make(map[string]string, len(resolved.EffectiveDefaults)+1)
	for k, v := range resolved.EffectiveDefaults {
		defaults[k] = v
	}
	defaults[SessionNameOptionKey] = value
	resolved.EffectiveDefaults = defaults
}
