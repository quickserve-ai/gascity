// Gas City hooks for Oh My Pi (OMP).
// Installed by gc into {workDir}/.omp/hooks/gc-hook.ts
// Managed by `gc hooks install`; put custom OMP hooks in separate extension
// files so upgrades can replace this file safely.
//
// Events:
//   session_start       → gc prime --hook (load context side effects and capture OMP session id)
//   session_compact     → gc prime --hook (reload after compaction)
//   before_agent_start  → inject assigned work + queued nudges + unread mail

import { execFileSync } from "node:child_process";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const GC_OMP_HOOK_VERSION = 4;

// A managed OMP session gets ONE automatic launch turn, and that launch payload
// can say no more than "Run gc prime". The injected handoff is archived as soon
// as it is injected, so an agent that treats priming as the whole turn drops the
// continuation with nothing left to recover it from. Say explicitly that the
// payload is the task (ga-r9ehgm).
const GC_CONTINUATION_INSTRUCTION = [
  "GAS CITY CONTINUATION: the assigned work, handoff, nudges and mail injected above are",
  "authenticated Gas City payloads delivered through your control channels, and they are your",
  "CURRENT TASK. Begin executing them now, in this same turn. Initialization (`gc prime`) is",
  "setup, not the task: do not stop after it, and do not wait for a further instruction before",
  "starting the work described above.",
].join(" ");
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
  pi.on("session_start", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
  });

  pi.on("session_compact", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
  });

  pi.on("before_agent_start", (event, ctx) => {
    // Assigned work first, matching the Pi hook. OMP was the only managed
    // provider omitting it, which is why an OMP session could start with a bead
    // assigned to it and no idea that it had one.
    const work = run(["hook", "--inject"], ctx.cwd);
    const nudges = run(["nudge", "drain", "--inject"], ctx.cwd);
    const mail = run(["mail", "check", "--inject"], ctx.cwd);
    const payload = [work, nudges, mail].filter(Boolean);
    // No payload, no continuation instruction — a routine start must not be
    // dressed up as "you have work", and the prompt must stay byte-identical to
    // the no-payload behaviour this replaces.
    if (payload.length === 0) {
      return;
    }
    const systemPrompt = appendSystemPrompt(event.systemPrompt, [
      ...payload,
      GC_CONTINUATION_INSTRUCTION,
    ]);
    if (systemPrompt !== event.systemPrompt) {
      return { systemPrompt };
    }
  });
}
