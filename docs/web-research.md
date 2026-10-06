# Web research for pi on the local model (optional)

pi ships with no web tools. This is the setup that gives it search, page reading and deep research while
staying self-hosted: the only things that leave the machine are the queries SearXNG sends to public search
engines and the pages pi fetches. There are no API keys and no accounts. None of it is needed for the rig, the
hold gate or `/pcv`; skip this page if you only want those.

```
pi  (model from llama-swap, through the hold gate on :8090)
 ├─ extensions/today.ts              today's date in the system prompt
 ├─ extensions/web-research-rules.ts the core research rules, always in the prompt when web_search is on
 ├─ skills/web-research              the full procedure (read on demand)
 ├─ pi-web-access  (npm, pinned)     web_search, fetch_content, get_search_content
 │     └─ config/pi/web-search.json  SearXNG only, local page fetching only, no browser cookies
 ├─ bash                             gh / npm / PyPI / Wikipedia / HN APIs
 └─ a deep-research skill ──► Local Deep Research  (tools/ldr-run.sh)
                                  ├─► SearXNG
                                  └─► llama-swap (through the gate)
SearXNG  (tools/searxng-run.sh) ──► public engines chosen in config/searxng/settings.yml
```

## Where each piece lives

| Piece | Source of truth | Installed as |
|---|---|---|
| SearXNG settings | `config/searxng/settings.yml` | read via `SEARXNG_SETTINGS_PATH` |
| SearXNG launcher | `tools/searxng-run.sh` (port: `SEARXNG_PORT`, default 8080) | `tools/install-launchd.sh searxng` |
| SearXNG code | a [SearXNG](https://github.com/searxng/searxng) checkout and its `.venv` (`SEARXNG_SRC`, default `~/repos/searxng`) | not versioned here |
| SearXNG secret | `~/.config/searxng/secret_key` (0600, made on first run) | never in git |
| Search health | `tools/searxng-health.sh` | run by hand or by the skill |
| LDR launcher | `tools/ldr-run.sh` (port: `LDR_WEB_PORT`, default 5055) | `tools/install-launchd.sh ldr` |
| LDR code | a [Local Deep Research](https://github.com/LearningCircuit/local-deep-research) checkout (`LDR_SRC`, default `~/repos/local-deep-research`) | not versioned here |
| pi web extension | [`pi-web-access`](https://www.npmjs.com/package/pi-web-access), version pinned | `pi install npm:pi-web-access@0.31.0` |
| its config | `config/pi/web-search.json` | symlinked to `~/.pi/agent/web-search.json` |
| date and rules | `extensions/today.ts`, `extensions/web-research-rules.ts` | symlinked into `~/.pi/agent/extensions/` |
| research skill | `skills/web-research/SKILL.md` | symlinked into `~/.pi/agent/skills/` |

## Setting it up

```sh
# SearXNG, natively (no Docker VM next to a resident model)
git clone https://github.com/searxng/searxng.git ~/repos/searxng
cd ~/repos/searxng
uv venv .venv --python 3.12
uv pip install --python .venv/bin/python -r requirements.txt -r requirements-server.txt setuptools wheel pybind11
uv pip install --python .venv/bin/python --no-build-isolation -e .
cd ~/repos/local-rig
tools/install-launchd.sh searxng
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.local-rig.searxng.plist
tools/searxng-health.sh

# pi side
pi install npm:pi-web-access@0.31.0
ln -s ~/repos/local-rig/config/pi/web-search.json ~/.pi/agent/web-search.json
ln -s ~/repos/local-rig/extensions/today.ts ~/.pi/agent/extensions/today.ts
ln -s ~/repos/local-rig/extensions/web-research-rules.ts ~/.pi/agent/extensions/web-research-rules.ts
ln -s ~/repos/local-rig/skills/web-research ~/.pi/agent/skills/web-research

# Local Deep Research (optional; its server settings come from LDR_* lines in ~/.config/ldr-pi.env)
tools/install-launchd.sh ldr
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.local-rig.ldr.plist
```

In LDR's web UI, Settings, LLM: provider `openai_endpoint`, URL `http://127.0.0.1:8090/v1`, and a registry id whose
served context holds LDR's final synthesis prompt plus its max tokens (for example `vision-q4-400k`). LDR does not
trim that prompt to fit: its context-window setting only feeds its token warnings. With three iterations and full
page content the prompt passed 100,000 tokens, so a 100K row fails at the last step. Search: `searxng`, result
format `json`.

Port 5055, not LDR's default 5000: macOS AirPlay Receiver holds port 5000 on both IPv4 and IPv6, so a default LDR
either fails to bind or is shadowed.

## Day to day

```sh
tools/searxng-health.sh                                              # OK / DEGRADED / DOWN, and which engines answer
launchctl kickstart -k gui/$(id -u)/com.example.local-rig.searxng    # restart (also clears engine suspensions)
launchctl kickstart -k gui/$(id -u)/com.example.local-rig.ldr
tail -f ~/.ds4/searxng.log ~/.ds4/ldr.log
```

## What was measured (2026-09-23)

- **Engines.** Each engine in `settings.yml` is on or off because of what it answered from this Mac on
  2026-09-23; the comments there say why. After a sustained agent load (a nine-question benchmark, twice) only bing
  and yahoo were still answering, so four engines that query from their own indexes were added. Re-measure with
  `tools/searxng-health.sh` rather than trusting the list on another network.
- **The always-on rules** (`web-research-rules.ts`) changed behaviour more than speed: on the same eight benchmark
  questions, GitHub questions went straight to `gh api`, and no run with the rules or the skill sent a URL through
  a public proxy (the run with neither did, five times). Total time was within run-to-run noise.
- **The date line** (`today.ts`) exists because in the baseline run the model ran `date` on its own in some tasks
  and not in others. It is a date, not a time, so it does not break the server's prompt-prefix cache.

The benchmark harness and its per-task results are not part of this export.
