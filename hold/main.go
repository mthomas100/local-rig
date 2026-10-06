// hold — one lock for this Mac's GPU, shared by the local LLM, renders, m3d and the human (2026-10-04).
//
//	hold [status] [--json]          who has the GPU: open (model calls pass), draining, or held, and by whom
//	hold on [--for 2h] <why>        keep the LLM off on purpose; waits for calls in flight, unloads, holds
//	hold off [id]                   end your manual hold
//	hold run [--kind K] [--reason R] -- <cmd...>
//	                                run a GPU job under a hold: queue, drain, unload, exec the command; the hold
//	                                ends when the command exits (also when it crashes). Inside a hold (HOLD_ID set,
//	                                or an ancestor holds) the command runs at once in that hold.
//	hold acquire [--pid N]... [--kind K] [--reason R] [--json] [--no-wait]
//	                                for programs: take a hold for these pids (default: the caller), print its id once
//	                                granted; it ends when they all exit or on `hold release <id>`
//	hold release <id>
//	hold wait [--timeout 10m]       block until model calls may pass (exit 1 on timeout)
//	hold gate [--listen 127.0.0.1:8090] [--upstream http://127.0.0.1:8091] ...   the gate itself (launchd)
//
// The gate's address comes from HOLD_GATE (default http://127.0.0.1:8090). Exit codes: 0 ok, 1 error or timeout,
// 2 usage, 3 the gate did not answer (`hold run` then runs the command anyway, with a warning: a missing gate also
// means no model can be loaded through :8090, so the job is not racing an LLM through it).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var gateURL = envOr("HOLD_GATE", "http://127.0.0.1:8090")

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	args := os.Args[1:]
	cmd := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "status":
		os.Exit(cmdStatus(args))
	case "on":
		os.Exit(cmdOn(args))
	case "off":
		os.Exit(cmdOff(args))
	case "run":
		os.Exit(cmdRun(args))
	case "acquire":
		os.Exit(cmdAcquire(args))
	case "release":
		os.Exit(cmdRelease(args))
	case "wait":
		os.Exit(cmdWait(args))
	case "gate":
		os.Exit(cmdGate(args))
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	src := `hold [status] [--json] | on [--for D] <why> | off [id] | run [--kind K] [--reason R] -- <cmd...>
     | acquire [--pid N]... [--kind K] [--reason R] [--json] [--no-wait] | release <id> | wait [--timeout D] | gate
One lock for this Mac's GPU: renders, m3d and 'hold on' take it; model calls wait or are refused while it is held.`
	fmt.Fprintln(w, src)
}

// ---------- talking to the gate ----------

var errGateDown = errors.New("the hold gate did not answer")

func call(method, path string, body any, out any, timeout time.Duration) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, gateURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w at %s (%v)", errGateDown, gateURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound && !strings.Contains(string(b), "hold") {
		return fmt.Errorf("%w at %s: something else answers there (is llama-swap still on that port?)", errGateDown, gateURL)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		json.Unmarshal(b, &e)
		if e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("%s", e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func status() (statusResp, error) {
	var s statusResp
	err := call(http.MethodGet, "/hold/status", nil, &s, 10*time.Second)
	return s, err
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "hold:", err)
	if errors.Is(err, errGateDown) {
		return 3
	}
	return 1
}

// ---------- status ----------

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "raw JSON")
	fs.Parse(args)
	s, err := status()
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(s, "", " ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Print(renderStatus(s))
	return 0
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func holdLine(h Hold) string {
	s := h.Kind
	if h.Reason != "" {
		s += " " + strconv.Quote(h.Reason)
	}
	switch h.State {
	case "granted":
		s += " for " + ago(h.GrantAt)
	default:
		s += " (" + h.State + ", asked " + ago(h.Created) + " ago)"
	}
	var ps []string
	for _, p := range h.Pids {
		ps = append(ps, fmt.Sprintf("pid %d %s", p.Pid, shortCmd(p.Cmd)))
	}
	if len(ps) > 0 {
		s += " [" + strings.Join(ps, "; ") + "]"
	} else {
		s += " [manual: `hold off` ends it"
		if !h.Expires.IsZero() {
			s += ", or at " + h.Expires.Format("15:04")
		}
		s += "]"
	}
	if h.Note != "" {
		s += " (" + h.Note + ")"
	}
	return s + "  id " + h.ID
}

func shortCmd(c string) string {
	f := strings.Fields(c)
	var out []string
	for _, a := range f {
		if len(out) == 3 {
			break
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, filepath.Base(a))
	}
	return strings.Join(out, " ")
}

func renderStatus(s statusResp) string {
	var b strings.Builder
	loaded := "nothing loaded"
	if len(s.Running) > 0 {
		loaded = strings.Join(s.Running, ", ") + " loaded"
	}
	if !s.RunningOK {
		loaded = "llama-swap did not answer at " + s.Upstream
	}
	switch s.Phase {
	case "open":
		fmt.Fprintf(&b, "open: model calls pass; %s", loaded)
	case "draining":
		fmt.Fprintf(&b, "draining: model calls are refused; %s", s.Why)
	default:
		fmt.Fprintf(&b, "held: model calls wait; %s", s.Why)
	}
	if s.Unloading {
		b.WriteString(" (unloading now)")
	}
	b.WriteString("\n")
	if len(s.InFlight) > 0 {
		var fs []string
		for _, f := range s.InFlight {
			fs = append(fs, fmt.Sprintf("%s %s from %s, %s", f.Method, f.Path, agentName(f.Agent), ago(f.Started)))
		}
		fmt.Fprintf(&b, "  in flight: %s\n", strings.Join(fs, "; "))
	}
	for i, h := range s.Holds {
		label := "  holds: "
		if i > 0 {
			label = "  next:  "
		}
		b.WriteString(label + holdLine(h) + "\n")
	}
	kids := map[int]int{} // processes under each root job (a render is run-queue.sh > story.sh > vidgen > ltx-2-mlx)
	byPid := map[int]Detected{}
	for _, d := range s.Detected {
		byPid[d.Pid] = d
	}
	for _, d := range s.Detected {
		for p := d; !p.Root; {
			up, ok := byPid[p.Ppid]
			if !ok {
				break
			}
			p = up
			if p.Root {
				kids[p.Pid]++
			}
		}
	}
	for _, d := range s.Detected {
		if !d.Root {
			continue
		}
		more := ""
		if n := kids[d.Pid]; n > 0 {
			more = fmt.Sprintf(" (+%d process%s under it)", n, plural(n, "", "es"))
		}
		fmt.Fprintf(&b, "  GPU job outside any hold: pid %d %s%s since %s\n", d.Pid, d.Name, more, d.Since.Format("15:04"))
	}
	if len(s.Waiting) > 0 {
		var ws []string
		for k, n := range s.Waiting {
			if n > 1 {
				k = fmt.Sprintf("%s x%d", k, n)
			}
			ws = append(ws, k)
		}
		fmt.Fprintf(&b, "  waiting for the LLM: %s\n", strings.Join(ws, ", "))
	}
	if s.Phase != "open" && len(s.Running) > 0 && !s.Unloading {
		fmt.Fprintf(&b, "  (%s)\n", loaded)
	}
	for _, e := range s.Engines {
		fmt.Fprintf(&b, "  note: language-model server outside llama-swap: pid %d %s (the gate does not control it)\n", e.Pid, shortCmd(e.Cmd))
	}
	return b.String()
}

func agentName(ua string) string {
	if ua == "" {
		return "?"
	}
	return strings.Fields(ua)[0]
}

// ---------- taking and ending holds ----------

func waitGranted(id string, quiet bool) (holdView, error) {
	last := ""
	t0 := time.Now()
	for {
		var v holdView
		if err := call(http.MethodGet, "/hold/wait?timeout=20&id="+url.QueryEscape(id), nil, &v, 30*time.Second); err != nil {
			return v, err
		}
		switch v.State {
		case "granted":
			if !quiet && time.Since(t0) > 2*time.Second {
				fmt.Fprintf(os.Stderr, "hold: granted after %s\n", time.Since(t0).Round(time.Second))
			}
			return v, nil
		case "gone":
			return v, fmt.Errorf("hold %s ended before it was granted", id)
		}
		msg := progress(v)
		if !quiet && msg != last {
			fmt.Fprintln(os.Stderr, "hold: "+msg)
			last = msg
		}
	}
}

func progress(v holdView) string {
	switch {
	case v.Position > 0:
		return fmt.Sprintf("queued (%d ahead); the GPU is %s", v.Position, v.Why)
	case v.Unloading:
		return "unloading " + strings.Join(v.Running, ", ")
	case v.InFlight > 0:
		return fmt.Sprintf("waiting for %d model call%s in flight to finish (model calls are refused meanwhile)", v.InFlight, plural(v.InFlight, "", "s"))
	default:
		return "waiting for the GPU (" + v.Why + ")"
	}
}

func acquireAndWait(req acquireReq, quiet bool) (holdView, error) {
	var v holdView
	if err := call(http.MethodPost, "/hold/acquire", req, &v, 30*time.Second); err != nil {
		return v, err
	}
	if v.State == "granted" {
		return v, nil
	}
	// Ctrl-C while waiting: give the place in the queue back instead of leaving it to the liveness scan
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sig:
			call(http.MethodPost, "/hold/release", map[string]string{"id": v.ID, "why": "cancelled while waiting"}, nil, 5*time.Second)
			os.Exit(130)
		case <-done:
		}
	}()
	return waitGranted(v.ID, quiet)
}

func cmdOn(args []string) int {
	fs := flag.NewFlagSet("on", flag.ExitOnError)
	dur := fs.Duration("for", 0, "end the hold by itself after this long (e.g. 2h)")
	kind := fs.String("kind", "manual", "what kind of hold")
	fs.Parse(args)
	why := strings.Join(fs.Args(), " ")
	if why == "" {
		fmt.Fprintln(os.Stderr, "hold on: say why, e.g. hold on \"benchmarking MTPLX\"")
		return 2
	}
	v, err := acquireAndWait(acquireReq{Kind: *kind, Reason: why, Owner: owner(), Pids: []int{os.Getpid()}}, false)
	if err != nil {
		return fail(err)
	}
	// detached from this process: the hold now lasts until `hold off` (or --for)
	if err := call(http.MethodPost, "/hold/detach", map[string]any{"id": v.ID, "ttl_seconds": int(dur.Seconds())}, nil, 10*time.Second); err != nil {
		return fail(err)
	}
	end := "until `hold off`"
	if *dur > 0 {
		end = "until " + time.Now().Add(*dur).Format("15:04") + " or `hold off`"
	}
	fmt.Printf("held: %q (id %s). The LLM stays off %s; model calls wait or are refused.\n", why, v.ID, end)
	return 0
}

func cmdOff(args []string) int {
	s, err := status()
	if err != nil {
		return fail(err)
	}
	var mine []Hold
	for _, h := range s.Holds {
		if len(args) > 0 && h.ID == args[0] || len(args) == 0 && len(h.Pids) == 0 {
			mine = append(mine, h)
		}
	}
	switch {
	case len(mine) == 0 && len(args) > 0:
		fmt.Fprintf(os.Stderr, "hold off: no hold %s (`hold status` lists them)\n", args[0])
		return 1
	case len(mine) == 0:
		fmt.Fprintln(os.Stderr, "hold off: no manual hold. Holds held by running jobs end with the job:")
		fmt.Fprint(os.Stderr, renderStatus(s))
		return 1
	case len(mine) > 1:
		fmt.Fprintln(os.Stderr, "hold off: several manual holds; name one:")
		for _, h := range mine {
			fmt.Fprintln(os.Stderr, "  "+holdLine(h))
		}
		return 2
	}
	if err := call(http.MethodPost, "/hold/release", map[string]string{"id": mine[0].ID, "why": "hold off"}, nil, 10*time.Second); err != nil {
		return fail(err)
	}
	fmt.Printf("released %s (%s)\n", mine[0].ID, mine[0].Reason)
	return 0
}

func owner() string {
	if n := os.Getenv("PI_SESSION_ID"); n != "" {
		return "pi " + n
	}
	wd, _ := os.Getwd()
	return filepath.Base(wd)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	kind := fs.String("kind", "job", "render | m3d | still | ...")
	reason := fs.String("reason", "", "what the job is (default: the command)")
	quiet := fs.Bool("quiet", false, "no progress lines")
	fs.Parse(args)
	argv := fs.Args()
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "hold run: no command (hold run [--kind K] [--reason R] -- <cmd...>)")
		return 2
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "hold run:", err)
		return 127
	}
	if *reason == "" {
		*reason = shortCmd(strings.Join(argv, " "))
	}
	v, err := acquireAndWait(acquireReq{Kind: *kind, Reason: *reason, Owner: owner(), Pids: []int{os.Getpid()}, Parent: os.Getenv("HOLD_ID")}, *quiet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hold run: %v; running %s WITHOUT a hold\n", err, filepath.Base(path))
		// "none", not empty: scripts that re-exec themselves under `hold run` when HOLD_ID is unset would loop
		os.Setenv("HOLD_ID", "none")
	} else {
		os.Setenv("HOLD_ID", v.ID)
	}
	// exec keeps this pid, so the hold names the job itself and ends when it exits
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "hold run:", err)
		return 126
	}
	return 0
}

type pidList []int

func (p *pidList) String() string { return fmt.Sprint(*p) }
func (p *pidList) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*p = append(*p, n)
	return nil
}

func cmdAcquire(args []string) int {
	fs := flag.NewFlagSet("acquire", flag.ExitOnError)
	var pids pidList
	fs.Var(&pids, "pid", "process the hold belongs to (repeatable; default: the caller)")
	kind := fs.String("kind", "job", "render | m3d | ...")
	reason := fs.String("reason", "", "what the job is")
	asJSON := fs.Bool("json", false, "print the hold as JSON")
	noWait := fs.Bool("no-wait", false, "print the id at once instead of waiting for the grant")
	quiet := fs.Bool("quiet", false, "no progress lines")
	fs.Parse(args)
	if len(pids) == 0 {
		pids = pidList{os.Getppid()}
	}
	req := acquireReq{Kind: *kind, Reason: *reason, Owner: owner(), Pids: pids, Parent: os.Getenv("HOLD_ID")}
	var v holdView
	var err error
	if *noWait {
		err = call(http.MethodPost, "/hold/acquire", req, &v, 30*time.Second)
	} else {
		v, err = acquireAndWait(req, *quiet)
	}
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	} else {
		fmt.Println(v.ID)
	}
	return 0
}

func cmdRelease(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "hold release <id>")
		return 2
	}
	if err := call(http.MethodPost, "/hold/release", map[string]string{"id": args[0], "why": "hold release"}, nil, 10*time.Second); err != nil {
		return fail(err)
	}
	return 0
}

func cmdWait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	timeout := fs.Duration("timeout", 0, "give up after this long (0: never)")
	fs.Parse(args)
	deadline := time.Now().Add(*timeout)
	for {
		var r struct{ Phase, Why string }
		if err := call(http.MethodGet, "/hold/wait-open?timeout=30&who=hold-wait", nil, &r, 45*time.Second); err != nil {
			return fail(err)
		}
		if r.Phase == "open" {
			return 0
		}
		if *timeout > 0 && time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "hold wait: still "+r.Phase+": "+r.Why)
			return 1
		}
	}
}

// ---------- the gate ----------

func cmdGate(args []string) int {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	cfg := Config{Tick: 500 * time.Millisecond, ScanEvery: 2 * time.Second}
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8090", "where clients connect (llama-swap's old address)")
	fs.StringVar(&cfg.Upstream, "upstream", "http://127.0.0.1:8091", "llama-swap")
	fs.StringVar(&cfg.StateFile, "state", filepath.Join(home, ".local/state/hold/state.json"), "holds survive a gate restart here")
	fs.DurationVar(&cfg.DrainTimeout, "drain-timeout", 5*time.Minute, "how long calls in flight may delay a hold")
	fs.DurationVar(&cfg.UnloadTimeout, "unload-timeout", 300*time.Second, "how long to wait for llama-swap's /unload")
	fs.DurationVar(&cfg.HeldRecheck, "held-recheck", 60*time.Second, "while held, how often to check llama-swap stayed empty")
	noImplicit := fs.Bool("no-implicit", false, "do not close the gate for GPU jobs that took no hold")
	fs.Parse(args)
	cfg.Implicit = !*noImplicit
	g, err := NewGate(cfg, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hold gate:", err)
		return 2
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: g, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go g.Run(ctx)
	g.logf("gate listening on %s, upstream %s, drain timeout %s, implicit GPU jobs %v, state %s",
		cfg.Listen, cfg.Upstream, cfg.DrainTimeout, cfg.Implicit, cfg.StateFile)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		g.logf("gate stopped: %v", err)
		return 1
	case <-ctx.Done():
		g.logf("gate shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		return 0
	}
}
