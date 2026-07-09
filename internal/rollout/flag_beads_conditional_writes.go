package rollout

import "github.com/gastownhall/gascity/internal/config"

// keyBeadsConditionalWrites is the registry Key for the beads CAS rollout gate.
const keyBeadsConditionalWrites = "beads.conditional_writes"

// envBeadsConditionalWrites is the single source of truth for this gate's env
// override name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it, so the three can
// never drift into a silent break-glass no-op.
const envBeadsConditionalWrites = "GC_BEADS_CONDITIONAL_WRITES"

// BeadsConditionalWrites returns the resolved beads.conditional_writes mode.
func (f Flags) BeadsConditionalWrites() Mode {
	return f.beadsConditionalWrites.value
}

// WithBeadsConditionalWrites overrides beads.conditional_writes on a ForTest
// Flags value.
func WithBeadsConditionalWrites(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.beadsConditionalWrites = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readBeadsConditionalWrites returns the raw config spelling for the gate and
// whether the merged config set it (empty string = unset, since the field is
// omitempty).
func readBeadsConditionalWrites(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Beads.ConditionalWrites
	return raw, raw != ""
}
