// Gas City hooks for Oh My Pi (OMP).
// Installed by gc into {workDir}/.omp/hooks/gc-hook.ts
// Managed by `gc hooks install`; put custom OMP hooks in separate extension
// files so upgrades can replace this file safely.
//
// Events:
//   session_start       → gc prime --hook (context side effects + OMP session id);
//                         stdout is CACHED and injected at the next
//                         before_agent_start — discarding it meant a primed
//                         session never saw its own prime output (ga-vat7sn)
//   session_compact     → gc prime --hook (reload after compaction), same caching
//   before_agent_start  → inject cached prime output + queued nudges + unread mail
//   tool_execution_start(ask) / tool_approval_requested
//                       → operator-attention registry entry + desktop notify
//                         (ga-s0fn27 slice 2; .gc/runtime/attention/<seat>.json)
//   input / tool_approval_resolved / tool_execution_end(ask)
//                       → clear the seat's attention entry
//
// gc launches this file via --hook, which omp merges into the EXTENSION
// loader (main.ts cliExtensionPaths), so it runs with the full ExtensionAPI —
// the tool/approval/input events below are extension-surface and available
// here (verified against omp 18.1.10 source, ga-s0fn27 feasibility probe).

import { execFileSync } from "node:child_process";
import { mkdirSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const GC_OMP_HOOK_VERSION = 5;
const PATH_PREFIX =
  `${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:/opt/homebrew/bin:/usr/local/bin:`;

function run(args: string[], cwd?: string, extraEnv: Record<string, string> = {}): string {
  try {
    return execFileSync("gc", args, {
      cwd: cwd || process.cwd(),
      encoding: "utf-8",
      timeout: 30000,
      stdio: ["ignore", "pipe", "inherit"],
      env: {
        ...process.env,
        ...extraEnv,
        PATH: PATH_PREFIX + (process.env.PATH || ""),
      },
    }).trim();
  } catch (err) {
    logRunFailure(args, cwd, err);
    return "";
  }
}

function logRunFailure(args: string[], cwd: string | undefined, err: unknown): void {
  try {
    const maybeError = err as { code?: string; signal?: string; message?: string } | undefined;
    const detail = maybeError?.code || maybeError?.signal || maybeError?.message || "unknown error";
    console.error(
      "gc-hooks run:",
      `gc ${args.join(" ")}`,
      "cwd",
      cwd || process.cwd(),
      "failed:",
      detail,
    );
  } catch {
    // Keep OMP hooks non-fatal even if stderr is unavailable.
  }
}

function providerSessionEnv(ctx: { sessionManager?: { getSessionId?: () => string } }): Record<string, string> {
  const sessionID = ctx.sessionManager?.getSessionId?.() || "";
  const env: Record<string, string> = { GC_PROVIDER_SESSION_ID_REQUIRED: "omp" };
  if (!sessionID) {
    return env;
  }
  env.GC_PROVIDER_SESSION_ID = sessionID;
  return env;
}

// --- Operator-attention registry (ga-s0fn27 slice 2) ---------------------
// Same contract as the Claude-side attention-notify/attention-clear pair:
// one JSON entry per seat under $GC_CITY_PATH/.gc/runtime/attention, written
// atomically (dot-prefixed tmp + rename — the reaper ignores dotfiles),
// cleared when the user acts. The stale-entry reaper's session-cycled
// predicate compares entry.session_id to gc session list's session_key,
// which for omp seats IS the omp session id (verified 2026-09-08), so we
// stamp ctx.sessionManager.getSessionId(). Non-gc omp sessions (no GC_AGENT
// or GC_CITY_PATH) write nothing. Every operation is try/catch non-fatal:
// the attention plane must never break the session it watches.

const ATTENTION_SEAT = process.env.GC_AGENT || "";
const ATTENTION_DIR = process.env.GC_CITY_PATH
  ? `${process.env.GC_CITY_PATH}/.gc/runtime/attention`
  : "";
const ATTENTION_SAFE_SEAT = ATTENTION_SEAT.replace(/[^A-Za-z0-9._-]/g, "_");

function attentionWrite(
  reason: "question" | "permission",
  summary: string,
  ctx: { sessionManager?: { getSessionId?: () => string } },
): void {
  if (!ATTENTION_SEAT || !ATTENTION_DIR) {
    return;
  }
  try {
    mkdirSync(ATTENTION_DIR, { recursive: true });
    const entry = {
      seat: ATTENTION_SEAT,
      runtime: "omp",
      event_id: randomUUID(),
      reason,
      state: "waiting_user",
      since: new Date().toISOString().replace(/\.\d{3}Z$/, "Z"),
      summary: summary.slice(0, 200),
      session_id: ctx.sessionManager?.getSessionId?.() || "",
    };
    const tmp = `${ATTENTION_DIR}/.${ATTENTION_SAFE_SEAT}.json.tmp`;
    writeFileSync(tmp, JSON.stringify(entry));
    renameSync(tmp, `${ATTENTION_DIR}/${ATTENTION_SAFE_SEAT}.json`);
  } catch {
    // Non-fatal by design.
  }
  try {
    const title = `${ATTENTION_SEAT} needs you`.replace(/"/g, "'");
    const body = summary.slice(0, 120).replace(/"/g, "'").replace(/\\/g, "");
    execFileSync(
      "osascript",
      ["-e", `display notification "${body}" with title "${title}" sound name "Ping"`],
      { timeout: 10000, stdio: "ignore" },
    );
  } catch {
    // Notification is best-effort; the registry entry is the record.
  }
}

function attentionClear(): void {
  if (!ATTENTION_SEAT || !ATTENTION_DIR) {
    return;
  }
  try {
    // Unconditional clear, single writer per seat (v1 semantics, matching
    // the Claude-side attention-clear hook).
    rmSync(`${ATTENTION_DIR}/${ATTENTION_SAFE_SEAT}.json`, { force: true });
  } catch {
    // Non-fatal by design.
  }
}

function appendSystemPrompt(systemPrompt: string[], additions: string[]): string[] {
  const extras = additions.filter(Boolean);
  if (extras.length === 0) {
    return systemPrompt;
  }
  return [...systemPrompt, extras.join("\n\n")];
}

export default function gascityOmpExtension(pi: ExtensionAPI) {
  // Prime output from session_start/session_compact, held until the next
  // before_agent_start can inject it. The start/compact callbacks have no
  // injection channel of their own, and v3 simply dropped the stdout — so a
  // freshly primed or freshly compacted session never saw its role context
  // (ga-vat7sn defect 3, evidence katya ga-c0v33s). Consumed on injection so
  // a prime is delivered exactly once.
  let pendingPrime = "";

  pi.on("session_start", (_event, ctx) => {
    pendingPrime = run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
  });

  pi.on("session_compact", (_event, ctx) => {
    pendingPrime = run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
  });

  pi.on("before_agent_start", (event, ctx) => {
    const prime = pendingPrime;
    pendingPrime = "";
    const nudges = run(["nudge", "drain", "--inject"], ctx.cwd);
    const mail = run(["mail", "check", "--inject"], ctx.cwd);
    const systemPrompt = appendSystemPrompt(event.systemPrompt, [prime, nudges, mail]);
    if (systemPrompt !== event.systemPrompt) {
      return { systemPrompt };
    }
  });

  // Operator-attention registry (ga-s0fn27 slice 2). Event mapping verified
  // against omp 18.1.10 source: the built-in ask tool's execution start is
  // the question prompt; tool_approval_requested is the authoritative
  // approval-dialog signal; input fires on any interactive submit (before
  // slash routing) and is the broadest clear; the two resolution events
  // clear their own prompt class. Handlers are synchronous and non-fatal.
  pi.on("tool_execution_start", (event, ctx) => {
    if (event.toolName === "ask") {
      attentionWrite("question", "omp is waiting for your answer (ask tool)", ctx);
    }
  });

  pi.on("tool_approval_requested", (event, ctx) => {
    attentionWrite("permission", `omp needs approval: ${event.toolName}`, ctx);
  });

  pi.on("tool_approval_resolved", () => {
    attentionClear();
  });

  pi.on("tool_execution_end", (event) => {
    if (event.toolName === "ask") {
      attentionClear();
    }
  });

  pi.on("input", () => {
    attentionClear();
  });
}
