// hold.ts — pi waits for the Mac's GPU hold instead of loading its model into someone's render (2026-10-04).
//
// The hold gate (~/repos/local-rig/hold, `hold status`) sits on 127.0.0.1:8090 in front of llama-swap. While a
// render, an m3d stage or the human (`hold on`) holds the GPU, the gate refuses model calls with a 503. This
// extension is the polite side of that rule: before every model call (pi's `context` hook runs before each one,
// retries included), a session whose model is served through the gate asks whether the LLM is free and, if it is
// not, waits with a status line, then sends the call the moment the hold ends. Nothing is lost and nothing needs
// re-sending: a message typed during a 40-minute render simply goes out after it.
//
// Why here and not only in film-rig.ts: film-rig's hook only knew LTX renders and only ran in sessions that load
// it; the rule (2026-10-04) is that nothing may force the model back while the GPU is deliberately held, for
// any session and any reason (renders, m3d, `hold on`). Sessions on other providers (cloud models, a ds4-serve
// engine on :8000) never wait. If the gate does not answer, the call goes ahead: the gate's own refusal (and pi's
// retry, which comes back through this hook) is the backstop.
//
// Compaction calls the model directly, outside the agent loop, so session_before_compact waits the same way.
//
// Symlinked into ~/.pi/agent/extensions/ (global); the film rig's director launcher (local-video, rig/film-pi.sh)
// loads it explicitly next to film-rig.ts.
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import * as path from "node:path";

const GATE = (process.env.HOLD_GATE ?? "http://127.0.0.1:8090").replace(/\/+$/, "");

function hostOf(u: string): string {
  try {
    const x = new URL(u);
    const host = x.hostname === "localhost" ? "127.0.0.1" : x.hostname;
    return `${host}:${x.port || (x.protocol === "https:" ? "443" : "80")}`;
  } catch {
    return "";
  }
}
const GATE_HOST = hostOf(GATE);

// :8090 answered without the hold API (llama-swap itself, before the gate was installed): ask again in a minute,
// not before every call, so llama-swap's log does not fill with 404s.
let noGateUntil = 0;

export function gatedModel(model: unknown): boolean {
  const base = (model as { baseUrl?: string } | undefined)?.baseUrl;
  return !!base && hostOf(base) === GATE_HOST;
}

export function fmtDuration(ms: number): string {
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  return m < 60 ? `${m}m${String(s % 60).padStart(2, "0")}s` : `${Math.floor(m / 60)}h${String(m % 60).padStart(2, "0")}m`;
}

interface Phase { phase?: string; why?: string }

async function ask(who: string, waitS: number, signal?: AbortSignal): Promise<Phase | "no-gate" | "down"> {
  const timeout = AbortSignal.timeout((waitS + 15) * 1000);
  const sig = signal ? AbortSignal.any([signal, timeout]) : timeout;
  try {
    const res = await fetch(`${GATE}/hold/wait-open?timeout=${waitS}&who=${encodeURIComponent(who)}`, { signal: sig });
    if (res.status === 404) return "no-gate";
    if (!res.ok) return "down";
    return (await res.json()) as Phase;
  } catch {
    return "down";
  }
}

export default function hold(pi: ExtensionAPI) {
  let who = `pi:${process.pid}`;

  async function waitForTheLLM(ctx: ExtensionContext, where: string): Promise<void> {
    if (!gatedModel(ctx.model) || Date.now() < noGateUntil) return;
    const t0 = Date.now();
    let told = false;
    while (!ctx.signal?.aborted) {
      // first look without waiting (an open gate costs one local round trip), then long-poll
      const r = await ask(who, told ? 30 : 0, ctx.signal);
      if (r === "no-gate") {
        noGateUntil = Date.now() + 60_000;
        break;
      }
      if (r === "down" || r.phase === "open") break;
      if (!told && ctx.hasUI) {
        ctx.ui.notify(`hold: the GPU is held (${r.why}). Your message is kept and goes out by itself when the hold ends.`, "warning");
      }
      told = true;
      if (ctx.hasUI) ctx.ui.setStatus("hold", `⏸ LLM held · ${r.why} · waiting ${fmtDuration(Date.now() - t0)}`);
    }
    if (told && ctx.hasUI) {
      ctx.ui.setStatus("hold", undefined);
      ctx.ui.notify(`hold: the GPU is free after ${fmtDuration(Date.now() - t0)}; continuing (${where})`, "info");
    }
  }

  pi.on("session_start", async (_e, ctx) => {
    let name: string | undefined;
    try { name = pi.getSessionName?.(); } catch { /* older pi */ }
    who = `pi:${name || path.basename(ctx.cwd ?? "") || "session"}:${process.pid}`;
  });
  pi.on("context", async (_e, ctx) => { await waitForTheLLM(ctx, "model call"); });
  pi.on("session_before_compact", async (_e, ctx) => { await waitForTheLLM(ctx, "compaction"); });
}
