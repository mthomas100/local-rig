# hold — one lock for this Mac's GPU (2026-10-04)

**Status: live since 2026-10-04 21:30** (`hold/switch-over.sh in`, at a quiet moment after a film
render finished). Checked live the same evening, through pi and the real qwen38: a prompt through the
gate; `hold on` unloaded qwen38, and for 24 s curl and `/upstream/qwen38/logs` got 503, `decide gpu` ([local-decision](https://github.com/mthomas100/local-decision)) and `m3d gpu`
([local-3d](https://github.com/mthomas100/local-3d)) reported the hold, llama-swap received 0 model requests and a pi prompt waited, then answered 9 s after `hold off`;
Codex's `/v1/responses` calls were refused (it retried, honouring Retry-After) and loaded nothing; the 2-clip
rig-smoke render through the new render_and_wait held the GPU 1m49s with a pi prompt sent mid-render waiting
throughout and answering 17 s after the release; an `m3d concept` stage did the same in 25 s. pi sessions that were
already open need `/reload` (or a restart) to wait instead of being refused.

## Why

This Mac can do one heavy thing at a time. A loaded local LLM (qwen38 wires about 80 GiB) and an LTX render, an
image model or an m3d stage do not fit together, and an LLM prompted beside a render slows it about 9x (film rig,
2026-09-23). Before this, the only guard was film-rig.ts's pi hook: it held pi's model calls while an LTX render
process existed. It knew nothing of m3d or of an unload done on purpose, it started only once the render process
existed (a busy session could reload the model in the gap after the unload and stall the film), and it bound pi
alone: Codex, Local Deep Research and curl went straight to llama-swap, which loads whatever it is asked for.

The rule (2026-10-04): when the LLM is held off on purpose, nothing may force it back on, whatever the client
and whatever the reason. A request made meanwhile should simply wait and go out when the LLM is back. A request made
while the model is loaded should use it, not swap it.

## The model

One lock. Model calls share it while it is free; a GPU job (a render, an m3d stage, `hold on`) takes it alone.

- **The gate** (`hold gate`, launchd `com.example.local-rig.hold-gate`) listens on 127.0.0.1:8090, the address every
  client already uses, and passes everything through to llama-swap, which moved to 127.0.0.1:8091.
- **Phases.** `open`: model calls pass. `draining`: a hold is next; new model calls are refused, calls already in
  flight finish (up to the drain timeout, 5 min, then the unload cuts them), then llama-swap is unloaded and the hold
  is granted. `held`: a hold is granted, or a GPU job runs outside any hold.
- **Holds queue first come, first served.** A second render or m3d job waits its turn instead of being refused.
- **A hold dies with its processes** (pid plus start time, checked every 2 s), so a crashed job cannot keep the LLM
  off. `hold on` holds are manual and last until `hold off` (or `--for`). A reboot drops every hold.
- **Nesting.** A process inside a granted hold (HOLD_ID in its environment, or an ancestor owns the hold) joins it
  instead of queueing behind it: run-queue.sh > story.sh > vidgen > ltx-2-mlx is one hold.
- **GPU jobs that took no hold** (a render by hand, an old script: ltx-2-mlx, story.sh, run-queue.sh, redo.sh,
  vidgen, bin/still, mflux-generate, hy3d generate/shape/paint, comfy3d.py, skintokens-cli) close the gate too; the
  model is unloaded once the calls in flight finish. Long-running servers (ComfyUI, mlx-serve, the dictation daemon)
  do not count: alive is not busy.
- **While held**, the gate checks llama-swap now and then; a model loaded behind its back (someone on :8091) is
  unloaded again.

## Who waits, who is refused

- **pi** (`extensions/hold.ts`, global, and loaded by `rig/film-pi.sh`): before every model call of a session whose
  model is served through the gate, it asks the gate and waits with a status line (`⏸ LLM held · …`). A message
  typed during a 40-minute render goes out by itself when the render ends; a hold taken between two of an agent's
  steps pauses it between them. Sessions on other providers never wait.
- **Everything else** (Codex, LDR, curl, `/upstream/<model>/…`): a 503 `llm_held` whose message names the holder.
  Nothing is sent to the model, so nothing loads. pi without the extension retries three times and then shows that
  message.
- **Always passed through**: /running, /unload, /v1/models (GET), /logs*, /ui*, /health, GET /api/*, the unload and
  inflight-cancel APIs.

## Commands

```
hold                         status: open / draining / held, by whom, what is in flight and waiting
hold on [--for 2h] "why"     hold the LLM off on purpose (waits for calls in flight, unloads, holds)
hold off [id]                end it
hold run [--kind K] [--reason R] -- <cmd...>   run a GPU job under a hold (exec: the hold ends with the job)
hold acquire --pid N --kind K --reason R [--json]   for programs; prints the id once granted
hold release <id>
hold wait [--timeout 10m]    block until model calls may pass
```

`HOLD_GATE` overrides the gate address (tests use :18090). Exit 3 means the gate did not answer; `hold run` then
runs the command anyway with a warning (a missing gate also means no model can be loaded through :8090).

## Who takes holds

- film rig ([local-video](https://github.com/mthomas100/local-video)): render_and_wait and redo_scenes take the hold before they unload (so no session
  can reload the model in the gap) and keep it through the post-render checks; run-queue.sh, redo.sh, vidgen and
  bin/still take one when run by hand; story.sh takes one at its first GPU step (keep_take's re-stitch runs it with
  nothing to render and must not unload the model).
- m3d ([local-3d](https://github.com/mthomas100/local-3d)): every GPU stage. `--yield-llm` is implied; it still matters for a `model <target>`
  engine outside llama-swap, which the gate does not control.

## Files

- `hold/` — Go source (`gate.go` the gate, `procs.go` process table and GPU-job match, `main.go` the CLI), unit
  tests against a stand-in llama-swap (`go test ./...`), `build.sh`, and `e2e/run.sh`, the end-to-end test with real
  pi (a test gate on :18090 in front of `e2e/stub.py` on :18091; nothing live is touched).
- `extensions/hold.ts` — pi's side.
- `launchd/com.example.local-rig.hold-gate.plist` — the gate; `launchd/com.example.local-rig.llama-swap.plist` listens on
  :8091. Both are templates: `tools/install-launchd.sh` renders them for your machine.
- State: `~/.local/state/hold/state.json` (holds survive a gate restart). Log: `~/.ds4/hold-gate.log`, next to
  llama-swap's.

## Going back

`hold/switch-over.sh out`, or by hand: bootout `com.example.local-rig.hold-gate`, set llama-swap's plist back to
`--listen 127.0.0.1:8090`, bootstrap it. Every
client works as before; film-rig.ts and m3d fall back to their own unload-and-settle when `hold` cannot reach a gate.
