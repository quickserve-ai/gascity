// The operator-attention registry (ga-s0fn27): the set of seats that are
// currently waiting on the human. Each seat's runtime hook writes one JSON file
// under <cityRoot>/.gc/runtime/attention/ when it starts waiting and removes it
// when the operator acts; a reaper clears entries whose session has cycled.
//
// GET /api/city/{cityName}/attention (internal/api/dashboardbff/attention.go)
// projects that directory. This is the raw registry shape — the "is this still
// true?" verification against live sessions and pending-approval probes happens
// in the frontend selector (attention/whosWaiting.ts), not here.

/**
 * Why a seat is waiting. `permission` and `blocked` are always the operator's
 * move; `question` is runtime-dependent — on claude it is the generic 60s idle
 * notification, on omp it is a real question.
 */
export type AttentionReason = 'question' | 'permission' | 'blocked';

/** Which runtime wrote the entry. Determines how `reason` should be read. */
export type AttentionRuntime = 'claude' | 'omp';

export interface AttentionRegistryEntry {
  /** Seat name, e.g. "qcore/archer" or "cheryl". Matches the agent/session alias. */
  seat: string;
  /** 'claude' | 'omp' as written; carried as a string so an unknown runtime still renders. */
  runtime: string;
  /** Deduplication id for the notification that produced this entry. */
  event_id: string;
  /** 'question' | 'permission' | 'blocked' as written. */
  reason: string;
  /** Waiting state as written by the hook, e.g. "waiting_user". */
  state: string;
  /** When the wait started. Either offset-style ("...-0700") or Z-style ISO. */
  since: string;
  /** One-line operator-facing summary from the hook. */
  summary: string;
  /**
   * The runtime's own session id. Equal to `gc session list`'s `session_key`
   * for both claude and omp seats, which is what makes a liveness join possible.
   */
  session_id: string;
}

export interface AttentionRegistry {
  /** Always an explicit array; empty when the registry directory does not exist. */
  entries: AttentionRegistryEntry[];
  /** Registry files that were present but did not parse. */
  skippedMalformed: number;
  /** ISO UTC timestamp of the directory read. */
  readAt: string;
}
