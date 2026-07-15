package worker

import "github.com/gastownhall/gascity/internal/sessionlog"

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
	// TranscriptTailMeta aliases transcript tail metadata.
	TranscriptTailMeta = sessionlog.TailMeta
	// TranscriptContextUsage aliases transcript context-usage accounting.
	TranscriptContextUsage = sessionlog.ContextUsage
	// AgentMapping aliases transcript agent-mapping metadata.
	AgentMapping = sessionlog.AgentMapping
)

// ErrAgentNotFound reports that the requested transcript agent was not found.
var ErrAgentNotFound = sessionlog.ErrAgentNotFound

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
