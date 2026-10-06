#!/bin/sh
# searxng-run.sh — start the local SearXNG (Granian WSGI, like the official
# container) bound to 127.0.0.1 only. Run by launchd
# (launchd/com.example.local-rig.searxng.plist) or by hand for debugging.
#
# Source checkout:  $SEARXNG_SRC (default ~/repos/searxng), with its .venv
# Settings:         config/searxng/settings.yml in this repo
# Secret:           ~/.config/searxng/secret_key, created 0600 on first run
set -eu

RIG_DIR=$(cd "$(dirname "$0")/.." && pwd)
SEARXNG_SRC=${SEARXNG_SRC:-$HOME/repos/searxng}
SECRET_FILE=$HOME/.config/searxng/secret_key

if [ ! -x "$SEARXNG_SRC/.venv/bin/granian" ]; then
    echo "searxng-run: no venv at $SEARXNG_SRC/.venv (see docs/web-research.md)" >&2
    exit 1
fi
if [ ! -s "$SECRET_FILE" ]; then
    mkdir -p "$(dirname "$SECRET_FILE")"
    chmod 700 "$(dirname "$SECRET_FILE")"
    (umask 077 && openssl rand -hex 32 > "$SECRET_FILE")
fi

# Granian binds with SO_REUSEPORT, so a second copy started by hand would
# silently share the port and answer half the requests with whatever settings
# it loaded. Refuse instead (launchd retries every ThrottleInterval seconds).
PORT=${SEARXNG_PORT:-8080}
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "searxng-run: something already listens on 127.0.0.1:$PORT:" >&2
    lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >&2
    echo "searxng-run: stop it first (launchctl bootout gui/\$(id -u)/com.example.local-rig.searxng for the service)" >&2
    exit 1
fi

export SEARXNG_SETTINGS_PATH="$RIG_DIR/config/searxng/settings.yml"
SEARXNG_SECRET=$(cat "$SECRET_FILE")
export SEARXNG_SECRET

cd "$SEARXNG_SRC"
exec "$SEARXNG_SRC/.venv/bin/granian" \
    --interface wsgi \
    --host 127.0.0.1 \
    --port "$PORT" \
    --workers 1 \
    --blocking-threads 4 \
    --process-name searxng \
    searx.webapp:app
