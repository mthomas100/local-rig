# local-rig — the llama-swap rig, the hold gate and the /pcv multi-agent loop (canonical file; `CLAUDE.md` is a symlink to this)

This is the instruction file coding agents (Claude Code, Codex, pi) read before touching the repo. Most of the
rig was built and is maintained by agents working from this file, so it states the rules they keep. It is a code
repo with a version-control contract: **commit atomically, as changes are made** — never one bulk "dump
everything" commit at the end of a session. The commit discipline is Rule 1 below and the hooks enforce it.

## Layout

```
config/llama-swap.yaml   GENERATED, gitignored — never hand-edit, never commit
tools/gen-swap-config.sh ds4-serve spec (or $RIG_SPEC) -> llama-swap.yaml
tools/gen-pi-models.py   the same registry -> pi models.json (`local`) + OMP models.yml (`local-rig`)
                         + Codex ~/.codex/local-rig-models.json (provider `local-rig`)
examples/registry.example  a registry in ds4-serve spec format, for machines without ds4-serve
config/codex/base-instructions.md  the system prompt Codex gives every rig model; re-run gen-pi-models.py after editing
tools/verify.sh          the deterministic gate (exit 0 pass / 1 fail / 2 no verifier)
tools/codex-smoke.py     one real Codex task per engine build through :8090 (loads each model)
tools/install-launchd.sh renders launchd/*.plist (placeholders __HOME__, __RIG__, ...) into ~/Library/LaunchAgents
agents/{planner,coder,reviewer}.md   source of truth; symlinked into ~/.pi/agent/agents/
prompts/pcv.md           /pcv workflow; symlinked into ~/.pi/agent/prompts/
hold/                    the hold gate: one GPU lock in front of llama-swap (Go; `hold` CLI, unit tests, e2e/run.sh with real pi); docs/hold.md
extensions/hold.ts       pi waits for the GPU hold instead of loading its model into a render or an m3d job
launchd/com.example.local-rig.hold-gate.plist   the gate on :8090; llama-swap's plist listens on :8091 behind it (hold/switch-over.sh out goes back)
launchd/com.example.local-rig.llama-swap.plist
launchd/com.example.local-rig.{searxng,ldr}.plist   optional web research services (docs/web-research.md)
config/searxng/settings.yml   SearXNG engine set, measured from this Mac
config/pi/web-search.json     pi-web-access config; symlinked into ~/.pi/agent/
tools/searxng-{run,health}.sh, tools/ldr-run.sh   launchers and the search health check
extensions/{today,web-research-rules}.ts   pi extensions (date, web-research rules); symlinked into ~/.pi/agent/extensions/
skills/web-research/     pi skill; symlinked into ~/.pi/agent/skills/
hooks/                   the atomic-commit gate (pre-commit, commit-msg)
docs/hold.md             the hold gate's design, commands and live checks
docs/web-research.md     pi's self-hosted web research: SearXNG, pi-web-access, LDR
docs/media/, docs/data/  README charts, the live terminal session and the data they are drawn from (docs/charts/)
```

`CLAUDE.md` is a symlink to this file. There is one source of truth; never create a divergent copy.

## Rule 1 — Commit atomically, as changes are made (the version-control contract)

This repo's history is the audit trail for the rig. Keep it one-logical-change per commit, and keep the working
tree clean on handoff.

- **Commit at the end of each change, not at the end of the session.** One logical change per commit; a "commit
  everything at once" commit is the failure mode this rule exists to stop.
- **Commit message convention:** `local-rig: <short description>`. Examples: `local-rig: add a --dry-run flag to
  tools/verify.sh`, `local-rig: switch reviewer to qwen27 in agents/reviewer.md`. hooks/commit-msg enforces the
  prefix.
- **Never leave uncommitted work across sessions.** Before ending a write session: `git status --short`, commit
  what is finished, and say what is intentionally left uncommitted (and why).
- **Commit generated artifacts? No.** `config/llama-swap.yaml` is generated and gitignored; pi's `models.json` and
  OMP's `models.yml` are generated and live outside this repo. The generators are the source of truth — commit
  *generator changes*, never their output.
- **A moving value never goes in prose:** a port, a model row, a measured number belongs in the code or README at
  its real home, read live — not copied into a commit message or a note that can drift.

## Rule 2 — `config/llama-swap.yaml` is GENERATED, never hand-edited

The model registry holds the single source of truth for every model: 11 pipe-separated fields plus an optional
field 12 for launch arguments and leading environment assignments. On the author's Mac it is `ds4_registry()` in
`ds4-serve`, a launcher script in a local [ds4](https://github.com/antirez/ds4) checkout (`./ds4-serve spec`
prints it); anywhere else, a file in the same format named by `RIG_SPEC` (see `examples/registry.example`).
`tools/gen-swap-config.sh` turns that into the llama-swap YAML. **Do not edit the YAML** — it is gitignored and
overwritten. To add a model: add one row, re-run the generator. With `-watch-config` (always pass it; the plist
does) llama-swap hot-reloads within 2 s. Regenerate, wait for `configuration reloaded` in the log and `/running`
to settle, then use it. Then re-run `tools/gen-pi-models.py` so the pi, OMP and Codex catalogs match.

Generator knobs (env, read by `gen-swap-config.sh`): `RIG_SPEC` (registry file), `RIG_ROUTER=group|matrix`
(default `matrix`), `RIG_RESIDENT` (default derived: llama rows with no vision, no drafter and no more context than
their base row; none qualifies in the current registry, so nothing is preloaded), `RIG_PAIR=1` (matrix only: also
permit flash beside the resident worker), `DS4_DSPARK=1`, `DS4_DIR` for the ds4 checkout.

## Rule 3 — The /pcv loop is defined here, run through pi

`/pcv <task>` runs one subagent chain, planner → coder → reviewer → coder, each stage its own short-lived `pi`
process and every hand-off a file (`plan.md`, `verify.log`, `review.md`). It must use the subagent extension's
`chain` parameter and NEVER `tasks` (parallel): chain is an awaited loop that keeps exactly one child alive, which
is the memory guarantee on a machine where every model runs alone. `agents/*.md` are the source of truth for each
role and are symlinked into `~/.pi/agent/agents/`; `prompts/pcv.md` into `~/.pi/agent/prompts/`. Edit here, and
the symlinks pick it up.

## Rule 4 — The hold gate owns :8090

Every client talks to `127.0.0.1:8090`, which is the hold gate (`hold/`, docs/hold.md); llama-swap listens on
`:8091` behind it. Never point a client at :8091 directly: that bypasses the GPU lock, and the gate unloads a model
loaded behind its back anyway. Rebuild with `hold/build.sh` (vet, tests, build); restart the gate only while
`hold status` says open and nothing is in flight, because a restart cuts streams that pass through it.

## Things that must not change (measured 2026-09-11)

- **`unloadTimeout: 180` globally, `240` on ds4 models.** Default is 10 s then SIGKILL; a SIGKILL on a process
  holding a large wired Metal allocation leaks it at kernel level until reboot. Never lower.
- **Budget against 107.52 GiB, not 128** (`max_recommended_working_set_size`); **no pair fits** — every model
  runs ALONE under the default `matrix` router. `RIG_PAIR=1` re-enables the flash+qwen pair only for re-measuring.
- **The resident worker is `--parallel 1 -ctk q8_0 -ctv q8_0 -fa auto`, no mmproj, no drafter.**
- **Verify through pi, never curl.** A 67-token curl passed on a config that died on pi's real system prompt.
- **Chain, never parallel** (Rule 3).
- **Scripted `pi -p` calls need `< /dev/null`**; pi blocks forever reading an open non-TTY stdin.
- **No `concurrencyLimit` on models:** over the limit llama-swap answers 429 (not queued), and an orphaned request
  holds the slot for its whole generation.

## Relationship to `ds4-serve`

`ds4-serve` can also run one model by itself on its own port, outside llama-swap. Only one of the two paths can be
live at a time (memory, and ds4-server's lock file). pi's `~/.pi/agent/models.json` and OMP's
`~/.omp/agent/models.yml` are **static** (`local` and `local-rig`, both → :8090), generated by
`tools/gen-pi-models.py`. OMP's distinct provider name avoids its reserved `local` namespace for embedded
tiny/speech models. Running `./ds4-serve <target>` rewrites pi's catalog back to its dynamic per-engine form;
re-run `tools/gen-pi-models.py` to return to the rig.

## Version control hygiene (in addition to Rule 1)

- `config/llama-swap.yaml` and `*.log` are gitignored. The pre-commit hook blocks staging them if they slip in.
- The hooks live in `hooks/`, wired via `git config core.hooksPath hooks` (so they are versioned, unlike
  `.git/hooks`). Run `git config core.hooksPath hooks` after cloning.
