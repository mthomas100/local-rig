#!/usr/bin/env python3
"""codex-smoke.py - prove Codex can work through the rig, one real task per model.

    ./tools/codex-smoke.py                 # one model per distinct engine build
    ./tools/codex-smoke.py qwen27-262k vision-500k   # exactly these registry ids

Each model gets the same small task in a fresh temp directory: write notes.txt
containing "rig-ok", cat it, and report what it printed. A model passes when the
file is right AND its final answer quotes it. That takes at least two tool calls
and three requests, so it also exercises Codex replaying the model's earlier
reasoning and tool output, which a one-shot curl never does (see "Verify through
pi, never curl" in AGENTS.md; the same applies to Codex).

Runs `codex exec` against a throwaway CODEX_HOME holding only the provider and
model catalog read from ~/.codex/config.toml, so test sessions stay out of the
user's own Codex hooks and history. Every model listed is loaded in turn,
which evicts whatever is loaded now. Results: ~/.cache/local-rig/codex-smoke/.
Exit 0 only if every model passed.
"""
import json, os, pathlib, subprocess, sys, tempfile, time, tomllib, urllib.request

DS4_SERVE = os.path.expanduser(os.environ.get("DS4_DIR", "~/repos/ds4")) + "/ds4-serve"
CODEX_CFG = pathlib.Path.home() / ".codex/config.toml"
OUT = pathlib.Path.home() / ".cache/local-rig/codex-smoke"
TASK = ("Create a file named notes.txt whose entire content is the single line: rig-ok\n"
        "Then run `cat notes.txt` and reply with exactly what it printed.")
TIMEOUT = 1200  # a cold load plus Codex's first prefill on a big model


def default_models():
    # One row per distinct launch: engine, family and llama-server binary
    # (field 12's LLAMA_BIN). Rows that differ only in context window share a
    # code path, so testing each would only add model loads.
    seen, picks = set(), []
    spec = subprocess.run([DS4_SERVE, "spec"], capture_output=True, text=True, check=True).stdout
    for line in spec.splitlines():
        if not line or line.startswith("#"):
            continue
        f = line.split("|")
        args = f[11] if len(f) > 11 else ""
        binary = next((w for w in args.split() if w.startswith("LLAMA_BIN=")), "")
        key = (f[1], f[2], binary)
        if key not in seen:
            seen.add(key)
            picks.append(f[0])
    return picks


def codex_home(cfg):
    prov = cfg["model_provider"]
    p = cfg["model_providers"][prov]
    lines = [f'model_provider = "{prov}"',
             f'model_catalog_json = "{cfg["model_catalog_json"]}"',
             f'model_reasoning_effort = "{cfg.get("model_reasoning_effort", "high")}"',
             f'[model_providers.{prov}]']
    lines += [f"{k} = {json.dumps(v)}" for k, v in p.items()]
    home = pathlib.Path(tempfile.mkdtemp(prefix="codex-smoke-home-"))
    (home / "config.toml").write_text("\n".join(lines) + "\n")
    return home, p["base_url"]


def run(model, env, work):
    d = pathlib.Path(tempfile.mkdtemp(prefix=f"{model}-", dir=work))
    t0, items, final, err, usage, rc = time.time(), [], "", "", None, None
    try:
        p = subprocess.run(["codex", "exec", "--json", "--skip-git-repo-check",
                            "--sandbox", "workspace-write", "-m", model, TASK],
                           cwd=d, env=env, stdin=subprocess.DEVNULL,  # codex exec reads an open stdin
                           capture_output=True, text=True, timeout=TIMEOUT)
        rc = p.returncode
        (d / "events.jsonl").write_text(p.stdout)
        (d / "stderr.txt").write_text(p.stderr)
        for line in p.stdout.splitlines():
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            it = ev.get("item") or {}
            if ev.get("type") == "item.completed":
                items.append(it.get("type"))
                if it.get("type") == "agent_message":
                    final = it.get("text", "")
            elif ev.get("type") in ("error", "turn.failed"):
                err = json.dumps(ev)[:300]
            elif ev.get("type") == "turn.completed":
                usage = ev.get("usage")
    except subprocess.TimeoutExpired:
        rc = "timeout"
    f = d / "notes.txt"
    content = f.read_text().strip() if f.exists() else None
    return {"model": model, "ok": content == "rig-ok" and "rig-ok" in final, "rc": rc,
            "secs": round(time.time() - t0), "file": content, "final": final.strip()[:80],
            # file_change = Codex's apply_patch; command_execution = shell.
            "items": items, "usage": usage, "error": err, "dir": str(d)}


def main():
    cfg = tomllib.loads(CODEX_CFG.read_text())
    home, base = codex_home(cfg)
    try:
        urllib.request.urlopen(base.rstrip("/") + "/models", timeout=5).read()
    except OSError as e:
        sys.exit(f"codex-smoke: {base} is not answering ({e}); is llama-swap running?")
    models = sys.argv[1:] or default_models()
    OUT.mkdir(parents=True, exist_ok=True)
    log = OUT / time.strftime("%Y-%m-%dT%H-%M-%S.jsonl")
    work = pathlib.Path(tempfile.mkdtemp(prefix="codex-smoke-work-"))
    env = dict(os.environ, CODEX_HOME=str(home))
    fails = 0
    for m in models:
        row = run(m, env, work)
        fails += not row["ok"]
        with log.open("a") as fh:
            fh.write(json.dumps(row) + "\n")
        print(f'{"PASS" if row["ok"] else "FAIL"} {m:16} {row["secs"]:4}s  {" ".join(row["items"])}'
              + (f'  {row["error"] or row["final"]}' if not row["ok"] else ""), flush=True)
    print(f"{len(models) - fails}/{len(models)} passed; log {log}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
