package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

// State-label inheritance guard for `gc bd create --parent` — a gc-side
// INTERIM (qc-p9m8oa9).
//
// bd create copies every parent label onto the child unless told
// --no-inherit-labels. Dimensional labels (town:*, init:*, ready:*) are meant
// to cascade; hold:*, cert:* and needs-summon are not — they describe the
// parent's situation, and automation reads them (the cert-landing patrol
// treats hold:cert-wait as in flight), so an inheriting child is silently
// skipped forever. See internal/beadmeta/create_inherited_state_labels.go.
//
// REMOVAL CONDITION: remove this file, its call in doBd, and the BdStore half
// once the bd-side exclusion ships in the fleet beads pin. The bd fix is the
// beads owner's; this only covers creates that pass through gc.
//
// Mechanism: BEFORE the create, as one bd invocation. gc reads the parent's
// labels from the store the passthrough targets and, only when the parent
// carries a state label the caller did not name, adds --no-inherit-labels
// plus --labels=<every non-state parent label>. That is a single atomic bd
// create — no window in which a patrol can observe the child carrying the
// inherited state — and it cannot remove a caller-explicit label because it
// never removes or reorders a caller token: bd still applies everything the
// caller passed. (The alternative, stripping after the create, would need the
// new id from bd's stdout, which the passthrough streams to the caller
// untouched, and would leave the label visible between two writes.)
//
// The one gap versus bd's own inheritance is a parent label that changes
// between gc's read and bd's create — the same race as creating a moment
// earlier. Fail-open throughout: anything gc cannot read or represent
// faithfully forwards the argv unchanged with a warning, never a refusal.

// bdCreateParentLabels reads the labels of the parent a `gc bd create
// --parent` names, from the store the passthrough targets — the store bd
// resolves the parent in. Package var so tests stub the store round-trip.
var bdCreateParentLabels = func(cityPath string, cfg *config.City, target execStoreTarget, parentID string) ([]string, error) {
	store, err := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
	if err != nil {
		return nil, err
	}
	parent, err := store.Get(parentID)
	if err != nil {
		return nil, err
	}
	return parent.Labels, nil
}

// bdCreateInheritance is what a bd create argv says about label inheritance.
type bdCreateInheritance struct {
	verbIndex int
	parentID  string
	// insertAt is the argv index the guard's flags go in front of: just
	// after the verb, or just after the caller's last
	// --no-inherit-labels=<bool> token, because bd takes the LAST occurrence
	// of a flag and an earlier injected --no-inherit-labels would lose to a
	// caller's =false. Both positions follow a complete token, so the
	// injected flags can never be read as some flag's value.
	insertAt int
	// noInherit is --no-inherit-labels in effect: nothing is inherited.
	noInherit bool
	// batch is --file/--graph: bd creates from a plan and ignores --parent.
	batch bool
	// repo is --repo: bd resolves the parent in another repo's store.
	repo     bool
	explicit []string
	// unparseable means bd itself would reject this argv; leave it to bd.
	unparseable bool
}

// stripInheritedStateLabelsFromBdCreateArgs returns bdArgs rewritten so the
// created child does not inherit its parent's hold:*/cert:*/needs-summon
// labels, or bdArgs unchanged when no such inheritance would happen. It logs
// one line to stderr whenever it strips.
func stripInheritedStateLabelsFromBdCreateArgs(bdArgs []string, parentLabels func(parentID string) ([]string, error), stderr io.Writer) []string {
	plan, ok := parseBdCreateInheritance(bdArgs)
	if !ok || plan.parentID == "" || plan.noInherit || plan.batch || plan.unparseable {
		return bdArgs
	}
	if plan.repo {
		fmt.Fprintf(stderr, "gc bd: note: create --repo --parent %s resolves the parent in another store; the state-label inheritance guard did not run, so the child may inherit hold:*/cert:*/needs-summon (qc-p9m8oa9)\n", plan.parentID) //nolint:errcheck // best-effort stderr
		return bdArgs
	}
	inherited, err := parentLabels(plan.parentID)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: WARNING: could not read the labels of parent %s (%v); the state-label inheritance guard did not run, so the child may inherit hold:*/cert:*/needs-summon (qc-p9m8oa9)\n", plan.parentID, err) //nolint:errcheck // best-effort stderr
		return bdArgs
	}
	strip := beadmeta.InheritedStateLabelsToStrip(inherited, plan.explicit)
	if len(strip) == 0 {
		return bdArgs
	}
	var keep []string
	seen := make(map[string]struct{}, len(inherited))
	for _, label := range inherited {
		if beadmeta.IsCreateInheritanceExcludedLabel(label) {
			continue
		}
		if _, dup := seen[label]; dup {
			continue
		}
		// bd trims explicit labels, so a stored label with edge whitespace
		// (or an empty one) cannot be re-passed faithfully. Changing it
		// silently would be worse than the defect this guards.
		if label == "" || strings.TrimSpace(label) != label {
			fmt.Fprintf(stderr, "gc bd: WARNING: parent %s carries label %q, which cannot be passed back to bd unchanged; the state-label inheritance guard did not run, so the child may inherit %s (qc-p9m8oa9)\n", plan.parentID, label, strings.Join(strip, ", ")) //nolint:errcheck // best-effort stderr
			return bdArgs
		}
		seen[label] = struct{}{}
		keep = append(keep, label)
	}
	injected := []string{"--no-inherit-labels"}
	if len(keep) > 0 {
		encoded, err := encodeBdLabelsCSV(keep)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: WARNING: could not encode the parent's labels for bd (%v); the state-label inheritance guard did not run (qc-p9m8oa9)\n", err) //nolint:errcheck // best-effort stderr
			return bdArgs
		}
		injected = append(injected, "--labels="+encoded)
	}
	out := make([]string, 0, len(bdArgs)+len(injected))
	out = append(out, bdArgs[:plan.insertAt]...)
	out = append(out, injected...)
	out = append(out, bdArgs[plan.insertAt:]...)
	fmt.Fprintf(stderr, "gc bd: create --parent %s: not inheriting the parent's state labels %s — they describe the parent, not a new child; every other parent label is still inherited, and -l <label> keeps one deliberately (qc-p9m8oa9 interim, removed when bd excludes these at create)\n", plan.parentID, strings.Join(strip, ", ")) //nolint:errcheck // best-effort stderr
	return out
}

// parseBdCreateInheritance walks a bd argv the way bd's flag parser (pflag)
// does, far enough to answer the inheritance questions: which parent, whether
// inheritance is off, and which labels the caller passed explicitly
// (-l/--labels/--label, repeatable, each value comma-separated). ok is false
// when the argv is not a create.
func parseBdCreateInheritance(bdArgs []string) (bdCreateInheritance, bool) {
	plan := bdCreateInheritance{verbIndex: -1}
	globals := bdflags.GlobalValueFlags()
	for i := 0; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if !strings.HasPrefix(arg, "-") {
			plan.verbIndex = i
			break
		}
		if arg == "--" {
			return plan, false
		}
		if !strings.Contains(arg, "=") && globals[arg] {
			i++
		}
	}
	if plan.verbIndex < 0 {
		return plan, false
	}
	// bd registers `new` as an alias for `create`.
	if verb := bdArgs[plan.verbIndex]; verb != "create" && verb != "new" {
		return plan, false
	}
	plan.insertAt = plan.verbIndex + 1
	valueFlags := bdflags.ValueFlags("create")
	boolFlags := bdflags.BoolFlags("create")
	// --label is bd's hidden alias for --labels; hidden flags are absent from
	// the help text the flag tables were transcribed from.
	valueFlags["--label"] = true

	addLabels := func(value string) {
		labels, err := readBdCSVValue(value)
		if err != nil {
			plan.unparseable = true
			return
		}
		plan.explicit = append(plan.explicit, labels...)
	}
	apply := func(name, value string, hasValue bool) {
		switch name {
		case "--parent":
			plan.parentID = value
		case "-l", "--labels", "--label":
			addLabels(value)
		case "--no-inherit-labels":
			on := true
			if hasValue {
				parsed, err := strconv.ParseBool(value)
				if err != nil {
					plan.unparseable = true
					return
				}
				on = parsed
			}
			plan.noInherit = on
		case "-f", "--file", "--graph":
			if value != "" {
				plan.batch = true
			}
		case "--repo":
			plan.repo = value != ""
		}
	}

	args := bdArgs[plan.verbIndex+1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return plan, true
		case !strings.HasPrefix(arg, "-") || arg == "-":
			continue
		case strings.HasPrefix(arg, "--"):
			name, value, inline := strings.Cut(arg, "=")
			if !inline && valueFlags[name] {
				if i+1 >= len(args) {
					plan.unparseable = true
					return plan, true
				}
				i++
				value = args[i]
				inline = true
			}
			if valueFlags[name] || boolFlags[name] {
				apply(name, value, inline)
			}
			if name == "--no-inherit-labels" && inline {
				plan.insertAt = plan.verbIndex + 1 + i + 1
			}
			// Unknown long flag: skipped without consuming a value, like
			// the sibling assignee walker. The rewrite only ever adds
			// tokens, so a misread here cannot drop a caller label.
		default:
			// A shorthand cluster: -q, -lvalue, -l=value, -ql value.
			shorthands := arg[1:]
			for j := 0; j < len(shorthands); j++ {
				name := "-" + string(shorthands[j])
				rest := shorthands[j+1:]
				switch {
				case valueFlags[name]:
					var value string
					switch {
					case len(rest) > 1 && rest[0] == '=':
						value = rest[1:]
					case rest != "":
						value = rest
					case i+1 < len(args):
						i++
						value = args[i]
					default:
						plan.unparseable = true
						return plan, true
					}
					apply(name, value, true)
					j = len(shorthands)
				case boolFlags[name]:
					if len(rest) > 1 && rest[0] == '=' {
						j = len(shorthands)
					}
				default:
					plan.unparseable = true
					return plan, true
				}
			}
		}
	}
	return plan, true
}

// readBdCSVValue splits one -l/--labels value exactly as bd's flag library
// does (a single CSV record), then trims and drops empties as bd's label
// normalization does.
func readBdCSVValue(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	fields, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			labels = append(labels, field)
		}
	}
	return labels, nil
}

// encodeBdLabelsCSV renders labels as the single CSV record bd's flag
// library reads back into exactly these values, quoting any label that
// contains a comma or quote.
func encodeBdLabelsCSV(labels []string) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(labels); err != nil {
		return "", err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
