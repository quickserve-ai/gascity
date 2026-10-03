package worker

import (
	"errors"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

type (
	// TranscriptSession aliases the sessionlog transcript session payload.
	TranscriptSession = sessionlog.Session
	// TranscriptEntry aliases a single transcript entry.
	TranscriptEntry = sessionlog.Entry
	// TranscriptContentBlock aliases a single structured content block.
	TranscriptContentBlock = sessionlog.ContentBlock
	// TranscriptMessageContent aliases normalized message content.
	TranscriptMessageContent = sessionlog.MessageContent
	// TranscriptPagination aliases transcript pagination metadata.
	TranscriptPagination = sessionlog.PaginationInfo
	// TranscriptCursorDirection aliases a transcript pagination direction.
	TranscriptCursorDirection = sessionlog.CursorDirection
	// TranscriptCursorNotFoundError aliases a missing provider entry cursor.
	TranscriptCursorNotFoundError = sessionlog.CursorNotFoundError
	// TranscriptDuplicateEntryIDError aliases an ambiguous provider entry ID.
	TranscriptDuplicateEntryIDError = sessionlog.DuplicateEntryIDError
	// TranscriptTailMeta aliases transcript tail metadata.
	TranscriptTailMeta = sessionlog.TailMeta
	// TranscriptContextUsage aliases transcript context-usage accounting.
	TranscriptContextUsage = sessionlog.ContextUsage
	// AgentMapping aliases transcript agent-mapping metadata.
	AgentMapping = sessionlog.AgentMapping
)

const (
	// TranscriptCursorDirectionBefore requests entries before a cursor.
	TranscriptCursorDirectionBefore = sessionlog.CursorDirectionBefore
	// TranscriptCursorDirectionAfter requests entries after a cursor.
	TranscriptCursorDirectionAfter = sessionlog.CursorDirectionAfter
)

// ErrAgentNotFound reports that the requested transcript agent was not found.
var ErrAgentNotFound = sessionlog.ErrAgentNotFound

// ErrTranscriptCursorNotFound reports a cursor absent from the current
// provider transcript view.
var ErrTranscriptCursorNotFound = sessionlog.ErrCursorNotFound

// ErrTranscriptDuplicateEntryID reports provider output whose entry IDs cannot
// identify an unambiguous page boundary.
var ErrTranscriptDuplicateEntryID = sessionlog.ErrDuplicateEntryID

// ErrTranscriptCursorConflict reports a request containing both before and
// after entry cursors.
var ErrTranscriptCursorConflict = errors.New("before and after entry IDs are mutually exclusive")

// DefaultSearchPaths returns the default transcript search roots.
func DefaultSearchPaths() []string {
	return sessionlog.DefaultSearchPaths()
}

// SessionHistoryEntry aliases one discovered transcript-history row.
type SessionHistoryEntry = sessionlog.SessionHistoryEntry

// TranscriptSummary aliases the cheap transcript head/tail summary.
type TranscriptSummary = sessionlog.TranscriptSummary

// ListClaudeSessionHistory lists every claude-family transcript recorded for
// workDir across live and archive roots, newest first.
func ListClaudeSessionHistory(searchPaths, archiveRoots []string, workDir string) []SessionHistoryEntry {
	return sessionlog.ListClaudeSessionHistory(searchPaths, archiveRoots, workDir)
}

// FindClaudeTranscriptsByID returns every live copy of the claude-family
// transcript for sessionID under any project folder of searchPaths, or the
// archived copies when no live one exists, newest first.
func FindClaudeTranscriptsByID(searchPaths, archiveRoots []string, sessionID string) []SessionHistoryEntry {
	return sessionlog.FindClaudeTranscriptsByID(searchPaths, archiveRoots, sessionID)
}

// ClaudeProjectSlugCandidates returns the project slug directory names a
// claude process whose cwd is workDir may have used.
func ClaudeProjectSlugCandidates(workDir string) []string {
	return sessionlog.ClaudeProjectSlugCandidates(workDir)
}

// ReadClaudeTranscriptAgentName returns the transcript's first recorded
// agent name, or "".
func ReadClaudeTranscriptAgentName(path string) string {
	return sessionlog.ReadClaudeTranscriptAgentName(path)
}

// ClaudeTranscriptCwdForSlug returns a cwd the transcript records whose
// project slug is slug, or "".
func ClaudeTranscriptCwdForSlug(path, slug string) string {
	return sessionlog.ClaudeTranscriptCwdForSlug(path, slug)
}

// ReadClaudeTranscriptSummary scans a transcript for its title, agent name,
// first user message, and first timestamp.
func ReadClaudeTranscriptSummary(path string) TranscriptSummary {
	return sessionlog.ReadClaudeTranscriptSummary(path)
}

// DefaultTranscriptArchiveRoots returns the transcript-reaper archive roots.
func DefaultTranscriptArchiveRoots() []string {
	return sessionlog.DefaultTranscriptArchiveRoots()
}

// MergeSearchPaths normalizes and deduplicates transcript search roots.
func MergeSearchPaths(paths []string) []string {
	return sessionlog.MergeSearchPaths(paths)
}

// ValidateAgentID verifies that the supplied transcript agent identifier is valid.
func ValidateAgentID(agentID string) error {
	return sessionlog.ValidateAgentID(agentID)
}

// InferTranscriptActivity summarizes transcript activity from the supplied entries.
func InferTranscriptActivity(entries []*TranscriptEntry) string {
	return sessionlog.InferActivityFromEntries(entries)
}
