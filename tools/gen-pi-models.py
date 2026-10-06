#!/usr/bin/env python3
"""gen-pi-models.py - derive pi, OMP and Codex catalogs from `ds4-serve spec` (or $RIG_SPEC).

    ./tools/gen-pi-models.py                # rewrites all three agent catalogs
    ./tools/gen-pi-models.py --stdout       # print pi JSON only
    ./tools/gen-pi-models.py --omp-stdout   # print OMP YAML-compatible JSON only
    ./tools/gen-pi-models.py --codex-stdout # print the Codex model catalog only

CODEX (2026-09-30): Codex reads ~/.codex/local-rig-models.json through
`model_catalog_json` in ~/.codex/config.toml, whose `local-rig` provider points
at llama-swap with wire_api = "responses". Both engines serve /v1/responses
natively, so no translating router sits in between (claude-code-router did, and
went dead when :8080 became SearXNG). The system prompt every local model gets
in Codex is config/codex/base-instructions.md; edit it there and re-run.

WHY STATIC (2026-09-11): ds4-serve's write_pi_config() rewrote models.json on
every launch so pi only ever saw what was loaded, because a bare engine serves
its resident GGUF under ANY model id and a stale catalog silently lied.
llama-swap loads the requested id on demand, so that hazard is gone and every
registry row can be advertised permanently. Same single source of truth as
tools/gen-swap-config.sh: add a row to ds4_registry(), re-run both.

NOTE: `./ds4-serve <target>` still rewrites models.json to its dynamic
per-engine form. Re-run this script afterwards to get back to the rig.
"""
import json, os, pathlib, subprocess, sys

DS4_SERVE = os.path.expanduser(os.environ.get("DS4_DIR", "~/repos/ds4")) + "/ds4-serve"
BASE_URL = os.environ.get("RIG_URL", "http://127.0.0.1:8090/v1")
MAXTOK = 16384
# pi's default model. It was `flash` until 2026-10-01, when the non-vision
# DeepSeek weights and their rows were deleted, then `vision`. Later on 2026-10-01
# `qwen38` (Qwen3.8 Flash Next on ds4, 262K) was chosen after a Flash Next
# head-to-head benchmark. main() refuses to write a default the
# registry no longer has.
PI_DEFAULT = "qwen38"


def model(mid, name, ctx, images=False, reasoning=True):
    # contextWindow = ctx - maxTokens. Output shares the context budget;
    # advertising the full ctx lets pi pack a prompt with no room to generate,
    # which the server rejects mid-run (carried from write_pi_config()).
    m = {"id": mid, "name": name, "reasoning": reasoning,
         "input": ["text", "image"] if images else ["text"],
         "contextWindow": max(ctx - MAXTOK, 1024), "maxTokens": MAXTOK,
         "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}
    return m


def pi_model(mid, name, ctx, images=False, reasoning=True, levels=None):
    m = model(mid, name, ctx, images=images, reasoning=reasoning)
    if reasoning and levels:
        m["thinkingLevelMap"] = levels
    return m


def omp_model(mid, name, ctx, images=False, reasoning=True, levels=None):
    m = model(mid, name, ctx, images=images, reasoning=reasoning)
    if reasoning and levels:
        # OMP's vocabulary replaces pi's thinkingLevelMap. `off` is implicit;
        # requiresEffort=false preserves the server's explicit no-thinking path.
        m["thinking"] = {
            "mode": "effort",
            "efforts": ["minimal", "low", "medium", "high", "xhigh", "max"],
            "defaultLevel": "medium",
            "effortMap": {k: v for k, v in levels.items() if k != "off"},
            "requiresEffort": False,
        }
    return m


# ds4 engine (ds4_server.c:1037-1062, upstream 0aaea5a): minimal/low map to
# DS4_THINK_LOW, medium to DS4_THINK_MEDIUM, high/xhigh to DS4_THINK_HIGH.
# DeepSeek and GLM render LOW and MEDIUM as HIGH, so for them only "none" and
# "max" differ, and "max" needs ctx >= 393216 or it degrades to high. Qwen3.8
# (qwen38) honours low/medium/xhigh; pi's default level, medium, adds no effort
# instruction (2026-09-16). "off" must be non-null or pi hides the ability to
# disable thinking, which the server does support.
DS4_LEVELS = {"off": "none", "minimal": "low", "low": "low", "medium": "medium",
              "high": "high", "xhigh": "xhigh", "max": "xhigh"}

# ONE provider, ONE compat block: the DeepSeek-shaped one from write_pi_config().
# thinkingFormat "deepseek" = reasoning_content on assistant messages, which is
# what DwarfStar speaks; llama-server (Qwen) also returns reasoning_content.
# The old qwen-chat-template format only existed to toggle enable_thinking,
# which is on by default. Verified through pi against both engines via :8090
# on 2026-09-11 (see README "Measured").
PI_COMPAT = {"supportsStore": False, "supportsDeveloperRole": False,
             "supportsReasoningEffort": True, "supportsUsageInStreaming": True,
             "maxTokensField": "max_tokens", "supportsStrictMode": False,
             "thinkingFormat": "deepseek",
             "requiresReasoningContentOnAssistantMessages": True}

# OMP 18 uses the same endpoint but a newer compat vocabulary. Its "openai"
# thinking format is the reasoning_effort field ds4-server accepts; reasoning
# comes back in reasoning_content and must be replayed on tool-call turns.
OMP_COMPAT = {"supportsStore": False, "supportsDeveloperRole": False,
              "supportsReasoningEffort": True, "supportsUsageInStreaming": True,
              "maxTokensField": "max_tokens", "supportsStrictMode": False,
              "thinkingFormat": "openai", "reasoningContentField": "reasoning_content",
              "requiresReasoningContentForToolCalls": True}

CODEX_INSTRUCTIONS = pathlib.Path(__file__).resolve().parent.parent / "config/codex/base-instructions.md"

# Codex sends `reasoning.effort` verbatim; ds4-server parses none, minimal,
# low, medium, high, xhigh and max (ds4_server.c parse_reasoning_effort_name).
# The descriptions say what the engine really does with each, per the
# DS4_LEVELS note above, rather than promising five distinct depths.
def codex_levels(family, ctx):
    same = family in ("deepseek", "glm")
    lv = [("none", "Thinking off"),
          ("low", "Same as high on this model" if same else "Light thinking"),
          ("medium", "Same as high on this model" if same else "Medium thinking"),
          ("high", "Full thinking"),
          ("xhigh", "Same as high on this model" if same else "Extra thinking")]
    if family == "deepseek" and ctx >= 393216:
        lv.append(("max", "Think Max (needs this 393K+ window)"))
    return [{"effort": e, "description": d} for e, d in lv]


def codex_model(i, mid, name, ctx, engine, family, images=False):
    ds4 = engine == "ds4"
    return {
        "slug": mid, "display_name": name, "description": name,
        "base_instructions": CODEX_INSTRUCTIONS.read_text(),
        # Same ctx - maxTokens budget as pi, for the same reason.
        "context_window": max(ctx - MAXTOK, 1024), "max_context_window": max(ctx - MAXTOK, 1024),
        "effective_context_window_percent": 100,
        "input_modalities": ["text", "image"] if images else ["text"],
        # llama-server has no effort knob for Qwen (enable_thinking only), so
        # Codex shows no effort picker for llama rows.
        "supported_reasoning_levels": codex_levels(family, ctx) if ds4 else [],
        "default_reasoning_level": "high" if ds4 else None,
        "default_reasoning_summary": "none",
        # Codex 0.159 only offers apply_patch as a freeform (type "custom")
        # tool. ds4-server round-trips custom_tool_call; llama-server's
        # Responses shim drops every non-"function" tool (server-chat.cpp:
        # "unsupported Responses tool type skipped"), so llama rows get no
        # apply_patch at all (null) and edit files through the shell instead.
        "apply_patch_tool_type": "freeform" if ds4 else None,
        "shell_type": "shell_command",
        # The skills linked into ~/.codex/skills are only advertised when this
        # is on; local models have no code mode to search them with instead.
        "include_skills_usage_instructions": True,
        "support_verbosity": False, "supports_image_detail_original": False,
        "supports_search_tool": False, "web_search_tool_type": "text",
        "truncation_policy": {"mode": "tokens", "limit": 10000},
        "visibility": "list", "supported_in_api": True, "priority": i,
        "additional_speed_tiers": [], "service_tiers": [], "experimental_supported_tools": [],
        "availability_nux": None, "upgrade": None,
    }


def slot_ctx(ctx, args):
    """The context ONE client can use. llama-server with --parallel N and no -kvu
    splits --ctx-size into N separate per-slot streams (n_ctx_seq = ctx / N), so a
    row that serves several agents at once must advertise the per-slot window,
    not the total, or pi packs prompts the slot rejects (2026-10-01)."""
    w = [] if args in ("", "-") else args.split()
    if "-kvu" in w or "--kv-unified" in w:
        return ctx
    n = 1
    for i, x in enumerate(w[:-1]):
        if x in ("--parallel", "-np"):
            n = max(int(w[i + 1]), 1)
    return ctx // n


def read_spec():
    # RIG_SPEC: a registry file in `ds4-serve spec` format (examples/registry.example), for a
    # machine without ds4-serve. Same knob as tools/gen-swap-config.sh.
    if os.environ.get("RIG_SPEC"):
        return pathlib.Path(os.environ["RIG_SPEC"]).read_text()
    return subprocess.run([DS4_SERVE, "spec"], capture_output=True, text=True, check=True).stdout


def main():
    spec = read_spec()
    pi_models = []
    omp_models = []
    codex_models = []
    for line in spec.splitlines():
        if not line.strip() or line.startswith("#"):
            continue
        # Field 12 (args, optional) matters here only for --parallel (see slot_ctx).
        fields = line.split("|")
        target, engine, family, ctx, trust, dl, label, use, gguf, vision, drafter = fields[:11]
        ctx = slot_ctx(int(ctx), fields[11] if len(fields) > 11 else "-")
        images = vision != "-"
        if engine == "ds4":
            levels = dict(DS4_LEVELS)
            # ds4 treats xhigh as high. Select Think Max explicitly when the
            # DeepSeek window can support it; other families keep their map.
            if family == "deepseek" and ctx >= 393216:
                levels.update(xhigh="max", max="max")
            pi_models.append(pi_model(target, f"{label} [{trust}]", ctx, images=images, levels=levels))
            omp_models.append(omp_model(target, f"{label} [{trust}]", ctx, images=images, levels=levels))
        else:
            # Qwen3.8 has a single enable_thinking toggle, no effort levels. A
            # thinkingLevelMap here fails pi's schema and then NO models load.
            pi_models.append(pi_model(target, f"{label} [{trust}]", ctx, images=images))
            omp_models.append(omp_model(target, f"{label} [{trust}]", ctx, images=images))
        codex_models.append(codex_model(len(codex_models), target, f"{label} [{trust}]", ctx,
                                        engine, family, images=images))
    pi_cfg = {"providers": {"local": {"baseUrl": BASE_URL, "api": "openai-completions",
                                       "apiKey": "local-rig", "compat": PI_COMPAT,
                                       "models": pi_models}}}
    # OMP reserves provider "local" for its embedded tiny/TTS/STT models. A
    # distinct id keeps Qwen-named rig entries classified as chat models.
    omp_cfg = {"providers": {"local-rig": {"baseUrl": BASE_URL, "api": "openai-completions",
                                            "auth": "none", "compat": OMP_COMPAT,
                                            "models": omp_models}}}
    pi_text = json.dumps(pi_cfg, indent=2) + "\n"
    omp_text = json.dumps(omp_cfg, indent=2) + "\n"
    codex_text = json.dumps({"models": codex_models}, indent=2) + "\n"
    if "--stdout" in sys.argv:
        sys.stdout.write(pi_text)
        return
    if "--omp-stdout" in sys.argv:
        sys.stdout.write(omp_text)
        return
    if "--codex-stdout" in sys.argv:
        sys.stdout.write(codex_text)
        return
    if PI_DEFAULT not in {m["id"] for m in pi_models}:
        sys.exit(f"gen-pi-models: pi default {PI_DEFAULT!r} is not a registry row; edit PI_DEFAULT")
    pi_dir = pathlib.Path.home() / ".pi/agent"
    pi_tmp = pi_dir / "models.json.tmp"
    pi_tmp.write_text(pi_text)
    pi_tmp.replace(pi_dir / "models.json")
    sp = pi_dir / "settings.json"
    st = json.loads(sp.read_text()) if sp.exists() else {}
    st["defaultProvider"], st["defaultModel"] = "local", PI_DEFAULT
    sp.write_text(json.dumps(st, indent=2) + "\n")
    omp_dir = pathlib.Path.home() / ".omp/agent"
    omp_dir.mkdir(parents=True, exist_ok=True)
    omp_tmp = omp_dir / "models.yml.tmp"
    omp_tmp.write_text(omp_text)
    omp_tmp.replace(omp_dir / "models.yml")
    # Only the catalog is written; the provider and default model live in
    # ~/.codex/config.toml, which Codex itself also rewrites.
    codex_dir = pathlib.Path.home() / ".codex"
    codex_tmp = codex_dir / "local-rig-models.json.tmp"
    codex_tmp.write_text(codex_text)
    codex_tmp.replace(codex_dir / "local-rig-models.json")
    ids = ", ".join(m["id"] for m in pi_models)
    print("pi local/ + OMP local-rig/ + Codex local-rig catalogs -> " + ids + f" (pi default local/{PI_DEFAULT})")


if __name__ == "__main__":
    main()
