#!/bin/sh
# gen-swap-config.sh — derive config/llama-swap.yaml from `ds4-serve spec`.
#
#   ./tools/gen-swap-config.sh > config/llama-swap.yaml
#   llama-swap --config config/llama-swap.yaml -validate
#
# WHY THIS EXISTS (2026-09-11): ds4-serve (a launcher script in a local ds4 checkout) holds THE single source
# of truth for every local model (its ds4_registry(), 11 pipe-separated
# fields and an optional 12th). llama-swap needs the same list in its own
# YAML. Hand-maintaining a second list would drift the day someone adds a row
# to one and not the other, so the YAML is GENERATED from `ds4-serve spec` and
# is .gitignored. To add a model: add ONE row to ds4_registry(), re-run this
# script. Without ds4-serve, point RIG_SPEC at a file in the same format
# (examples/registry.example). Nothing here needs to change unless a new *engine* or *family*
# appears (see the two case blocks). A llama row can select a checkpoint-
# specific executable with LLAMA_BIN=/absolute/path in field 12.
#
# Registry columns (from ds4-serve's maintenance contract):
#   1 target  2 engine(ds4|llama)  3 family(deepseek|glm|qwen|qwen4)  4 ctx
#   5 trust   6 dl  7 label  8 use  9 gguf  10 vision|-  11 drafter|-
#   12 args|- (optional; leading NAME=value words -> env:, the rest -> cmd)
#
# Knobs (env):
#   DS4_DIR        ds4 checkout             (default ~/repos/ds4)
#   RIG_SPEC       read the registry from this file instead of `$DS4_DIR/ds4-serve spec`
#   RIG_ROUTER     group | matrix           (default: matrix — STEP 4/5: glm53+qwen OOMs under llama-swap)
#   RIG_PAIR=1     matrix only: ALSO permit flash + the resident worker to run
#                  together. OFF by default: measured 2026-09-12 (twice) — with
#                  qwen27-65k resident, flash's 5.2k-token TOOLS prefill dies
#                  with kIOGPUCommandBufferCallbackErrorOutOfMemory. The two
#                  processes alone wire 107.5 GiB, i.e. the whole Metal
#                  ceiling, so any prefill past prefill_cap=4096 has nowhere
#                  to grow. Turn on only after a real pi agent run proves it.
#                  Needs the Q2 `flash` row: its weights were deleted on
#                  2026-10-01, so RIG_PAIR=1 now stops with an error.
#   RIG_RESIDENT   space-separated targets that stay loaded beside a big model
#                  (default: derived — every llama-engine row with NO vision, NO
#                  drafter, no more context than its base row, and at most a
#                  65K context, i.e. the lean rows built for co-residency;
#                  none since qwen27-65k was removed on 2026-10-02)
#   KV_DISK_MB     ds4 --kv-disk-space-mb for every ds4 row (default 65536)
#   DS4_DSPARK=1   attach the DeepSeek DSpark drafter (--dspark --mtp-model).
#                  OFF by default, exactly like ds4-serve: measured 150.1 s vs
#                  0.67 s on a 79.8k-ctx prefix-cache hit (ds4-serve, 2026-09-06).
set -eu

DS4_DIR=${DS4_DIR:-$HOME/repos/ds4}
GGUF=$DS4_DIR/gguf
DS4_BIN=$DS4_DIR/ds4-server
LLAMA_BIN=$DS4_DIR/third_party/llama.cpp/llama-server
RIG_ROUTER=${RIG_ROUTER:-matrix}
# The models the /pcv roles run on (`model: local/<target>` in agents/*.md),
# read so that a role's model change carries its ttl with it. This was a
# hard-coded `flash` until that row's Q2 weights were deleted on 2026-10-01.
RIG_DIR=$(cd "$(dirname "$0")/.." && pwd)
LOOP_MODELS=$(sed -n 's|^model: local/||p' "$RIG_DIR"/agents/*.md | sort -u | tr '\n' ' ')

# Registry rows, comment line stripped. Captured once because we walk it twice
# (models, then groups) and a `spec | while read` loop would run in a subshell.
if [ -n "${RIG_SPEC:-}" ]; then
    [ -f "$RIG_SPEC" ] || { echo "gen-swap-config: RIG_SPEC=$RIG_SPEC not found" >&2; exit 1; }
    SPEC=$(grep -v '^#' "$RIG_SPEC" | grep -v '^[[:space:]]*$')
else
    [ -x "$DS4_DIR/ds4-serve" ] || { echo "gen-swap-config: $DS4_DIR/ds4-serve not found (or set RIG_SPEC)" >&2; exit 1; }
    SPEC=$("$DS4_DIR/ds4-serve" spec | grep -v '^#')
fi

# Lean llama rows = the resident set, unless overridden. Vision (mmproj 0.87
# GiB) and the MTP drafter (1.28 GiB) are exactly what a co-resident worker
# must NOT carry (measured in the co-residency tests, 2026-09-11), so "no vision, no drafter"
# is the honest definition of "built to sit beside flash". Nor a bigger KV
# cache: a row above 65K, or with more context than its base row (the first row
# with the same weights), is a long-context variant you ask for, and preloading
# it at every llama-swap start would wire that whole cache for nothing.
if [ -z "${RIG_RESIDENT:-}" ]; then
    RIG_RESIDENT=$(printf '%s\n' "$SPEC" | awk -F'|' '
        !($9 in base) { base[$9] = $4 }
        $2=="llama" && $10=="-" && $11=="-" && $4+0 <= 65536 && $4+0 <= base[$9]+0 {printf "%s ", $1}')
fi
# Every target outside the deliberately lean resident set is exclusive. This
# includes full llama models with vision, drafters, or large contexts—not just
# ds4 rows. Omitting those llama rows from the router would allow an unsafe
# co-resident launch despite the rig's "every model alone" contract.
BIG=""
for _t in $(printf '%s\n' "$SPEC" | awk -F'|' '{print $1}'); do
    case " $RIG_RESIDENT " in
    *" $_t "*) ;;
    *) BIG="$BIG${BIG:+ }$_t" ;;
    esac
done

in_list() { case " $2 " in *" $1 "*) return 0 ;; esac; return 1; }
yaml_list() { printf '['; _s=""; for _x in $1; do printf '%s%s' "$_s" "$_x"; _s=", "; done; printf ']'; }

# row_launch <target> <args field>: registry field 12, split and checked
# exactly as ds4-serve's row_launch() does, into row_env (the leading
# NAME=value words) and row_args (the rest). A row ds4-serve would refuse to
# launch fails here too, so the two launchers never disagree about a row.
row_launch() {
    row_env=""; row_args=""
    _x=$2
    [ "$_x" != "-" ] || _x=""
    if [ -n "$(printf '%s' "$_x" | tr -d ' A-Za-z0-9_=.,:/+@%-')" ]; then
        echo "gen-swap-config: $1: registry args may hold only letters, digits, spaces and _=.,:/+@%-: $_x" >&2
        exit 1
    fi
    case " $_x " in
    *" -m "*|*" --model "*|*" -c "*|*" --ctx "*|*" --ctx-size "*|*" --port "*|*" --vision "*|*" --mmproj "*)
        echo "gen-swap-config: $1: registry args may not set -m, -c/--ctx/--ctx-size, --port, --vision or --mmproj: $_x" >&2
        exit 1 ;;
    esac
    for _w in $_x; do
        case "$_w" in
        [A-Za-z_]*=*)
            if [ -z "$row_args" ] && [ -z "$(printf '%s' "${_w%%=*}" | tr -d 'A-Za-z0-9_')" ]; then
                row_env="$row_env${row_env:+ }$_w"; continue
            fi ;;
        esac
        row_args="$row_args${row_args:+ }$_w"
    done
}

# without_flags <flags> <row args>: <flags> minus every flag the row's args
# also set, with that flag's value. The row's own copy wins either way (both
# servers keep the last value), but llama.cpp logs "DEPRECATED: argument
# specified multiple times" when handed both. Spell a flag here the way
# <flags> does (-ctk, not --cache-type-k) or both copies are passed.
without_flags() {
    _o=""; _drop=0
    for _w in $1; do
        case "$_w" in
        -*) _drop=0
            case " $2 " in *" $_w "*) _drop=1 ;; esac ;;
        esac
        [ "$_drop" = 1 ] || _o="$_o${_o:+ }$_w"
    done
    printf '%s' "$_o"
}

cat <<HDR
# GENERATED by tools/gen-swap-config.sh from \`${RIG_SPEC:-$DS4_DIR/ds4-serve spec}\`
# on $(date '+%Y-%m-%d %H:%M') — DO NOT HAND-EDIT. Re-run the generator instead.
# Source of truth: ds4_registry() in ds4-serve. This file is .gitignored.

# Big models take 13–17 s to LOAD on this M5 Max (measured 2026-09-11,
# with a swap benchmark), so give readiness plenty of room.
healthCheckTimeout: 300

# ⚠️  DEFAULT IS 10 s, THEN SIGKILL. On macOS a SIGKILL against a process
#    holding a large wired Metal allocation leaks that memory at KERNEL level
#    (recoverable only by reboot — jundot/omlx#2184, exo#1872/#1972, the
#    latter a kernel panic on an M4 Max 128 GB). Every ds4 model below raises
#    this further to 240. NEVER lower either number.
unloadTimeout: 180

startPort: 5800
sendLoadingState: true
logLevel: info
logTimeFormat: stamp   # timestamps: a 3.5 h client hang on 2026-09-11 was undiagnosable without them
HDR

echo
echo "models:"
printf '%s\n' "$SPEC" | while IFS='|' read -r target engine family ctx trust dl label use gguf vision drafter args; do
    [ -n "$target" ] || continue
    row_launch "$target" "$args"
    row_llama_bin=$LLAMA_BIN
    for kv in $row_env; do
        case "$kv" in LLAMA_BIN=*) row_llama_bin=${kv#LLAMA_BIN=} ;; esac
    done
    echo "  \"$target\":"
    echo "    name: \"$label\""
    echo "    description: \"$use (trust: $trust)\""
    # NO concurrencyLimit (the first design said 1). Measured 2026-09-11: a request
    # over the limit gets an IMMEDIATE 429, it is not queued, and a request
    # orphaned by a killed client keeps the slot for its whole generation, so
    # pi's next call burned through its auto-retries and failed. Both backends
    # already serialize internally (llama-server --parallel 1, ds4-server's
    # own scheduler), so the limit bought nothing and cost real failures.
    if in_list "$target" "$RIG_RESIDENT" || in_list "$target" "$LOOP_MODELS"; then
        # The two models the loop lives on never expire on their own.
        echo "    ttl: 0"
    fi
    images=""
    [ "$vision" = "-" ] || images=", image"
    # One client's window: --parallel N without -kvu splits --ctx-size into N
    # per-slot streams (the same rule as slot_ctx() in gen-pi-models.py).
    slot_ctx=$ctx
    case " $row_args " in
    *" -kvu "*|*" --kv-unified "*) ;;
    *)  _n=$(printf '%s\n' "$row_args" | awk '{for (i = 1; i < NF; i++) if ($i == "--parallel" || $i == "-np") n = $(i + 1)} END {print n + 0}')
        [ "$_n" -le 1 ] || slot_ctx=$((ctx / _n)) ;;
    esac
    echo "    capabilities: { in: [text$images], out: [text], tools: true, context: $slot_ctx }"
    echo "    metadata: { engine: $engine, family: $family, trust: \"$trust\" }"
    # The row's environment (field 12). ds4-serve exports the same words.
    if [ -n "$row_env" ]; then
        echo "    env:"
        for kv in $row_env; do echo "      - \"$kv\""; done
    fi

    case "$engine" in
    ds4)
        # DwarfStar has NO /health; /v1/models is the only cheap 200.
        echo "    checkEndpoint: /v1/models"
        echo "    unloadTimeout: 240"
        # Upstream ignores/echoes the model id, but pi's compat block is keyed
        # to the id the server *reports*, so send what its /v1/models says:
        # DeepSeek → deepseek-v4-flash; GLM-5.3 → glm-5.3-flash (measured
        # 2026-09-22 on ds4 0aaea5a; before that upstream reported glm-5.2 for
        # every GLM, and still accepts glm-5.2 as an alias).
        case "$family" in
        deepseek) echo "    useModelName: deepseek-v4-flash" ;;
        glm)      echo "    useModelName: glm-5.3-flash" ;;
        esac
        echo "    cmd: |"
        echo "      $DS4_BIN"
        # ds4-server resolves metal/flash_attn.metal relative to its cwd and
        # aborts with "metal backend unavailable" from anywhere else (hit on
        # 2026-09-11 under llama-swap). ds4-serve cd's into the checkout. Same fix here.
        echo "      --chdir $DS4_DIR"
        echo "      -m $GGUF/$gguf"
        echo "      --ctx $ctx"
        # 64 GB since 2026-09-24: at 32 GB the disk cache was full during a pi film run and evicted
        # the live session's older checkpoints, which the next request then had to re-read from token 0
        # (a film-rig post-mortem, 2026-09-24). The disk had 700+ GB free.
        echo "      --kv-disk-dir $HOME/.ds4/kv-disk --kv-disk-space-mb ${KV_DISK_MB:-65536}"
        [ "$vision" = "-" ] || echo "      --vision $GGUF/$vision"
        if [ "$drafter" != "-" ] && [ "${DS4_DSPARK:-0}" = "1" ]; then
            echo "      --dspark --mtp-model $GGUF/$drafter"
        fi
        # Mirrors launch_flags() in ds4-serve. --mtp is INCOMPATIBLE with
        # --ssd-streaming (measured: "glm mtp step failed at stage 'enorm'");
        # GLM is resident here, never streamed (0.4 tok/s streamed, §4.5).
        case "$family" in
        glm) echo "      --mtp --power 100 --mixed-prefill-quantum 1024" ;;
        # Qwen3.8-Flash-Next on ds4 (2026-09-16): built-in MTP, plain mode.
        # useModelName for qwen4 waits for a measured /v1/models (the source
        # lists qwen3.8-flash-next first); an unknown id is served as-is.
        qwen4) echo "      --mtp" ;;
        esac
        # The row's own args (field 12), after the family flags as in ds4-serve.
        [ -z "$row_args" ] || echo "      $row_args"
        echo "      --port \${PORT}"
        ;;
    llama)
        echo "    checkEndpoint: /health"
        echo "    cmd: |"
        echo "      $row_llama_bin"
        echo "      -m $GGUF/$gguf"
        echo "      --ctx-size $ctx"
        # Same floor ds4-serve uses: Qwen-VL grounding wants >= 1024 image tokens.
        [ "$vision" = "-" ] || echo "      --mmproj $GGUF/$vision --image-min-tokens 1024"
        [ "$drafter" = "-" ] || echo "      --spec-draft-model $GGUF/$drafter"
        echo "      --port \${PORT}"
        # --parallel 1: llama-server defaults to 4 slots and quadruples KV
        # (trap 2). q8_0 KV halves it (7.5 -> 4.0 GiB at 131k); quantized V
        # needs flash attention, -fa auto turns it on. This is the invocation
        # measured to co-reside with flash (verified 2/2 through pi, 2026-09-11).
        # The row's own args (field 12) come last and replace any of these they
        # set: ds4-serve passes none of these, so a row that needs them must
        # carry them, and llama-swap then passes each flag once.
        flags=$(without_flags "--parallel 1 -ctk q8_0 -ctv q8_0 -fa auto --jinja" "$row_args")
        [ -z "$flags" ] || echo "      $flags"
        [ -z "$row_args" ] || echo "      $row_args"
        ;;
    *)
        echo "gen-swap-config: unknown engine '$engine' for $target — add a case" >&2
        exit 1 ;;
    esac
done

# An empty resident set (no lean llama row; the case since qwen27-65k was
# removed on 2026-10-02) still writes `preload: []`, so the file states that
# nothing is preloaded instead of leaving it to be inferred.
if [ -n "$(printf '%s' "$RIG_RESIDENT" | tr -d ' ')" ]; then
    PRELOAD_NOTE="# The resident worker is up before the first request arrives."
else
    PRELOAD_NOTE="# No resident worker: no lean llama row, so nothing loads until asked for."
fi
cat <<EOT

hooks:
  on_startup:
    $PRELOAD_NOTE
    preload: $(yaml_list "$RIG_RESIDENT")

routing:
  router:
EOT

case "$RIG_ROUTER" in
group)
    cat <<EOT
    use: group
    settings:
      groups:
        # One big model at a time; loading one evicts any other big one.
        # NOT the default (RIG_ROUTER=group to get it): this router cannot
        # say "glm53 also evicts the persistent worker", and glm53 + qwen
        # OOMed at warmup when llama-swap swapped flash -> glm53 on
        # 2026-09-11 (kIOGPUCommandBufferCallbackErrorOutOfMemory). It only
        # ever worked when hand-loaded into a settled memory state.
        big:
          swap: true
          exclusive: true
          members: $(yaml_list "$BIG")
        # The lean worker that stays loaded beside a big model.
        resident:
          persistent: true
          swap: false
          exclusive: false
          members: $(yaml_list "$RIG_RESIDENT")
EOT
    ;;
matrix)
    # DEFAULT router. WHY (2026-09-11/12): hand-loaded, glm53 + qwen27-65k
    # answered 4/4 through pi with wired at 106.7 of 107.52 GiB. Under
    # llama-swap the same pair OOMed at Metal warmup (the swapper starts the
    # next process before the kernel reclaims the last one's wired pages), and
    # flash + qwen27-65k, the first design's "zero swap" pair, OOMed twice on a real
    # 5.2k-token pi tool prefill. The group
    # router cannot express "X evicts the persistent worker"; the matrix can.
    # So: every model alone. Requesting any model evicts whatever is loaded
    # (qwen back in 7 s, flash in ~13 s warm). Permit a pair only after
    # proving it through a real pi agent run AND an actual llama-swap swap.
    # The pair was only ever measured with the Q2 `flash` row, whose weights
    # were deleted on 2026-10-01. Without that row RIG_PAIR=1 would name a
    # model that llama-swap does not have.
    if [ "${RIG_PAIR:-0}" = 1 ] && ! in_list flash "$BIG"; then
        echo "gen-swap-config: RIG_PAIR=1 needs the flash row, whose Q2 weights were deleted on 2026-10-01" >&2
        exit 1
    fi
    ALONE=""
    for t in $BIG; do
        if [ "${RIG_PAIR:-0}" = 1 ] && [ "$t" = flash ]; then continue; fi
        ALONE="$ALONE${ALONE:+ | }$t"
    done
    WORKERS=$(printf '%s' "$RIG_RESIDENT" | sed 's/^ *//; s/ *$//; s/ / | /g')
    # With no lean row the worker sets are left out entirely: llama-swap
    # rejects an empty set expression ("worker_alone": empty DSL expression).
    PAIR_SET=""
    WORKER_SET=""
    if [ -n "$WORKERS" ]; then
        if [ "${RIG_PAIR:-0}" = 1 ]; then
            PAIR_SET="          with_worker: \"flash & ($WORKERS)\""
        else
            PAIR_SET="          # with_worker: \"flash & ($WORKERS)\"   # RIG_PAIR=1 — see header"
        fi
        WORKER_SET="          worker_alone: \"$WORKERS\""
    fi
    # No evict_costs since 2026-10-01. Cold loads measured 2026-09-11 were
    # qwen27 7 s, flash 13 s, glm53 17 s, and the costs named those rows by
    # hand; the flash rows and then glm53 were deleted, leaving nothing. When
    # every set holds one model there is only one way to serve a request, so a
    # cost changes nothing. Give a cost again only with a pair set, and only for
    # a row that is in the registry. This note stays out of the YAML so the
    # generated file never names a deleted row.
    cat <<EOT
    use: matrix
    settings:
      matrix:
        # Every model runs ALONE unless a set below says otherwise. Each
        # plan->code->review loop therefore costs two swaps (big model -> qwen ->
        # big model). That is the price of never OOMing mid-prefill.
        sets:
$PAIR_SET
          big_alone: "$ALONE"
$WORKER_SET
EOT
    ;;
*)
    echo "gen-swap-config: RIG_ROUTER must be group or matrix" >&2; exit 1 ;;
esac
