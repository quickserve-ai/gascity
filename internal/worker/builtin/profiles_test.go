package builtin

import (
	"testing"
)

func TestBuiltinProvidersAndOrder(t *testing.T) {
	providers := BuiltinProviders()
	order := BuiltinProviderOrder()

	if len(providers) != 17 {
		t.Fatalf("len(BuiltinProviders()) = %d, want 17", len(providers))
	}
	if len(order) != 17 {
		t.Fatalf("len(BuiltinProviderOrder()) = %d, want 17", len(order))
	}

	for _, name := range order {
		spec, ok := providers[name]
		if !ok {
			t.Fatalf("BuiltinProviders() missing %q", name)
		}
		if spec.Command == "" {
			t.Fatalf("provider %q has empty Command", name)
		}
		if spec.DisplayName == "" {
			t.Fatalf("provider %q has empty DisplayName", name)
		}
	}
}

func TestBuiltinProviderOMPSpecDeclaresStartupReadiness(t *testing.T) {
	spec, ok := BuiltinProviders()["omp"]
	if !ok {
		t.Fatal("BuiltinProviders() missing omp")
	}
	if spec.ReadyPromptPrefix != "" {
		t.Fatalf("omp ReadyPromptPrefix = %q, want empty fixed-delay readiness", spec.ReadyPromptPrefix)
	}
	if got, want := spec.ReadyDelayMs, 8000; got != want {
		t.Fatalf("omp ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestHookEnabledBuiltinProvidersDeclareStartupReadiness(t *testing.T) {
	for name, spec := range BuiltinProviders() {
		if !spec.SupportsHooks {
			continue
		}
		if spec.ReadyPromptPrefix == "" && spec.ReadyDelayMs <= 0 {
			t.Errorf("hook-enabled provider %q has no startup readiness signal", name)
		}
	}
}

func TestBuiltinProviderMimoCodeSpec(t *testing.T) {
	providers := BuiltinProviders()
	spec, ok := providers["mimocode"]
	if !ok {
		t.Fatal("BuiltinProviders() missing mimocode")
	}
	if spec.Command != "mimo" {
		t.Errorf("mimocode Command = %q, want %q", spec.Command, "mimo")
	}
	if spec.DisplayName != "MiMo Code" {
		t.Errorf("mimocode DisplayName = %q, want %q", spec.DisplayName, "MiMo Code")
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--never-ask" {
		t.Errorf("mimocode Args = %v, want [--never-ask]", spec.Args)
	}
	if spec.PromptMode != "flag" || spec.PromptFlag != "--prompt" {
		t.Errorf("mimocode prompt = (%q, %q), want (flag, --prompt)", spec.PromptMode, spec.PromptFlag)
	}
	if !spec.SupportsACP || !spec.SupportsHooks {
		t.Errorf("mimocode SupportsACP=%v SupportsHooks=%v, want both true", spec.SupportsACP, spec.SupportsHooks)
	}
	if spec.ResumeFlag != "--session" || spec.ResumeStyle != "flag" {
		t.Errorf("mimocode resume = (%q, %q), want (--session, flag)", spec.ResumeFlag, spec.ResumeStyle)
	}
	if len(spec.ACPArgs) != 1 || spec.ACPArgs[0] != "acp" {
		t.Errorf("mimocode ACPArgs = %v, want [acp]", spec.ACPArgs)
	}
	if spec.InstructionsFile != "AGENTS.md" {
		t.Errorf("mimocode InstructionsFile = %q, want AGENTS.md", spec.InstructionsFile)
	}

	order := BuiltinProviderOrder()
	opencodeIdx, mimocodeIdx := -1, -1
	for i, name := range order {
		switch name {
		case "opencode":
			opencodeIdx = i
		case "mimocode":
			mimocodeIdx = i
		}
	}
	if mimocodeIdx == -1 {
		t.Fatal("BuiltinProviderOrder() missing mimocode")
	}
	if mimocodeIdx != opencodeIdx+1 {
		t.Errorf("mimocode order index = %d, want immediately after opencode (%d)", mimocodeIdx, opencodeIdx)
	}
}

func TestBuiltinProvidersReturnClonedData(t *testing.T) {
	a := BuiltinProviders()
	b := BuiltinProviders()

	a["claude"] = BuiltinProviderSpec{Command: "mutated"}
	if b["claude"].Command == "mutated" {
		t.Fatal("BuiltinProviders() should return a cloned map")
	}

	claude := a["codex"]
	claude.ProcessNames[0] = "mutated"
	a["codex"] = claude
	if b["codex"].ProcessNames[0] == "mutated" {
		t.Fatal("BuiltinProviders() should clone nested slices")
	}
}

func TestBuiltinCodexModelChoicesUseAvailable53CodexAlias(t *testing.T) {
	unavailable53CodexAlias := "gpt-5.3-" + "codex-spark"
	codex, ok := BuiltinProviders()["codex"]
	if !ok {
		t.Fatal("BuiltinProviders() missing codex")
	}

	var modelOption BuiltinProviderOption
	for _, option := range codex.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	if modelOption.Key == "" {
		t.Fatal("codex provider missing model option")
	}

	var found53 bool
	for _, choice := range modelOption.Choices {
		if choice.Value == unavailable53CodexAlias {
			t.Fatalf("codex model choices include unavailable alias %q", choice.Value)
		}
		if choice.Value == "gpt-5.3-codex" {
			found53 = true
			if got, want := choice.Label, "GPT-5.3 Codex"; got != want {
				t.Fatalf("gpt-5.3-codex label = %q, want %q", got, want)
			}
		}
	}
	if !found53 {
		t.Fatal("codex model choices missing gpt-5.3-codex")
	}
}

func TestBuiltinCodexModelChoicesIncludeGPT56Variants(t *testing.T) {
	codex, ok := BuiltinProviders()["codex"]
	if !ok {
		t.Fatal("BuiltinProviders() missing codex")
	}

	var modelOption BuiltinProviderOption
	for _, option := range codex.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	if modelOption.Key == "" {
		t.Fatal("codex provider missing model option")
	}

	byValue := make(map[string]BuiltinOptionChoice, len(modelOption.Choices))
	for _, choice := range modelOption.Choices {
		byValue[choice.Value] = choice
	}

	wantLabels := map[string]string{
		"gpt-5.6-sol":   "GPT-5.6 Sol",
		"gpt-5.6-terra": "GPT-5.6 Terra",
		"gpt-5.6-luna":  "GPT-5.6 Luna",
	}
	for value, wantLabel := range wantLabels {
		choice, ok := byValue[value]
		if !ok {
			t.Fatalf("codex model choices missing %q", value)
		}
		if choice.Label != wantLabel {
			t.Errorf("%s label = %q, want %q", value, choice.Label, wantLabel)
		}
		wantFlagArgs := []string{"--model", value}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != wantFlagArgs[0] || choice.FlagArgs[1] != wantFlagArgs[1] {
			t.Errorf("%s FlagArgs = %v, want %v", value, choice.FlagArgs, wantFlagArgs)
		}
		if len(choice.FlagAliases) != 1 || len(choice.FlagAliases[0]) != 2 ||
			choice.FlagAliases[0][0] != "-m" || choice.FlagAliases[0][1] != value {
			t.Errorf("%s FlagAliases = %v, want [[-m %s]]", value, choice.FlagAliases, value)
		}
	}
}

func TestBuiltinOMPModelChoicesPinModelWithEffortSuffix(t *testing.T) {
	omp, ok := BuiltinProviders()["omp"]
	if !ok {
		t.Fatal("BuiltinProviders() missing omp")
	}

	var modelOption BuiltinProviderOption
	for _, option := range omp.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	if modelOption.Key == "" {
		t.Fatal("omp provider missing model option")
	}

	byValue := make(map[string]BuiltinOptionChoice, len(modelOption.Choices))
	for _, choice := range modelOption.Choices {
		byValue[choice.Value] = choice
	}

	def, ok := byValue[""]
	if !ok {
		t.Fatal("omp model choices missing the empty Default choice")
	}
	if len(def.FlagArgs) != 0 {
		t.Fatalf("omp Default model choice FlagArgs = %v, want none (omp's own config decides)", def.FlagArgs)
	}

	// Effort rides inside omp's --model string as a :<effort> suffix
	// (verified live on omp 18.1.10, 2026-09-04).
	wantModels := map[string]string{
		"astra-high":   "openai-codex/gpt-6-astra:high",
		"astra-medium": "openai-codex/gpt-6-astra:medium",
		"sol-high":     "openai-codex/gpt-5.6-sol:high",
		"sol-medium":   "openai-codex/gpt-5.6-sol:medium",
	}
	for value, wantModel := range wantModels {
		choice, ok := byValue[value]
		if !ok {
			t.Fatalf("omp model choices missing %q", value)
		}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != "--model" || choice.FlagArgs[1] != wantModel {
			t.Errorf("%s FlagArgs = %v, want [--model %s]", value, choice.FlagArgs, wantModel)
		}
	}

	var thinkingOption BuiltinProviderOption
	for _, option := range omp.OptionsSchema {
		if option.Key == "thinking" {
			thinkingOption = option
			break
		}
	}
	if thinkingOption.Key == "" {
		t.Fatal("omp provider missing thinking option")
	}
	for _, choice := range thinkingOption.Choices {
		if choice.Value == "" {
			continue
		}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != "--thinking" || choice.FlagArgs[1] != choice.Value {
			t.Errorf("thinking %s FlagArgs = %v, want [--thinking %s]", choice.Value, choice.FlagArgs, choice.Value)
		}
	}
}
