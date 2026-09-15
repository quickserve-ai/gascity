package builtin

import "testing"

// TestBuiltinClaudeModelChoicesIncludeOpus5 is the falsifiable floor for
// ra-jbbv0 / ra-4cq5w: the builtin claude provider's "model" select is a
// closed enum, and a value outside it yields no FlagArgs — so gc silently
// emits no --model flag at all rather than erroring, and 'gc config show'
// keeps reporting the pin while the launched process runs the provider
// default model. claude-sonnet-5 (#3867) and claude-fable-5 (#3284) were
// added to this enum; claude-opus-5 was not.
func TestBuiltinClaudeModelChoicesIncludeOpus5(t *testing.T) {
	claude, ok := BuiltinProviders()["claude"]
	if !ok {
		t.Fatal("BuiltinProviders() missing claude")
	}

	var modelOption BuiltinProviderOption
	for _, option := range claude.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	if modelOption.Key == "" {
		t.Fatal("claude provider missing model option")
	}

	byValue := make(map[string]BuiltinOptionChoice, len(modelOption.Choices))
	for _, choice := range modelOption.Choices {
		byValue[choice.Value] = choice
	}

	choice, ok := byValue["opus-5"]
	if !ok {
		t.Fatal("claude model choices missing \"opus-5\" (claude-opus-5 has no enum entry, " +
			"so resolving it yields no --model FlagArgs and gc silently launches the provider default)")
	}
	// Carried divergence (CARRY.md, "Claude model enum"): the fleet launches
	// opus-5 at the 1M window and bare "opus" through the generation-tracking
	// opus[1m] alias, where upstream emits claude-opus-5 and pins
	// "opus" to claude-opus-4-8. Drop when every city states these choices in
	// its provider config (options_schema_merge = "by_key").
	wantFlagArgs := []string{"--model", "claude-opus-5[1m]"}
	if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != wantFlagArgs[0] || choice.FlagArgs[1] != wantFlagArgs[1] {
		t.Errorf("opus-5 FlagArgs = %v, want %v", choice.FlagArgs, wantFlagArgs)
	}
	if len(choice.FlagAliases) != 1 || len(choice.FlagAliases[0]) != 2 ||
		choice.FlagAliases[0][0] != "-m" || choice.FlagAliases[0][1] != "claude-opus-5[1m]" {
		t.Errorf("opus-5 FlagAliases = %v, want [[-m claude-opus-5[1m]]]", choice.FlagAliases)
	}

	bare, ok := byValue["opus"]
	if !ok {
		t.Fatal("claude model choices missing \"opus\"")
	}
	if len(bare.FlagArgs) != 2 || bare.FlagArgs[1] != "opus[1m]" {
		t.Errorf("opus (bare) FlagArgs = %v, want [--model opus[1m]] (carried generation-tracking alias)", bare.FlagArgs)
	}
}

// TestBuiltinClaudeModelChoicesAcceptCanonicalIDsVerbatim is the second half
// of ra-jbbv0's root cause: operators pin the full provider model ID
// ("claude-opus-5", not the short alias "opus-5") in agent.toml. The incident
// showed loial/egwene/siuan/perrin pinned to exactly "claude-opus-5" and
// moiraine to "claude-opus-5[1m]" — none of which were enum values, so the
// named-session resolution path hard-errored ("invalid value for model:
// claude-opus-5") while the launch path silently dropped --model instead.
// Neither #3867 (Sonnet 5) nor #3284 (Fable 5) added the canonical-id form as
// an accepted value — only the short alias — so this gap predates and is
// broader than Opus 5 alone.
func TestBuiltinClaudeModelChoicesAcceptCanonicalIDsVerbatim(t *testing.T) {
	claude, ok := BuiltinProviders()["claude"]
	if !ok {
		t.Fatal("BuiltinProviders() missing claude")
	}
	var modelOption BuiltinProviderOption
	for _, option := range claude.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	byValue := make(map[string]BuiltinOptionChoice, len(modelOption.Choices))
	for _, choice := range modelOption.Choices {
		byValue[choice.Value] = choice
	}

	for _, canonical := range []string{"claude-opus-5", "claude-opus-5[1m]", "claude-sonnet-5", "claude-fable-5"} {
		choice, ok := byValue[canonical]
		if !ok {
			t.Errorf("claude model choices missing canonical id %q as a directly-accepted value", canonical)
			continue
		}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != "--model" || choice.FlagArgs[1] != canonical {
			t.Errorf("%s FlagArgs = %v, want [--model %s]", canonical, choice.FlagArgs, canonical)
		}
	}
}
