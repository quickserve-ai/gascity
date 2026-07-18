package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Assignee canonicalization for the gc bd passthrough (ga-i44k).
//
// The find-work read paths match a bead's assignee by EXACT string equality
// against a session's identity set (sessionBeadAssigneeIdentities), and the
// agent-side hook query filters by exact $GC_AGENT. Only the alias form is
// visible to BOTH paths, yet hand-written `bd update/create --assignee`
// values arrive in every historical spelling (qcore/crew/lana, qcore--lana,
// bare names, dead template-ish forms). Beads assigned to a non-matching
// string sit invisible while LOOKING assigned — the ~98-bead strand the
// ga-i44k audit measured, 25h on PR #2504 alone.
//
// gc hook --claim was the top write chokepoint and now writes the alias form
// (6d6c33382). This file guards the manual chokepoint: known variants are
// rewritten to the canonical alias with a notice, and shapes matching no
// live identity produce a loud warning but pass through unchanged — a
// cross-town assignee (q_core/*, Alex-town crew) is legitimate here, so
// unknown must warn, never block or rewrite.
//
// Fail-open by design: any error building the identity index skips
// canonicalization silently. This path runs in front of every bd write and
// must never turn an index hiccup into a blocked mutation or spurious noise.
// Set GC_BD_ASSIGNEE_CANONICALIZE=off to disable entirely.

const bdAssigneeCanonicalizeEnv = "GC_BD_ASSIGNEE_CANONICALIZE"

// bdListSessionBeadsForAssigneeIndex is a package var so tests can stub the
// store round-trip. Live:true keeps ephemeral (wisp) session beads visible —
// plain list queries hide them.
var bdListSessionBeadsForAssigneeIndex = func(cityPath string) ([]beads.Bead, error) {
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, err
	}
	return session.ListAllSessionBeads(store, beads.ListQuery{Live: true})
}

// canonicalizeBdAssigneeArgs rewrites -a/--assignee values on bd
// update/create invocations to the canonical alias form when the written
// value maps unambiguously to a live agent identity, and warns on values
// matching nothing. Returns args unchanged (and stays silent) when the
// subcommand carries no assignee, the identity index cannot be built, or
// canonicalization is disabled.
func canonicalizeBdAssigneeArgs(bdArgs []string, cityPath string, cfg *config.City, stderr io.Writer) []string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(bdAssigneeCanonicalizeEnv)), "off") {
		return bdArgs
	}
	sub := bdFirstPositionalArg(bdArgs)
	if sub != "update" && sub != "create" {
		return bdArgs
	}
	tokens := bdAssigneeTokens(sub, bdArgs)
	if len(tokens) == 0 {
		return bdArgs
	}
	if cfg == nil {
		return bdArgs
	}
	sessionBeads, err := bdListSessionBeadsForAssigneeIndex(cityPath)
	if err != nil {
		// Config-only resolution could misreport live-only identities
		// (session bead IDs, pool session names) as unknown. Better no
		// canonicalization than wrong warnings.
		return bdArgs
	}
	index := buildBdAssigneeIndex(cfg, sessionBeads)

	out := append([]string(nil), bdArgs...)
	for _, tok := range tokens {
		raw := strings.TrimSpace(tok.value)
		if raw == "" {
			continue
		}
		canonical, verdict := index.resolve(raw)
		switch verdict {
		case bdAssigneeCanonical:
			// Already a form every read path accepts — nothing to say.
		case bdAssigneeRewrite:
			fmt.Fprintf(stderr, "gc bd: assignee %q canonicalized -> %q (ga-i44k: the written form is invisible to its owner's find-work)\n", raw, canonical) //nolint:errcheck // best-effort stderr
			if tok.inline {
				out[tok.index] = tok.flag + "=" + canonical
			} else {
				out[tok.index] = canonical
			}
		case bdAssigneePoolTemplate:
			fmt.Fprintf(stderr, "gc bd: WARNING: assignee %q is a pool TEMPLATE — template-assigned beads sit stranded while pool workers report an empty pool (qc-4ubdx). Route pool work with 'gc sling' instead. Leaving as-is.\n", raw) //nolint:errcheck // best-effort stderr
		case bdAssigneeAmbiguous:
			fmt.Fprintf(stderr, "gc bd: WARNING: assignee %q is ambiguous among [%s] — leaving as-is; use the full alias form (ga-i44k)\n", raw, strings.Join(index.candidatesFor(raw), ", ")) //nolint:errcheck // best-effort stderr
		default: // bdAssigneeUnknown
			fmt.Fprintf(stderr, "gc bd: WARNING: assignee %q matches no live agent identity in this city — the bead will NOT surface in any agent's find-work (ga-i44k). Verify with 'gc agent list' / 'gc session list', or ignore if this is a cross-town assignee.\n", raw) //nolint:errcheck // best-effort stderr
		}
	}
	return out
}

func bdFirstPositionalArg(args []string) string {
	for _, arg := range args {
		if arg == "--" {
			return ""
		}
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

type bdAssigneeToken struct {
	index  int    // index in args of the token to replace
	inline bool   // --assignee=value / -a=value form
	flag   string // flag spelling as written, for inline reconstruction
	value  string
}

// bdAssigneeTokens locates assignee values by walking the argument list with
// the subcommand's full flag tables, mirroring bdMutationWriteIDs, so a flag
// VALUE that merely looks like -a is never misread. Unknown flags are skipped
// without consuming a value — worst case a value is misread as a flag and no
// canonicalization happens; this walker must never cause a hard failure.
func bdAssigneeTokens(sub string, args []string) []bdAssigneeToken {
	valueFlags := bdAssigneeValueFlags(sub)
	boolFlags := bdAssigneeBoolFlags(sub)
	var tokens []bdAssigneeToken
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if eq := strings.IndexByte(arg, '='); eq >= 0 {
			flag := arg[:eq]
			if flag == "--assignee" || flag == "-a" {
				tokens = append(tokens, bdAssigneeToken{index: i, inline: true, flag: flag, value: arg[eq+1:]})
			}
			continue
		}
		flagName := strings.TrimLeft(arg, "-")
		longForm := "--" + flagName
		shortForm := "-" + flagName
		isValueFlag := valueFlags[longForm] || (len(flagName) == 1 && valueFlags[shortForm])
		if isValueFlag {
			if (arg == "--assignee" || arg == "-a") && i+1 < len(args) {
				tokens = append(tokens, bdAssigneeToken{index: i + 1, flag: arg, value: args[i+1]})
			}
			i++
			continue
		}
		if boolFlags[longForm] || (len(flagName) == 1 && boolFlags[shortForm]) {
			continue
		}
		// Unknown flag: skip conservatively without consuming a value.
	}
	return tokens
}

func bdAssigneeValueFlags(sub string) map[string]bool {
	if sub == "update" {
		return bdSubcmdValueFlags("update")
	}
	// bd create value flags, sourced from `bd create --help` (2026-07-18),
	// merged with the same global value flags bdSubcmdValueFlags carries.
	flags := map[string]bool{
		"--actor": true, "--db": true, "--directory": true, "-C": true,
		"--dolt-auto-commit": true,
		"--acceptance":       true, "--append-notes": true,
		"-a": true, "--assignee": true,
		"--body-file": true, "--context": true,
		"--defer": true, "--deps": true,
		"-d": true, "--description": true,
		"--design": true, "--design-file": true,
		"--due": true,
		"-e":    true, "--estimate": true,
		"--event-actor": true, "--event-category": true,
		"--event-payload": true, "--event-target": true,
		"--external-ref": true,
		"-f":             true, "--file": true,
		"--graph": true, "--id": true,
		"-l": true, "--labels": true,
		"--metadata": true, "--mol-type": true,
		"--notes": true, "--parent": true,
		"-p": true, "--priority": true,
		"--repo": true, "--skills": true, "--spec-id": true,
		"--title": true,
		"-t":      true, "--type": true,
	}
	return flags
}

func bdAssigneeBoolFlags(sub string) map[string]bool {
	if sub == "update" {
		return bdSubcmdBoolFlags("update")
	}
	return map[string]bool{
		"--global": true, "--ignore-schema-skew": true,
		"--json": true, "--profile": true,
		"-q": true, "--quiet": true,
		"--readonly": true, "--sandbox": true,
		"-v": true, "--verbose": true,
		"-h": true, "--help": true,
		"--dry-run": true, "--ephemeral": true,
		"--force": true, "--no-history": true,
		"--no-inherit-labels": true, "--silent": true,
		"--stdin": true,
	}
}

type bdAssigneeVerdict int

const (
	bdAssigneeUnknown bdAssigneeVerdict = iota
	bdAssigneeCanonical
	bdAssigneeRewrite
	bdAssigneeAmbiguous
	bdAssigneePoolTemplate
)

type bdAssigneeIndex struct {
	// canonical: strings that are the alias/identity form — visible to both
	// the assigned-work scope AND the owner's hook query. Includes every
	// identity of alias-less sessions (pool workers), whose session-name IS
	// their only form.
	canonical map[string]struct{}
	// rewrite: non-canonical members of a live session's identity set
	// (session_name, session bead ID, alias history, runtime tmux names) ->
	// the canonical alias. Multi-valued when sources conflict.
	rewrite map[string]map[string]struct{}
	// byName: bare leaf name ("lana", "refinery") -> canonical forms, for
	// resolving legacy /crew/ paths, bare names, and dead template-ish
	// spellings by their last segment.
	byName map[string]map[string]struct{}
	// poolTemplates: qualified names of instance-expanding agents; valid
	// route targets but strand-prone as assignees.
	poolTemplates map[string]struct{}
}

func buildBdAssigneeIndex(cfg *config.City, sessionBeads []beads.Bead) *bdAssigneeIndex {
	ix := &bdAssigneeIndex{
		canonical:     make(map[string]struct{}),
		rewrite:       make(map[string]map[string]struct{}),
		byName:        make(map[string]map[string]struct{}),
		poolTemplates: make(map[string]struct{}),
	}
	cityName := cfg.EffectiveCityName()
	for i := range cfg.NamedSessions {
		identity := strings.TrimSpace(cfg.NamedSessions[i].QualifiedName())
		if identity == "" {
			continue
		}
		ix.addCanonical(identity)
		if runtime := strings.TrimSpace(config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity)); runtime != "" && runtime != identity {
			ix.addRewrite(runtime, identity)
		}
	}
	for i := range cfg.Agents {
		agentCfg := &cfg.Agents[i]
		qn := strings.TrimSpace(agentCfg.QualifiedName())
		if qn == "" {
			continue
		}
		if agentCfg.SupportsInstanceExpansion() {
			ix.poolTemplates[qn] = struct{}{}
			continue
		}
		ix.addCanonical(qn)
	}
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		alias := strings.TrimSpace(sb.Metadata["alias"])
		if alias == "" {
			alias = strings.TrimSpace(sb.Metadata[session.NamedSessionIdentityMetadata])
		}
		identities := sessionBeadAssigneeIdentities(sb)
		if alias == "" {
			// Alias-less pool worker: its session-name/bead-ID forms are the
			// canonical assignment identities — accept, never rewrite.
			for _, id := range identities {
				ix.canonical[id] = struct{}{}
			}
			continue
		}
		ix.addCanonical(alias)
		for _, id := range identities {
			if id != alias {
				ix.addRewrite(id, alias)
			}
		}
	}
	return ix
}

func (ix *bdAssigneeIndex) addCanonical(identity string) {
	ix.canonical[identity] = struct{}{}
	for _, leaf := range bdAssigneeLeaves(identity) {
		set := ix.byName[leaf]
		if set == nil {
			set = make(map[string]struct{}, 1)
			ix.byName[leaf] = set
		}
		set[identity] = struct{}{}
	}
}

func (ix *bdAssigneeIndex) addRewrite(from, to string) {
	set := ix.rewrite[from]
	if set == nil {
		set = make(map[string]struct{}, 1)
		ix.rewrite[from] = set
	}
	set[to] = struct{}{}
}

// bdAssigneeLeaves returns the lookup keys a qualified identity is reachable
// under: its basename ("qcore/lana" -> "lana", "qcore/gastown.refinery" ->
// "gastown.refinery") and, when the basename is binding-prefixed, the bare
// role leaf ("refinery") so dead template-ish spellings like
// "qcore/refinery" resolve to the bound agent.
func bdAssigneeLeaves(identity string) []string {
	base := session.TargetBasename(identity)
	if base == "" {
		return nil
	}
	leaves := []string{base}
	if dot := strings.LastIndexByte(base, '.'); dot >= 0 && dot+1 < len(base) {
		leaves = append(leaves, base[dot+1:])
	}
	return leaves
}

func (ix *bdAssigneeIndex) resolve(raw string) (string, bdAssigneeVerdict) {
	if _, ok := ix.canonical[raw]; ok {
		return raw, bdAssigneeCanonical
	}
	if set, ok := ix.rewrite[raw]; ok {
		if len(set) == 1 {
			return singleBdAssignee(set), bdAssigneeRewrite
		}
		return "", bdAssigneeAmbiguous
	}
	if _, ok := ix.poolTemplates[raw]; ok {
		return "", bdAssigneePoolTemplate
	}
	leaf, prefix := bdAssigneeLeafAndPrefix(raw)
	if leaf == "" {
		return "", bdAssigneeUnknown
	}
	candidates := ix.byName[leaf]
	if len(candidates) == 0 {
		return "", bdAssigneeUnknown
	}
	// Prefer candidates under the same rig prefix when the written form had
	// one; a cross-rig prefix (cherub/mallory) falls back to the full set.
	if prefix != "" {
		scoped := make(map[string]struct{})
		for c := range candidates {
			if strings.HasPrefix(c, prefix+"/") {
				scoped[c] = struct{}{}
			}
		}
		if len(scoped) > 0 {
			candidates = scoped
		}
	}
	if len(candidates) == 1 {
		return singleBdAssignee(candidates), bdAssigneeRewrite
	}
	return "", bdAssigneeAmbiguous
}

// candidatesFor lists the possible canonicals behind an ambiguous verdict,
// for the warning message.
func (ix *bdAssigneeIndex) candidatesFor(raw string) []string {
	var set map[string]struct{}
	if s, ok := ix.rewrite[raw]; ok && len(s) > 1 {
		set = s
	} else if leaf, _ := bdAssigneeLeafAndPrefix(raw); leaf != "" {
		set = ix.byName[leaf]
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// bdAssigneeLeafAndPrefix splits a written assignee into its last segment and
// leading rig component: "qcore/crew/lana" -> ("lana", "qcore"),
// "qcore--lana" -> ("lana", "qcore"), "lana" -> ("lana", "").
func bdAssigneeLeafAndPrefix(raw string) (string, string) {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", ""
	}
	if i := strings.LastIndexByte(raw, '/'); i >= 0 {
		prefix := raw[:strings.IndexByte(raw, '/')]
		return raw[i+1:], prefix
	}
	if i := strings.LastIndex(raw, "--"); i >= 0 && i+2 < len(raw) {
		return raw[i+2:], raw[:strings.Index(raw, "--")]
	}
	return raw, ""
}

func singleBdAssignee(set map[string]struct{}) string {
	for c := range set {
		return c
	}
	return ""
}
