#!/bin/sh
# install-launchd.sh — render launchd/*.plist for this machine into ~/Library/LaunchAgents.
#
#   tools/install-launchd.sh                    # hold-gate and llama-swap (the rig itself)
#   tools/install-launchd.sh searxng ldr        # the optional web-research services
#   tools/install-launchd.sh --print hold-gate  # print the rendered plist, install nothing
#
# The plists in this repo carry placeholders instead of one machine's paths:
#   __HOME__         $HOME
#   __RIG__          this checkout
#   __SEARXNG_SRC__  $SEARXNG_SRC (default ~/repos/searxng), a SearXNG checkout with its .venv
#   __LDR_SRC__      $LDR_SRC (default ~/repos/local-deep-research), a Local Deep Research checkout
# Labels are com.example.local-rig.<name>; rename them here and in hold/switch-over.sh if you want your own.
# This only writes files. Load a job with
#   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.local-rig.<name>.plist
# (for the gate and llama-swap, hold/switch-over.sh in does the ordering for you).
set -eu

RIG=$(cd "$(dirname "$0")/.." && pwd)
LA=$HOME/Library/LaunchAgents
SEARXNG_SRC=${SEARXNG_SRC:-$HOME/repos/searxng}
LDR_SRC=${LDR_SRC:-$HOME/repos/local-deep-research}
PRINT=0
[ "${1:-}" = "--print" ] && { PRINT=1; shift; }
[ $# -gt 0 ] || set -- hold-gate llama-swap

render() {
    sed -e "s|__RIG__|$RIG|g" -e "s|__SEARXNG_SRC__|$SEARXNG_SRC|g" \
        -e "s|__LDR_SRC__|$LDR_SRC|g" -e "s|__HOME__|$HOME|g" "$1"
}

for name in "$@"; do
    src=$RIG/launchd/com.example.local-rig.$name.plist
    [ -f "$src" ] || { echo "install-launchd: no $src" >&2; exit 1; }
    if [ "$PRINT" = 1 ]; then render "$src"; continue; fi
    mkdir -p "$LA" "$HOME/.ds4"   # the logs live next to the ds4 engine's own
    render "$src" > "$LA/com.example.local-rig.$name.plist"
    plutil -lint "$LA/com.example.local-rig.$name.plist" >/dev/null
    echo "wrote $LA/com.example.local-rig.$name.plist"
done
