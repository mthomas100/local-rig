package main

// Process table, liveness and GPU-job detection for the hold gate (2026-10-04).
//
// One `ps` scan answers three questions: is a hold's owner still alive (pid plus start time, because pids are
// reused), which processes descend from a hold (a render's story.sh, vidgen and ltx-2-mlx belong to the hold its
// runner took), and which GPU jobs run outside any hold (a render started by hand or by an old script), which close
// the gate just like a hold does.

import (
	"bufio"
	"bytes"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Proc is one row of `ps -axww -o pid=,ppid=,lstart=,command=`.
type Proc struct {
	Pid, Ppid int
	Start     string // lstart, e.g. "Sun Oct  4 18:28:01 2026"; with the pid it names one process
	Cmd       string
}

type ProcTable map[int]Proc

func scanProcs() (ProcTable, error) {
	// -ww: without it ps cuts the command at the terminal width (or 132 columns), and the GPU-job match reads argv.
	out, err := exec.Command("ps", "-axww", "-o", "pid=,ppid=,lstart=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parsePs(out), nil
}

func parsePs(out []byte) ProcTable {
	t := ProcTable{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 7 { // pid, ppid and the five words of lstart
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		t[pid] = Proc{Pid: pid, Ppid: ppid, Start: strings.Join(f[2:7], " "), Cmd: strings.Join(f[7:], " ")}
	}
	return t
}

// chain returns pid and its ancestors, nearest first.
func (t ProcTable) chain(pid int) []int {
	var out []int
	seen := map[int]bool{}
	for p := pid; p > 1 && !seen[p]; {
		seen[p] = true
		out = append(out, p)
		pr, ok := t[p]
		if !ok {
			break
		}
		p = pr.Ppid
	}
	return out
}

// alive: the pid exists and, when its start time was recorded, is still the same process.
func (t ProcTable) alive(r PidRef) bool {
	p, ok := t[r.Pid]
	return ok && (r.Start == "" || p.Start == r.Start)
}

// Programs that never touch the GPU, however their arguments read (m3d's NOT_GPU list, 2026-10-02: a `gh api
// repos/ddalcu/mlx-serve/...` was once counted as a GPU job).
var notGPU = map[string]bool{
	"gh": true, "git": true, "grep": true, "rg": true, "ugrep": true, "tail": true, "head": true, "less": true,
	"more": true, "cat": true, "vim": true, "nvim": true, "curl": true, "ssh": true, "pgrep": true, "ps": true,
	"lsof": true, "sed": true, "awk": true, "open": true, "code": true, "kb": true, "find": true, "ls": true,
	"ffmpeg": true, "ffprobe": true, "afplay": true, "hold": true, "tmux": true, "watch": true,
}

var stillPath = regexp.MustCompile(`/local-video/bin/still$`)

// gpuJob names the GPU program a command line runs, or "". It reads the program being run (the first few non-flag
// argv items, so `zsh stories/story.sh`, `python -m ltx_pipelines` and `/usr/bin/time -l hy3d generate` are found),
// never a word anywhere in the arguments, the way m3d's is_gpu_job does. Long-running servers (ComfyUI, mlx-serve,
// the dictation daemon) are left out on purpose: alive is not busy, and an idle server must not keep the LLM off.
func gpuJob(cmd string) string {
	argv := strings.Fields(cmd)
	if len(argv) == 0 {
		return ""
	}
	first := filepath.Base(argv[0])
	if notGPU[first] {
		return ""
	}
	if first == "sh" || first == "bash" || first == "zsh" {
		for _, a := range argv[1:min(3, len(argv))] {
			if a == "-c" { // a shell only mentions a GPU job in its text (a watchdog loop did, 2026-09-26)
				return ""
			}
		}
	}
	var toks []string
	for _, a := range argv[:min(6, len(argv))] {
		if !strings.HasPrefix(a, "-") {
			toks = append(toks, a)
		}
	}
	if len(toks) > 4 {
		toks = toks[:4]
	}
	for _, tok := range toks {
		b := filepath.Base(tok)
		switch {
		case b == "ltx-2-mlx" || strings.Contains(tok, "/ltx-2-mlx/"):
			return "ltx-2-mlx"
		case strings.HasPrefix(b, "ltx_pipelines"):
			return "ltx_pipelines"
		case b == "story.sh" || b == "redo.sh" || b == "run-queue.sh" || b == "vidgen":
			return b
		case stillPath.MatchString(tok):
			return "still"
		case strings.HasPrefix(b, "mflux-generate"):
			return "mflux-generate"
		case b == "hy3d" && hasAny(argv, "generate", "shape", "paint"):
			return "hy3d"
		case b == "comfy3d.py" || b == "skintokens-cli":
			return b
		}
	}
	return ""
}

func hasAny(argv []string, words ...string) bool {
	for _, a := range argv {
		for _, w := range words {
			if a == w {
				return true
			}
		}
	}
	return false
}

// label names what a process runs in a few words, for status lines: the program and its first arguments, without
// the long interpreter and venv paths (the Homebrew python path cut at 120 characters read "Py…", 2026-10-04).
func label(cmd string) string {
	var out []string
	for _, a := range strings.Fields(cmd) {
		if len(out) == 4 {
			break
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, filepath.Base(a))
	}
	return trunc(strings.Join(out, " "), 100)
}

// llmEngine reports a language-model server running outside llama-swap's children, for status only: an engine
// started with `model <target>` (ds4-serve) holds its memory whatever the gate decides.
func llmEngine(cmd string) bool {
	argv := strings.Fields(cmd)
	if len(argv) == 0 {
		return false
	}
	b := filepath.Base(argv[0])
	return b == "ds4-server" || b == "llama-server"
}
