#!/bin/zsh
# hold/switch-over.sh check | in | out — put the hold gate in front of llama-swap, or take it out (docs/hold.md).
#   check  is now a safe moment? no GPU job running and nothing loaded or starting in llama-swap (so no model call
#          can be in flight). Exit 0 when clear.
#   in     llama-swap moves to :8091, the gate takes :8090 (launchd com.example.local-rig.hold-gate), `hold` goes on PATH
#          (~/.local/bin/hold), pi's hold.ts is linked into ~/.pi/agent/extensions; then `hold status`.
#   out    the way back: the gate out, llama-swap on :8090 again, hold.ts unlinked (the CLI link stays; with no gate
#          it says so, and every tool that takes a hold falls back to its old behaviour).
# Both restart llama-swap, so both refuse unless `check` passes (`--force` skips that, only for an idle Mac whose
# check is wrong). Every client keeps using 127.0.0.1:8090 throughout; nothing else needs reconfiguring. pi
# sessions already running pick up hold.ts on /reload or restart; until then the gate refuses their calls while a
# hold is on (they retry, then show the refusal) instead of making them wait.
set -u
R=${0:a:h:h}; LA=~/Library/LaunchAgents; U=gui/$(id -u)
SWAP=com.example.local-rig.llama-swap; GATE=com.example.local-rig.hold-gate

check() {
  local bad=0 r
  if pgrep -f 'run-queue[.]sh|story[.]sh|redo[.]sh|ltx-2-mlx generat[e]|mflux-generat[e]|bin/[s]till|hy3d (generate|shape|paint)' >/dev/null; then
    print "not now: a GPU job is running:"; pgrep -lf 'run-queue[.]sh|story[.]sh|redo[.]sh|ltx-2-mlx generat[e]|mflux-generat[e]|bin/[s]till' | head -5 | cut -c1-140; bad=1
  fi
  for port in 8090 8091; do
    r=$(curl -s -m 3 127.0.0.1:$port/running 2>/dev/null) || continue
    [[ $r == *'"model"'* ]] && { print "not now: llama-swap on :$port has a model loaded or starting: $r"; bad=1; }
  done
  (( bad )) || print "clear: no GPU job, nothing loaded"
  return $bad
}

wait_for() { # url, what, seconds
  local i
  for i in {1..$3}; do curl -s -m 2 -o /dev/null $1 && return 0; sleep 1; done
  print "timed out waiting for $2 ($1)"; return 1
}

swap_on() { # port: rewrite the installed llama-swap plist to listen there and (re)start it
  sed -i '' -E "s|<string>127\.0\.0\.1:809[01]</string>|<string>127.0.0.1:$1</string>|" $LA/$SWAP.plist
  launchctl bootout $U/$SWAP 2>/dev/null
  for i in {1..60}; do pgrep -x llama-swap >/dev/null || break; sleep 1; done
  launchctl bootstrap $U $LA/$SWAP.plist
  wait_for http://127.0.0.1:$1/running "llama-swap on :$1" 30
}

case ${1:-check} in
  check) check ;;
  in)
    [[ ${2:-} == --force ]] || check || exit 1
    (cd $R/hold && ./build.sh) || exit 1
    ln -sfn $R/hold/hold ~/.local/bin/hold
    $R/tools/install-launchd.sh hold-gate >/dev/null || exit 1
    [[ -f $LA/$SWAP.plist ]] || $R/tools/install-launchd.sh llama-swap >/dev/null || exit 1
    swap_on 8091 || exit 1
    launchctl bootout $U/$GATE 2>/dev/null
    launchctl bootstrap $U $LA/$GATE.plist
    wait_for http://127.0.0.1:8090/hold/status "the gate on :8090" 30 || exit 1
    ln -sfn $R/extensions/hold.ts ~/.pi/agent/extensions/hold.ts
    print "switched in:"; ~/.local/bin/hold status ;;
  out)
    [[ ${2:-} == --force ]] || check || exit 1
    launchctl bootout $U/$GATE 2>/dev/null
    for i in {1..20}; do curl -s -m 1 -o /dev/null 127.0.0.1:8090/ || break; sleep 1; done
    swap_on 8090 || exit 1
    rm -f ~/.pi/agent/extensions/hold.ts
    print "switched out: llama-swap answers on :8090 again; /reload or restart pi sessions to drop hold.ts" ;;
  *) print "usage: hold/switch-over.sh check | in | out [--force]"; exit 2 ;;
esac
