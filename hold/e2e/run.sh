#!/bin/zsh
# hold/e2e/run.sh — the hold gate end to end with real pi (2026-10-04). Touches nothing live: a test gate on
# 127.0.0.1:18090 in front of a stand-in llama-swap on :18091 (e2e/stub.py, which logs every request it gets), and
# a throwaway pi agent dir whose only extension is ../../extensions/hold.ts. Ground truth is the stub's log: during a
# hold no model call may arrive there, and every held call must arrive after the release.
#   A  open gate: pi answers
#   B  pi prompted during a manual hold: waits, nothing reaches the model, goes out after `hold off`
#   D  hold taken while pi's own bash tool runs: pi's next model call waits for `hold off`
#   E  `hold run` asked while a reply streams: the reply finishes whole, then unload, then the job (HOLD_ID set)
#   F  during a hold: curl (Codex's view) and /upstream get 503 llm_held; read-only calls pass; pi without the
#      extension retries and reports the refusal; nothing reaches the model
# Usage: hold/e2e/run.sh   (exit 0 = all pass; about 45 s)
set -u
here=${0:a:h}; H=${here:h}/hold
for port in 18090 18091; do lsof -ti tcp:$port -sTCP:LISTEN >/dev/null 2>&1 && { print "port $port is busy; stop what listens there first"; exit 2 }; done
S=$(mktemp -d -t hold-e2e); trap 'kill $stub $gate 2>/dev/null; rm -rf $S' EXIT
(cd ${here:h} && go build -o hold .) || exit 1
mkdir -p $S/pi-agent/extensions; ln -s ${here:h:h}/extensions/hold.ts $S/pi-agent/extensions/hold.ts
python3 - $S <<'PY'
import json, sys, os
S = sys.argv[1]
m = json.load(open(os.path.expanduser('~/.pi/agent/models.json')))
p = m['providers']['local']; q = [x for x in p['models'] if x['id'] == 'qwen38'][0]
json.dump({"providers": {"local": {**{k: v for k, v in p.items() if k != 'models'}, "baseUrl": "http://127.0.0.1:18090/v1", "models": [q]}}}, open(f"{S}/pi-agent/models.json", "w"))
json.dump({"defaultProvider": "local", "defaultModel": "qwen38", "packages": []}, open(f"{S}/pi-agent/settings.json", "w"))
PY
: > $S/stub.log; python3 $here/stub.py 18091 $S/stub.log & stub=$!
$H gate --listen 127.0.0.1:18090 --upstream http://127.0.0.1:18091 --state $S/state.json --no-implicit --drain-timeout 60s > $S/gate.log 2>&1 & gate=$!
export HOLD_GATE=http://127.0.0.1:18090 PI_CODING_AGENT_DIR=$S/pi-agent
cd $S; sleep 1
kill -0 $stub $gate 2>/dev/null || { print "the stub or the test gate did not start"; cat $S/gate.log; exit 2 }
to() { perl -e 'alarm shift; exec @ARGV' "$@"; }
now() { python3 -c 'import time;print(f"{time.time():.3f}")'; }
calls_since() { awk -v m=$1 '$1>m && /CHAT role/' $S/stub.log | wc -l | tr -d ' '; }
first_call_after() { awk -v m=$1 '$1>m && /CHAT role/ {print $1; exit}' $S/stub.log; }
fails=0; ok() { print "PASS $1"; }; bad() { print "FAIL $1"; fails=$((fails+1)); }

out=$(to 60 pi -p --no-session "say PONG" < /dev/null 2>&1); [[ $out == *PONG* ]] && ok "A open gate: pi answers" || bad "A: $out"

$H on "e2e B" >/dev/null; m=$(now)
(to 120 pi -p --no-session "say PONG" < /dev/null > piB.out 2>&1) & p=$!
sleep 6; n=$(calls_since $m); kill -0 $p 2>/dev/null && w=1 || w=0
r=$(now); $H off >/dev/null; wait $p; f=$(first_call_after $m)
[[ $n == 0 && $w == 1 && $(<piB.out) == *PONG* && -n $f ]] && (( f > r )) && ok "B held prompt waited 6 s with 0 model calls, went out $(printf %.0f $(( (f-r)*1000 ))) ms after hold off" || bad "B: calls=$n waiting=$w first=$f release=$r out=$(<piB.out)"

m=$(now); (to 120 pi -p --no-session "USE_TOOL please" < /dev/null > piD.out 2>&1) & p=$!
sleep 1.5; $H on "e2e D" >/dev/null; sleep 7; n=$(calls_since $m); r=$(now); $H off >/dev/null; wait $p
[[ $n == 1 && $(<piD.out) == *DONE-AFTER-TOOL* ]] && ok "D the call after pi's tool waited for hold off (1 call before, the rest after)" || bad "D: calls during hold=$n out=$(<piD.out)"

m=$(now); (to 120 pi -p --no-session "SLOW reply please" < /dev/null > piE.out 2>&1) & p=$!
sleep 2; job=$($H run --quiet --kind m3d --reason "e2e E" -- sh -c 'python3 -c "import time;print(f\"{time.time():.3f}\")"; echo "id=$HOLD_ID"'); wait $p
done_at=$(awk -v m=$m '$1>m && /CHAT done/ {print $1; exit}' $S/stub.log); unl=$(awk -v m=$m '$1>m && /UNLOADED/ {print $1; exit}' $S/stub.log); st=${job%%$'\n'*}
[[ $(<piE.out) == *PONG* && $job == *id=h* && -n $done_at && -n $unl && -n $st ]] && (( done_at <= unl && unl <= st )) && ok "E reply finished ($done_at), then unload ($unl), then the job ($st)" || bad "E: done=$done_at unload=$unl job=$job out=$(<piE.out)"

$H on "e2e F" >/dev/null; m=$(now)
c1=$(curl -s -o $S/f1 -w '%{http_code}' -X POST 127.0.0.1:18090/v1/chat/completions -d '{"model":"qwen38"}')
c2=$(curl -s -o /dev/null -w '%{http_code}' 127.0.0.1:18090/upstream/qwen38/logs)
c3=$(curl -s -o /dev/null -w '%{http_code}' 127.0.0.1:18090/running); c4=$(curl -s -o /dev/null -w '%{http_code}' 127.0.0.1:18090/v1/models)
pf=$(to 120 pi -p --no-session --no-extensions "say PONG" < /dev/null 2>&1); n=$(calls_since $m); $H off >/dev/null
[[ $c1 == 503 && $(<$S/f1) == *llm_held* && $c2 == 503 && $c3 == 200 && $c4 == 200 && $pf == *"LLM is held"* && $n == 0 ]] \
  && ok "F refused: curl 503, /upstream 503; passed: /running, /v1/models; pi without the extension reported the hold; 0 model calls" \
  || bad "F: curl=$c1 upstream=$c2 running=$c3 models=$c4 calls=$n pi=${pf[1,200]}"

(( fails )) && { print "\n$fails FAILED; gate log:"; cat $S/gate.log; exit 1 }
print "all pass"
