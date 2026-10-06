#!/bin/sh
# ldr-run.sh — start the Local Deep Research web server that pi's
# deep-research skill drives. Run by launchd
# (launchd/com.example.local-rig.ldr.plist) or by hand for debugging.
#
# Port: 5055, not LDR's default 5000. macOS AirPlay Receiver (ControlCenter)
# holds port 5000 on both IPv4 and IPv6 (seen 2026-09-23), so a default LDR
# either fails to bind or is shadowed, and the driver talks to AirTunes.
#
# Server settings come from the LDR_* lines of ~/.config/ldr-pi.env. The
# LDR_PI_* lines there are the driver's login and are deliberately NOT
# exported into the server's environment.
set -eu

LDR_SRC=${LDR_SRC:-$HOME/repos/local-deep-research}
ENV_FILE=${LDR_ENV_FILE:-$HOME/.config/ldr-pi.env}
PORT=${LDR_WEB_PORT:-5055}

if [ ! -x "$LDR_SRC/.venv/bin/ldr-web" ]; then
    echo "ldr-run: no ldr-web in $LDR_SRC/.venv" >&2
    exit 1
fi
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "ldr-run: something already listens on port $PORT:" >&2
    lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >&2
    exit 1
fi

if [ -f "$ENV_FILE" ]; then
    # shellcheck disable=SC2046
    export $(grep -E '^LDR_[A-Z0-9_]+=' "$ENV_FILE" | grep -v '^LDR_PI_' | xargs)
fi
export LDR_WEB_HOST=127.0.0.1
export LDR_WEB_PORT="$PORT"

cd "$LDR_SRC"
exec "$LDR_SRC/.venv/bin/ldr-web"
