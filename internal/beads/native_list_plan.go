package beads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// NativeListPlan returns the reader-role requests NativeDoltStore.List issues
// for query, in the order it issues them, on a scope whose infra-type setting
// is unset. The store unions their rows by id and ApplyListQuery then applies
// the whole query exactly, so a plan only decides how many rows cross from the
// backend, never which rows the caller receives: every predicate it pushes is
// exact or a superset of the query's.
//
// A plural Assignees set fans out into one request per distinct assignee, each
// with that assignee pushed, however many there are: a keyed request reads one
// assignee's rows, while the one request a plural set would otherwise collapse
// into carries no assignee and, for a type and status that cannot be pushed
// (open mail), walks the whole ledger. The status and type pushdowns are
// nativeListRequestFromListQuery's.
//
// A FANNED-OUT PLAN IS SEVERAL SNAPSHOTS. The reader role publishes no read
// transaction, so each request reads the ledger as of its own arrival: a bead
// whose assignee moves between two of the requests can be seen by both (the
// union keeps it once) or by neither. The answer is then one the ledger never
// held at a single instant, but every row in it was true when it was read, and
// the next hook's listing reads the moved row where it now lives. Hooks
// tolerate that by design — they are re-run on every turn and converge — and a
// caller that needs one instant's answer passes a singular Assignee.
//
// It is a pure function of the query so a test can price a recorded List
// without a server: each request costs one backend listing, and a request that
// NativeListRequestKeyed reports as unkeyed reads the whole ledger.
func NativeListPlan(query ListQuery) []issueops.ListRequest {
	return nativeListPlan(query, nativeListDefaultPushableTypes, "")
}

// nativeListPlan is NativeListPlan over a store's own pushable type set and
// not-closed status set. A nil type set pushes no type at all; an empty status
// set pushes no not-closed predicate.
func nativeListPlan(query ListQuery, pushableTypes map[string]bool, notClosed string) []issueops.ListRequest {
	base := nativeListRequestFromListQuery(query, pushableTypes, notClosed)
	assignees := nativeListFanOutAssignees(query)
	if len(assignees) == 0 {
		return nativeListIDChunks(base, query.IDs)
	}
	plan := make([]issueops.ListRequest, 0, len(assignees))
	for _, assignee := range assignees {
		req := base
		req.Assignee = assignee
		plan = append(plan, req)
	}
	return plan
}

// nativeListIDChunks pushes an id set as IDFilter, one request per
// nativeServerEdgeAnchorCap ids (the id count a served backend is known to
// accept on one call). Without it a ListQuery naming ids listed the whole
// ledger and filtered in Go. ApplyListQuery still applies the ids exactly, so
// the union of the chunks is a superset of nothing it should not return. A
// backend that refuses IDFilter is handled by List (nativeListIDsRefused).
func nativeListIDChunks(base issueops.ListRequest, ids []string) []issueops.ListRequest {
	if len(ids) == 0 {
		return []issueops.ListRequest{base}
	}
	plan := make([]issueops.ListRequest, 0, (len(ids)+nativeServerEdgeAnchorCap-1)/nativeServerEdgeAnchorCap)
	for start := 0; start < len(ids); start += nativeServerEdgeAnchorCap {
		req := base
		req.IDFilter = strings.Join(ids[start:min(start+nativeServerEdgeAnchorCap, len(ids))], ",")
		plan = append(plan, req)
	}
	return plan
}

// nativeListIDsRefused reports a backend refusing IDFilter on a listing: an
// http-native IssueReader does so client-side, before any round trip, with an
// error that unwraps to *beadslib.ErrUnsupported.
func nativeListIDsRefused(err error) bool {
	var unsupported *beadslib.ErrUnsupported
	return errors.As(err, &unsupported)
}

// nativeListFanOutAssignees returns the distinct assignees a plan fans out
// over, or nil when the plural set cannot be pushed exactly. An empty entry
// means "unassigned", which an Assignee predicate cannot say.
func nativeListFanOutAssignees(query ListQuery) []string {
	if query.Assignee != "" || len(query.Assignees) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(query.Assignees))
	out := make([]string, 0, len(query.Assignees))
	for _, assignee := range query.Assignees {
		if assignee == "" {
			return nil
		}
		if seen[assignee] {
			continue
		}
		seen[assignee] = true
		out = append(out, assignee)
	}
	return out
}

// nativeListCanPushNotClosed reports whether query's plan can carry the
// not-closed set: it opted in and leaves the status to the default. It says
// nothing of whether this store has a set to push.
func nativeListCanPushNotClosed(query ListQuery) bool {
	return query.PushNotClosed && query.Status == "" && !query.IncludeClosed && !query.CompleteActive
}

// nativeListPushableStatus reports whether a gc status maps one-to-one onto a
// bd status. mapBdStatus folds every other bd status into gc's "open".
func nativeListPushableStatus(status string) bool {
	return status == "in_progress" || status == "closed"
}

// nativeTypesInfraConfigKey is the workspace setting naming the types bd treats
// as infrastructure.
const nativeTypesInfraConfigKey = "types.infra"

// nativeListHistoricalInfraTypes are every type a bd release has treated as
// infra by default: agent, role and message today, and rig until bd commit
// 4d3a552424 (Jun 22 2026). Naming an infra type on a listing routes it to the
// ephemeral plane alone (workapi.BuildListFilter), and the library reads the
// durable rows only when no wisp matches, so pushing one would intermittently
// drop durable rows of that type — a loss the Go-side re-filter cannot undo.
// None of them is ever pushed, whatever the server's own setting says, because
// a server may run a release whose built-in default still carries one.
var nativeListHistoricalInfraTypes = map[string]bool{"agent": true, "rig": true, "role": true, "message": true}

// nativeListDefaultPushableTypes is the type vocabulary a listing may push on a
// scope whose types.infra setting is unset: the types gc registers in every
// scope it provisions (RequiredCustomTypes), less the historical infra types.
// A server that validates types knows every one of them; a scope provisioned
// without them refuses the request, and NativeDoltStore.List retries it
// without the type.
var nativeListDefaultPushableTypes = nativeListPushableTypesFor("")

// nativeListPushableTypesFor is nativeListDefaultPushableTypes less every type
// a types.infra setting value names. The value is bd's comma-separated list.
//
// THE ONE RESIDUE is a server whose setting is unset and whose config.yaml
// carries its own types.infra (bd's second fallback). The workspace-config
// role publishes only stored settings, so that list is invisible here; gc
// never writes it, and a type an operator adds there must also be added to the
// stored setting for the pushdown to honor it.
func nativeListPushableTypesFor(infraSetting string) map[string]bool {
	infra := make(map[string]bool)
	for _, t := range strings.Split(infraSetting, ",") {
		if t = strings.TrimSpace(t); t != "" {
			infra[t] = true
		}
	}
	out := make(map[string]bool, len(RequiredCustomTypes))
	for _, t := range RequiredCustomTypes {
		if !nativeListHistoricalInfraTypes[t] && !infra[t] {
			out[t] = true
		}
	}
	return out
}

// nativeReadListPushableTypes reads the scope's types.infra setting through the
// workspace-config role, as nativeReadIssuePrefix reads the prefix, and returns
// the types a listing of this scope may push.
func nativeReadListPushableTypes(ctx context.Context, storage beadslib.Storage) (map[string]bool, error) {
	settings, err := storage.WorkspaceConfig()
	if err != nil {
		return nil, err
	}
	result, err := settings.GetSetting(ctx, issueops.GetSettingRequest{Key: nativeTypesInfraConfigKey})
	if err != nil {
		return nil, err
	}
	return nativeListPushableTypesFor(result.Value), nil
}

// loadListPushableTypes sets the store's pushable type set from its scope's
// types.infra setting. It runs once, at open, so a type an operator adds to
// the setting later stays pushable on a store already open — losing that
// type's durable rows — until the process holding the store restarts. A
// setting that cannot be read leaves the set nil, and the store then pushes no
// type at all: a type the server classifies as infra would lose durable rows,
// and only the server's own setting says which types those are.
func (s *NativeDoltStore) loadListPushableTypes(ctx context.Context) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		s.listLogger().Warn("native bead store: cannot read the scope's types.infra setting; listing without type pushdown for this store",
			"prefix", s.idPrefix, "err", err)
		return
	}
	defer release()
	pushable, err := nativeReadListPushableTypes(ctx, storage)
	if err != nil {
		s.listLogger().Warn("native bead store: cannot read the scope's types.infra setting; listing without type pushdown for this store",
			"prefix", s.idPrefix, "err", err)
		return
	}
	s.listPushableTypes = pushable
}

// nativeListBuiltinNotClosedStatuses is bd's built-in status vocabulary less
// closed. mapBdStatus folds every bd status but closed and in_progress into
// gc's "open", so gc's not-closed default is exactly every bd status but
// closed. pinned and hooked are listed for a second reason: naming either in
// the request's Status lifts the pinned-flag predicate, as AllFlag lifts the
// status exclusions (see issueops.ListRequest.Status).
var nativeListBuiltinNotClosedStatuses = []string{"open", "in_progress", "blocked", "deferred", "pinned", "hooked"}

// nativeListNotClosedStatusFor returns the not-closed OR set for a scope whose
// status.custom setting is customSetting, and false when the setting does not
// parse. The setting is bd's comma-separated list, each entry a name with an
// optional ":category" (the category never makes a status closed to gc:
// mapBdStatus reads the name). A custom status that a listing left out would be
// hidden from every no-Status read of the scope, so anything this cannot read
// as a plain name pushes nothing rather than guessing.
func nativeListNotClosedStatusFor(customSetting string) (string, bool) {
	statuses := append([]string(nil), nativeListBuiltinNotClosedStatuses...)
	seen := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		seen[status] = true
	}
	for _, entry := range strings.Split(customSetting, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, _, _ := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		if name == "closed" || !nativeListStatusNameOK(name) {
			return "", false
		}
		if !seen[name] {
			seen[name] = true
			statuses = append(statuses, name)
		}
	}
	return strings.Join(statuses, ","), true
}

// nativeListStatusNameOK reports whether name is a plain status name: letters,
// digits, underscore and hyphen. It is the request's OR-set separator and the
// SQL it reaches that make anything else unsafe to pass on.
func nativeListStatusNameOK(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')) {
			return false
		}
	}
	return true
}

// loadListNotClosedStatus records the store's not-closed status set from its
// scope's status.custom setting at open, read through the workspace-config
// role as loadListPushableTypes reads types.infra, and warns once if it cannot
// be read. Opted-in listings do not use this copy: each reads the setting
// afresh (readListNotClosedStatus), so a custom status added after open is
// pushed by the next one.
//
// WHAT THE SET CANNOT NAME until ExcludeStatus (beads #7647) lets the pushdown
// say "not closed" directly: a row whose status is in no current status.custom,
// either a name since removed from the setting while rows still carry it, or
// one kept only in a server's config.yaml. A pushed read would hide it, so
// guardNotClosed probes for such rows before each push and reads that listing
// unpushed when it finds one or cannot tell. Only a row that leaves the set
// between the probe and the read is missed, by that one listing.
//
// THE SAME RESIDUE as types.infra: a status.custom kept only in a server's
// config.yaml is invisible to the workspace-config role, so its statuses are
// missing from the set and any row in one costs that scope the pushdown; such
// a status must also be stored as the setting.
func (s *NativeDoltStore) loadListNotClosedStatus(ctx context.Context) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		s.listLogger().Warn("native bead store: cannot read the scope's status.custom setting; listing without the not-closed pushdown",
			"prefix", s.idPrefix, "err", err)
		return
	}
	defer release()
	set, err := notClosedStatusFromStorage(ctx, storage)
	if err == nil {
		s.listNotClosedStatus = set
		return
	}
	s.listLogger().Warn("native bead store: cannot read the scope's status.custom setting; listing without the not-closed pushdown for this store",
		"prefix", s.idPrefix, "err", err)
}

// readListNotClosedStatus is the not-closed set for one opted-in read, read
// afresh from storage's status.custom setting so a name added after open is
// in it. An unreadable or unparsable setting pushes nothing ("") for this
// read, the full read the listing always made, and counts as a fallback on
// tally: no probe runs, but the pushdown this listing opted into was lost.
func (s *NativeDoltStore) readListNotClosedStatus(ctx context.Context, storage beadslib.Storage, tally *nativeListTally) string {
	set, err := notClosedStatusFromStorage(ctx, storage)
	if err != nil {
		tally.guardFallbacks++
		s.listLogger().Warn("native bead store: cannot read status.custom for an opted-in listing; reading it unpushed",
			"prefix", s.idPrefix, "err", err, "guard_stats", s.listCounters.guardText(*tally))
		return ""
	}
	return set
}

func notClosedStatusFromStorage(ctx context.Context, storage beadslib.Storage) (string, error) {
	if storage == nil {
		return "", fmt.Errorf("no storage")
	}
	settings, err := storage.WorkspaceConfig()
	if err != nil {
		return "", err
	}
	result, err := settings.GetSetting(ctx, issueops.GetSettingRequest{Key: issueops.SettingKeyStatusCustom})
	if err != nil {
		return "", err
	}
	set, ok := nativeListNotClosedStatusFor(result.Value)
	if !ok {
		return "", fmt.Errorf("status.custom %q does not parse as a status list", result.Value)
	}
	return set, nil
}

// nativeListTypeRefused reports whether err is the role refusing a pushed
// type: the wire's validation problem, or the local filter builder's
// unknown-type error, which carries no sentinel.
func nativeListTypeRefused(err error) bool {
	return errors.Is(err, issueops.ErrValidation) || strings.Contains(err.Error(), "invalid issue type")
}

// NativeListRequestKeyed reports whether req carries a predicate that narrows
// the backend's candidate set below the whole ledger. A request without one
// returns every row of both tiers, closed rows included, and is the full walk
// the list-request tripwire counts.
func NativeListRequestKeyed(req issueops.ListRequest) bool {
	return req.ParentID != "" ||
		req.IDFilter != "" ||
		req.Assignee != "" ||
		len(req.Labels) > 0 ||
		len(req.MetadataFields) > 0 ||
		req.CreatedBefore != nil ||
		req.Status != "" ||
		req.IssueType != ""
}

// ListRequestStats counts the backend listing requests a store has issued.
type ListRequestStats struct {
	// Lists is the number of List calls the store served.
	Lists int64
	// Requests is the number of backend listing requests those calls issued.
	Requests int64
	// Unkeyed is the number of requests that carried no narrowing predicate
	// and therefore read the whole ledger.
	Unkeyed int64
	// Rows is the number of rows the backend returned across all requests,
	// before any client-side filtering.
	Rows int64
	// GuardRuns is the number of not-closed guard probes run for opted-in
	// listings, counting a retried List's last attempt only.
	GuardRuns int64
	// GuardRows is the number of rows those probes found outside the pushed
	// set. Each probe reports at most nativeListGuardRowLimit.
	GuardRows int64
	// GuardFallbacks is the number of opted-in listings sent down the full
	// unpushed read because status.custom could not be used or a probe found
	// rows or failed. A status.custom failure runs no probe, so this can pass
	// GuardRuns.
	GuardFallbacks int64
}

// listRequestCounter is implemented by a store that counts the backend listing
// requests its List calls issue. It is unexported so the diagnostic stays out
// of the engine's method set, which the store wrappers must forward in full.
type listRequestCounter interface {
	listRequestStats() ListRequestStats
}

// ListRequestStatsOf returns the backend listing counts store has accumulated
// since it opened, and false when store does not keep them. NativeDoltStore
// keeps them; budget tests read them to prove a hook never walks the whole
// ledger.
func ListRequestStatsOf(store Store) (ListRequestStats, bool) {
	c, ok := store.(listRequestCounter)
	if !ok {
		return ListRequestStats{}, false
	}
	return c.listRequestStats(), true
}

// nativeListCounters is the atomic backing of ListRequestStats.
type nativeListCounters struct {
	lists    atomic.Int64
	requests atomic.Int64
	unkeyed  atomic.Int64
	rows     atomic.Int64

	guardRuns      atomic.Int64
	guardRows      atomic.Int64
	guardFallbacks atomic.Int64
}

func (c *nativeListCounters) noteList() { c.lists.Add(1) }

// nativeListTally is what one attempt of a List sent: every backend listing
// request, a refused one included, and the rows the answered ones returned. A
// List commits only its last attempt's tally, so a withReadRetry retry does not
// count the abandoned attempt's requests twice.
type nativeListTally struct {
	requests int64
	unkeyed  int64
	rows     int64

	guardRuns      int64
	guardRows      int64
	guardFallbacks int64
}

func (t *nativeListTally) noteRequest(req issueops.ListRequest, rows int) {
	t.requests++
	if !NativeListRequestKeyed(req) {
		t.unkeyed++
	}
	t.rows += int64(rows)
}

// commit adds a finished attempt's counts. It returns the store's guard run
// count when t ran the guard, and 0 when it did not. Runs are added last, so a
// reader that sees this run's count sees its rows and fallback too.
func (c *nativeListCounters) commit(t nativeListTally) int64 {
	c.requests.Add(t.requests)
	c.unkeyed.Add(t.unkeyed)
	c.rows.Add(t.rows)
	c.guardRows.Add(t.guardRows)
	c.guardFallbacks.Add(t.guardFallbacks)
	if t.guardRuns == 0 {
		return 0
	}
	return c.guardRuns.Add(t.guardRuns)
}

func (c *nativeListCounters) snapshot() ListRequestStats {
	return ListRequestStats{
		Lists:    c.lists.Load(),
		Requests: c.requests.Load(),
		Unkeyed:  c.unkeyed.Load(),
		Rows:     c.rows.Load(),

		GuardRuns:      c.guardRuns.Load(),
		GuardRows:      c.guardRows.Load(),
		GuardFallbacks: c.guardFallbacks.Load(),
	}
}

// listRequestStats implements listRequestCounter.
func (s *NativeDoltStore) listRequestStats() ListRequestStats {
	if s == nil {
		return ListRequestStats{}
	}
	return s.listCounters.snapshot()
}

// ListOpenQuery is the ListQuery the native store answers ListOpen with, and
// the one OpRecordingStore records for every delegate's ListOpen. MemStore and
// FileStore leave IncludeClosed unset for a closed status; they select the
// same beads, because a set Status alone decides ListQuery.Matches.
func ListOpenQuery(status ...string) ListQuery {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
		if status[0] == "closed" {
			query.IncludeClosed = true
		}
	}
	return query
}

// ChildrenQuery is the ListQuery a List-composed store answers Children with.
func ChildrenQuery(parentID string, opts ...QueryOpt) ListQuery {
	return ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	}
}

// ListByLabelQuery is the ListQuery a List-composed store answers ListByLabel
// with.
func ListByLabelQuery(label string, limit int, opts ...QueryOpt) ListQuery {
	return ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	}
}

// ListByAssigneeQuery is the ListQuery a List-composed store answers
// ListByAssignee with.
func ListByAssigneeQuery(assignee, status string, limit int) ListQuery {
	return ListQuery{Assignee: assignee, Status: status, Limit: limit, AllowScan: true}
}

// ListByMetadataQuery is the ListQuery a List-composed store answers
// ListByMetadata with.
func ListByMetadataQuery(filters map[string]string, limit int, opts ...QueryOpt) ListQuery {
	return ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	}
}

// listLogger is the logger the list pushdown reports through.
func (s *NativeDoltStore) listLogger() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// disableTypePushdown latches type pushdown off for this store after the
// backend refused issueType and answered the same request without it, and
// warns once: the answers stay exact, but the scope is missing gc's type
// vocabulary and every typed listing now reads more rows than it needs.
func (s *NativeDoltStore) disableTypePushdown(issueType string, err error) {
	if !s.typePushdownOff.CompareAndSwap(false, true) {
		return
	}
	s.listLogger().Warn("native bead store: backend refused a pushed issue type; listing without type pushdown for this store (register gc's custom types in this scope: gc doctor --fix)",
		"prefix", s.idPrefix, "issue_type", issueType, "err", err)
}

// disableStatusPushdown latches the not-closed pushdown off for this store
// after the backend refused the pushed status set without quoting a name and
// answered the same request without it, and warns once naming the store and
// the set: answers
// stay exact (closed rows are dropped Go-side), the opted-in listing just
// reads the whole ledger again until the process restarts.
func (s *NativeDoltStore) disableStatusPushdown(set string, err error) {
	if !s.statusPushdownOff.CompareAndSwap(false, true) {
		return
	}
	s.listLogger().Warn("native bead store: backend refused the pushed not-closed status set; listing without it for this store until restart (a status.custom name the scope no longer accepts)",
		"prefix", s.idPrefix, "refused_status", nativeListRefusedStatusName(err), "status_set", set, "err", err)
}

// nativeListRefusedStatusName is the status name a refusal quotes
// (`invalid status "name"`), or "" when it quotes none.
func nativeListRefusedStatusName(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	i := strings.Index(msg, `invalid status "`)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(`invalid status "`):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// nativeListStatusRefused reports whether err is the backend refusing a
// pushed status name: the wire's validation problem, or an unknown-status
// message from the local filter builder.
func nativeListStatusRefused(err error) bool {
	return errors.Is(err, issueops.ErrValidation) || strings.Contains(err.Error(), "invalid status")
}

// noteUnkeyedPlan is the whole-ledger tripwire: a plan request that carries no
// narrowing predicate pages through every row, and is logged at debug with
// the query that asked for it. ListRequestStats counts it. The query is
// rendered only when debug logging is on.
func (s *NativeDoltStore) noteUnkeyedPlan(query ListQuery, plan []issueops.ListRequest) {
	for _, req := range plan {
		if NativeListRequestKeyed(req) {
			continue
		}
		logger := s.listLogger()
		if !logger.Enabled(context.Background(), slog.LevelDebug) {
			return
		}
		logger.Debug("native bead store: list walks the whole ledger",
			"prefix", s.idPrefix, "query", fmt.Sprintf("%+v", query))
		return
	}
}
