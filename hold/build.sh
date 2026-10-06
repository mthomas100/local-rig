#!/bin/zsh
# hold/build.sh — vet, test and build the hold binary in place (hold/hold, gitignored). ~/.local/bin/hold and the
# launchd job com.example.local-rig.hold-gate run this file, so a rebuild takes effect for the CLI at once and for the
# gate at its next restart (launchctl kickstart -k gui/$UID/com.example.local-rig.hold-gate, only while nothing is held
# and no call is in flight: a restart cuts streams that pass through it).
set -eu
cd ${0:a:h}
go vet ./...
go test -count=1 ./...
go build -o hold.new . && mv hold.new hold
print "built $(pwd)/hold"
