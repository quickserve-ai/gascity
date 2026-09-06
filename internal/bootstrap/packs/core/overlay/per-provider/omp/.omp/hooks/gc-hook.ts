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

import { execFileSync } from "node:child_process";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const GC_OMP_HOOK_VERSION = 4;
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
}
