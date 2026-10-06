package main

// The hold gate (2026-10-04): one lock for the Mac's GPU, enforced where every model load passes.
//
// The gate listens where llama-swap used to (127.0.0.1:8090) and passes every request through to it (now on
// :8091). A hold is an exclusive claim on the GPU, taken by a render, an m3d stage or the human (`hold on`). While
// any hold exists, or a GPU job runs outside one, the gate is closed: requests that could start a model are
// refused with a 503 that says who holds the GPU, and pi's hold extension waits instead of sending them. Model calls
// share the open gate freely.
//
// A new hold first closes the gate, lets the calls already in flight finish (up to DrainTimeout), unloads
// llama-swap, and only then is granted; holds queue first come, first served. A hold dies with its processes (pid
// plus start time, checked every ScanEvery), so a crashed job cannot keep the LLM off. Why: the rule of
// 2026-10-04 is that nothing may bring the model back while the GPU is deliberately held, for every client (pi,
// Codex, LDR, curl), not just pi's film-rig hook, which only knew LTX renders.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type PidRef struct {
	Pid   int    `json:"pid"`
	Start string `json:"start,omitempty"`
	Cmd   string `json:"cmd,omitempty"`
}

type Hold struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Reason  string    `json:"reason"`
	Owner   string    `json:"owner,omitempty"`
	Pids    []PidRef  `json:"pids,omitempty"` // empty: a manual hold, kept until `hold off` or Expires
	Created time.Time `json:"created"`
	State   string    `json:"state"` // queued | draining | granted
	DrainAt time.Time `json:"drain_at,omitzero"`
	GrantAt time.Time `json:"granted_at,omitzero"`
	Expires time.Time `json:"expires,omitzero"`
	Note    string    `json:"note,omitempty"`
	// KeepLoaded: close the gate and drain, but leave the loaded model where it is (the film rig's resident mode, a
	// small model beside a 480p render, 2026-09-23). Nothing may call it while held; the gate still refuses.
	KeepLoaded bool `json:"keep_loaded,omitempty"`
}

type Flight struct {
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	Agent   string    `json:"agent,omitempty"`
	Started time.Time `json:"started"`
}

// Detected is a GPU job running outside every hold's process tree.
type Detected struct {
	Pid   int       `json:"pid"`
	Ppid  int       `json:"ppid"`
	Root  bool      `json:"root"` // its parent is not a detected job: the one line status shows for a whole render
	Name  string    `json:"name"`
	Cmd   string    `json:"cmd"`
	Since time.Time `json:"since"`
}

type Config struct {
	Listen        string
	Upstream      string
	StateFile     string
	DrainTimeout  time.Duration // how long calls in flight may delay a hold before the unload cuts them
	UnloadTimeout time.Duration // llama-swap waits up to unloadTimeout (240 s on ds4 rows) for a model to exit
	Implicit      bool          // close the gate for GPU jobs that took no hold
	Tick          time.Duration
	ScanEvery     time.Duration
	HeldRecheck   time.Duration // while held, how often to check that nothing was loaded behind the gate
}

type Gate struct {
	cfg   Config
	up    *url.URL
	proxy *httputil.ReverseProxy
	ctl   *http.Client
	logw  io.Writer
	logmu sync.Mutex
	scan  func() (ProcTable, error)
	kickc chan struct{}

	mu         sync.Mutex
	holds      []*Hold
	flights    map[uint64]*Flight
	nextFlight uint64
	procs      ProcTable
	detected   []Detected
	detSince   map[string]time.Time
	engines    []PidRef
	running    []string
	runningOK  bool
	runningAt  time.Time // last /running read, by anyone (status refreshes it for display)
	checkedAt  time.Time // last /running read by step() while held: the enforcement clock
	unloading  bool
	closedAt   time.Time
	waiting    map[string]int
	changed    chan struct{}
	boot       string
}

func NewGate(cfg Config, logw io.Writer) (*Gate, error) {
	up, err := url.Parse(cfg.Upstream)
	if err != nil || up.Host == "" {
		return nil, fmt.Errorf("bad upstream %q", cfg.Upstream)
	}
	g := &Gate{cfg: cfg, up: up, logw: logw, scan: scanProcs, kickc: make(chan struct{}, 1),
		flights: map[uint64]*Flight{}, detSince: map[string]time.Time{}, waiting: map[string]int{},
		changed: make(chan struct{}), procs: ProcTable{}}
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // pass bytes through as llama-swap sent them
		// No ResponseHeaderTimeout: a non-streaming request's headers come after the whole generation.
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = pr.In.Host
		},
		FlushInterval: -1, // every chunk at once: pi and Codex stream server-sent events
		Transport:     tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				return // the client went away; the upstream request was cancelled with it
			}
			g.logf("upstream error on %s %s: %v", r.Method, r.URL.Path, err)
			writeError(w, http.StatusBadGateway, "upstream_unreachable",
				fmt.Sprintf("502 Bad Gateway: llama-swap did not answer behind the hold gate (%v)", err))
		},
	}
	g.ctl = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}}
	g.boot = bootTime()
	g.load()
	return g, nil
}

func (g *Gate) logf(format string, a ...any) {
	g.logmu.Lock()
	defer g.logmu.Unlock()
	fmt.Fprintf(g.logw, "%s [hold] %s\n", time.Now().Format("Jan _2 15:04:05"), fmt.Sprintf(format, a...))
}

func (g *Gate) kick() {
	select {
	case g.kickc <- struct{}{}:
	default:
	}
}

func (g *Gate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// ---------- the proxy ----------

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/hold" || strings.HasPrefix(r.URL.Path, "/hold/") {
		g.api(w, r)
		return
	}
	if safeRequest(r) {
		g.proxy.ServeHTTP(w, r)
		return
	}
	g.mu.Lock()
	if phase, why := g.phaseLocked(); phase != "open" {
		g.mu.Unlock()
		g.refuse(w, r, why)
		return
	}
	g.nextFlight++
	id := g.nextFlight
	g.flights[id] = &Flight{Method: r.Method, Path: r.URL.Path, Agent: r.UserAgent(), Started: time.Now()}
	g.mu.Unlock()
	defer func() { // runs on the abort panic of a cut stream too
		g.mu.Lock()
		delete(g.flights, id)
		g.notifyLocked()
		g.mu.Unlock()
		g.kick()
	}()
	g.proxy.ServeHTTP(w, r)
}

// safeRequest: requests that never start a model, passed through even while the GPU is held. Everything else can
// load one and is gated: any POST under /v1/, the UI playground (/api/v1/), profiles, MCP, and /upstream/<model>/...
// of any method (a GET /upstream/qwen38/logs started qwen38 on 2026-10-02, under memory pressure).
func safeRequest(r *http.Request) bool {
	p, read := r.URL.Path, r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case p == "/running" || p == "/unload" || p == "/health" || p == "/" || p == "/favicon.ico":
		return true
	case p == "/v1/models" || p == "/models" || strings.HasPrefix(p, "/models/"):
		return read
	case strings.HasPrefix(p, "/logs") || strings.HasPrefix(p, "/ui"):
		return true
	case strings.HasPrefix(p, "/api/models/unload") || strings.HasPrefix(p, "/api/inflight"):
		return true // unloading and cancelling never load
	case strings.HasPrefix(p, "/api/v1/") || strings.HasPrefix(p, "/api/mcp"):
		return false
	case strings.HasPrefix(p, "/api/"):
		return read // events, metrics, version, hardware...; a POST (profiles, jobs) might load
	}
	return false
}

func (g *Gate) refuse(w http.ResponseWriter, r *http.Request, why string) {
	w.Header().Set("Retry-After", "30")
	writeError(w, http.StatusServiceUnavailable, "llm_held", "503 Service Unavailable: the local LLM is held ("+why+
		"). This request was not sent to the model, so nothing was loaded. pi waits and continues by itself; "+
		"anything else: retry when `hold status` says open.")
	g.logf("refused %s %s from %q: %s", r.Method, r.URL.Path, r.UserAgent(), why)
}

func writeError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	enc.Encode(v)
}

// ---------- state ----------

// phaseLocked: open (model calls pass), draining (a hold is next: calls in flight finish, the model is unloaded),
// or held (a hold is granted, or a GPU job runs outside any hold).
func (g *Gate) phaseLocked() (string, string) {
	if len(g.holds) > 0 {
		h := g.holds[0]
		if h.State == "granted" {
			return "held", describe(h)
		}
		return "draining", "about to be held by " + describe(h)
	}
	if len(g.detected) > 0 {
		d := g.detected[0]
		for _, x := range g.detected {
			if x.Root {
				d = x
				break
			}
		}
		return "held", fmt.Sprintf("GPU job %s (pid %d) running outside any hold", d.Name, d.Pid)
	}
	return "open", ""
}

func describe(h *Hold) string {
	s := h.Kind
	if h.Reason != "" {
		s += " " + strconv.Quote(h.Reason)
	}
	t := h.Created
	if !h.GrantAt.IsZero() {
		t = h.GrantAt
	}
	s += " since " + t.Format("15:04")
	if len(h.Pids) == 0 {
		s += ", manual: `hold off` releases it"
		if !h.Expires.IsZero() {
			s += " (or it ends at " + h.Expires.Format("15:04") + ")"
		}
	}
	return s
}

func (g *Gate) find(id string) *Hold {
	for _, h := range g.holds {
		if h.ID == id {
			return h
		}
	}
	return nil
}

// grantedFor: the granted hold a new request belongs inside, if any (the caller inherited HOLD_ID, or one of its
// pids or their ancestors already owns a granted hold: story.sh under run-queue.sh, vidgen under story.sh).
func (g *Gate) grantedFor(parent string, pids []int) *Hold {
	if h := g.find(parent); h != nil && h.State == "granted" {
		return h
	}
	owner := map[int]*Hold{}
	for _, h := range g.holds {
		if h.State != "granted" {
			continue
		}
		for _, p := range h.Pids {
			if g.procs.alive(p) {
				owner[p.Pid] = h
			}
		}
	}
	for _, pid := range pids {
		for _, a := range g.procs.chain(pid) {
			if h := owner[a]; h != nil {
				return h
			}
		}
	}
	return nil
}

// ---------- the loop ----------

func (g *Gate) Run(ctx context.Context) {
	g.scanTick()
	tick := time.NewTicker(g.cfg.Tick)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-g.kickc:
		}
		if time.Since(last) >= g.cfg.ScanEvery {
			g.scanTick()
			last = time.Now()
		}
		g.step()
	}
}

// scanTick: one ps scan; drops holds whose processes are gone or whose time is up, and finds GPU jobs outside holds.
func (g *Gate) scanTick() {
	t, err := g.scan()
	if err != nil {
		g.logf("ps failed: %v", err)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.procs = t
	now := time.Now()
	kept, changed := g.holds[:0], false
	for _, h := range g.holds {
		switch {
		case !h.Expires.IsZero() && now.After(h.Expires):
			g.logf("hold %s ended: its time ran out (%s)", h.ID, describe(h))
			changed = true
		case len(h.Pids) > 0 && !anyAlive(t, h.Pids):
			g.logf("hold %s released: its process%s exited (%s)", h.ID, plural(len(h.Pids), "", "es"), describe(h))
			changed = true
		default:
			for i, p := range h.Pids { // `hold run` execs the job: show what the pid runs now, not "hold run"
				if pr, ok := t[p.Pid]; ok && t.alive(p) {
					h.Pids[i].Cmd = label(pr.Cmd)
				}
			}
			kept = append(kept, h)
		}
	}
	for i := len(kept); i < len(g.holds); i++ {
		g.holds[i] = nil
	}
	g.holds = kept
	g.detected = g.detectLocked(t, now)
	g.engines = enginesOutside(t)
	if changed {
		g.persistLocked()
		g.notifyLocked()
	}
}

func anyAlive(t ProcTable, refs []PidRef) bool {
	for _, r := range refs {
		if t.alive(r) {
			return true
		}
	}
	return false
}

func (g *Gate) detectLocked(t ProcTable, now time.Time) []Detected {
	if !g.cfg.Implicit {
		return nil
	}
	held := map[int]bool{os.Getpid(): true}
	for _, h := range g.holds { // queued holds too: a waiting runner is not a running GPU job
		for _, p := range h.Pids {
			if t.alive(p) {
				held[p.Pid] = true
			}
		}
	}
	var out []Detected
	seen := map[string]bool{}
	for pid, p := range t {
		name := gpuJob(p.Cmd)
		if name == "" {
			continue
		}
		inside := false
		for _, a := range t.chain(pid) {
			if held[a] {
				inside = true
				break
			}
		}
		if inside {
			continue
		}
		key := strconv.Itoa(pid) + "|" + p.Start
		seen[key] = true
		since, ok := g.detSince[key]
		if !ok {
			since = now
			g.detSince[key] = now
			g.logf("GPU job outside any hold: pid %d %s (%s); model calls are refused until it ends", pid, name, trunc(p.Cmd, 120))
		}
		out = append(out, Detected{Pid: pid, Ppid: p.Ppid, Name: name, Cmd: trunc(p.Cmd, 160), Since: since})
	}
	in := map[int]bool{}
	for _, d := range out {
		in[d.Pid] = true
	}
	for i := range out {
		out[i].Root = !in[out[i].Ppid]
	}
	for key := range g.detSince {
		if !seen[key] {
			delete(g.detSince, key)
			g.logf("GPU job pid %s ended", strings.SplitN(key, "|", 2)[0])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Since.Before(out[j].Since) || (out[i].Since.Equal(out[j].Since) && out[i].Pid < out[j].Pid)
	})
	return out
}

func enginesOutside(t ProcTable) []PidRef {
	swap := map[int]bool{}
	for pid, p := range t {
		if f := strings.Fields(p.Cmd); len(f) > 0 && filepath.Base(f[0]) == "llama-swap" {
			swap[pid] = true
		}
	}
	var out []PidRef
	for pid, p := range t {
		if !llmEngine(p.Cmd) || swap[p.Ppid] {
			continue
		}
		out = append(out, PidRef{Pid: pid, Start: p.Start, Cmd: label(p.Cmd)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pid < out[j].Pid })
	return out
}

// step advances the head hold: queued -> draining (the gate closes) -> unload -> granted.
func (g *Gate) step() {
	now := time.Now()
	g.mu.Lock()
	if len(g.holds) > 0 && g.holds[0].State == "queued" {
		h := g.holds[0]
		h.State, h.DrainAt = "draining", now
		g.logf("hold %s is next (%s): model calls are refused from now; %d in flight finish first", h.ID, describe(h), len(g.flights))
		g.persistLocked()
		g.notifyLocked()
	}
	if len(g.holds) == 0 && len(g.detected) == 0 {
		g.closedAt = time.Time{}
		g.mu.Unlock()
		return
	}
	if g.closedAt.IsZero() {
		g.closedAt = now
	}
	if g.unloading {
		g.mu.Unlock()
		return
	}
	var head *Hold
	if len(g.holds) > 0 {
		head = g.holds[0]
	}
	granting := head != nil && head.State == "draining"
	drainFrom := g.closedAt
	if granting {
		drainFrom = head.DrainAt
	}
	inflight := len(g.flights)
	timedOut := inflight > 0 && now.Sub(drainFrom) > g.cfg.DrainTimeout
	if inflight > 0 && !timedOut {
		g.mu.Unlock()
		return
	}
	if !granting && now.Sub(g.checkedAt) < g.cfg.HeldRecheck {
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()

	running, ok := g.fetchRunning()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.running, g.runningOK, g.runningAt, g.checkedAt = running, ok, time.Now(), time.Now()
	keep := len(g.holds) > 0 && g.holds[0].KeepLoaded
	if ok && len(running) > 0 && !keep {
		switch {
		case timedOut:
			g.logf("drain timed out after %s with %d call(s) in flight: unloading anyway, which cuts them", g.cfg.DrainTimeout, inflight)
			if head != nil {
				head.Note = fmt.Sprintf("drain timed out after %s; %d call(s) cut", g.cfg.DrainTimeout, inflight)
			}
		case !granting:
			g.logf("%s loaded while the GPU is held: unloading it again", strings.Join(running, ", "))
		}
		g.unloading = true
		go g.unload(running)
		return
	}
	if !granting || len(g.holds) == 0 || g.holds[0] != head || head.State != "draining" {
		return
	}
	if len(g.flights) > 0 && !timedOut {
		return // a call slipped in between the two locks; wait for it
	}
	if len(g.detected) > 0 {
		return // a GPU job outside every hold is still running; this hold waits for it to end
	}
	head.State, head.GrantAt = "granted", time.Now()
	if !ok {
		head.Note = "llama-swap did not answer /running when this hold was granted"
	}
	g.logf("hold %s granted after %s: %s", head.ID, head.GrantAt.Sub(head.Created).Round(time.Second), describe(head))
	g.persistLocked()
	g.notifyLocked()
}

func (g *Gate) fetchRunning() ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.up.String()+"/running", nil)
	resp, err := g.ctl.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var body struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil, false
	}
	out := []string{}
	for _, m := range body.Running {
		out = append(out, m.Model+" ("+m.State+")")
	}
	return out, true
}

func (g *Gate) unload(models []string) {
	t0 := time.Now()
	g.logf("unloading %s through llama-swap /unload", strings.Join(models, ", "))
	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.UnloadTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.up.String()+"/unload", nil)
	resp, err := g.ctl.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unloading = false
	g.checkedAt = time.Time{} // look again at once
	if err != nil {
		g.logf("unload failed after %s: %v", time.Since(t0).Round(time.Millisecond), err)
	} else {
		g.logf("unload returned after %s", time.Since(t0).Round(time.Millisecond))
	}
	g.notifyLocked()
	g.kick()
}

// ---------- persistence ----------

type savedState struct {
	Boot  string  `json:"boot"`
	Holds []*Hold `json:"holds"`
}

func (g *Gate) persistLocked() {
	if g.cfg.StateFile == "" {
		return
	}
	b, _ := json.MarshalIndent(savedState{Boot: g.boot, Holds: g.holds}, "", " ")
	tmp := g.cfg.StateFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(g.cfg.StateFile), 0o755); err != nil {
		g.logf("state: %v", err)
		return
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		g.logf("state: %v", err)
		return
	}
	if err := os.Rename(tmp, g.cfg.StateFile); err != nil {
		g.logf("state: %v", err)
	}
}

// load restores the holds of a gate that restarted (a crash, a launchd restart): a render that took a hold before
// must not lose it. After a reboot nothing survives, manual holds included: every process they named is gone, and a
// forgotten `hold on` should not keep the LLM off a freshly booted Mac.
func (g *Gate) load() {
	if g.cfg.StateFile == "" {
		return
	}
	b, err := os.ReadFile(g.cfg.StateFile)
	if err != nil {
		return
	}
	var s savedState
	if json.Unmarshal(b, &s) != nil {
		g.logf("state file %s unreadable; starting with no holds", g.cfg.StateFile)
		return
	}
	if s.Boot != g.boot {
		if len(s.Holds) > 0 {
			g.logf("the Mac rebooted since the last state; dropping %d hold(s)", len(s.Holds))
		}
		return
	}
	g.holds = s.Holds
	for _, h := range g.holds {
		g.logf("restored hold %s (%s, %s)", h.ID, h.State, describe(h))
	}
}

var bootRe = regexp.MustCompile(`sec = (\d+)`)

func bootTime() string {
	out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return ""
	}
	if m := bootRe.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// ---------- the API under /hold/ ----------

type acquireReq struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Owner  string `json:"owner"`
	Pids   []int  `json:"pids"`
	Parent string `json:"parent"` // the HOLD_ID the caller inherited
	TTL    int    `json:"ttl_seconds"`
	Keep   bool   `json:"keep_loaded"`
}

type holdView struct {
	ID        string   `json:"id"`
	State     string   `json:"state"` // queued | draining | granted | gone
	Nested    bool     `json:"nested,omitempty"`
	Position  int      `json:"position"`
	Phase     string   `json:"phase"`
	Why       string   `json:"why,omitempty"`
	InFlight  int      `json:"in_flight"`
	Running   []string `json:"running"`
	Unloading bool     `json:"unloading"`
	Hold      *Hold    `json:"hold,omitempty"` // a copy: encoded after the lock is released
}

func (g *Gate) viewLocked(id string) holdView {
	phase, why := g.phaseLocked()
	v := holdView{ID: id, State: "gone", Phase: phase, Why: why, InFlight: len(g.flights), Running: g.running, Unloading: g.unloading}
	for i, h := range g.holds {
		if h.ID == id {
			c := *h
			c.Pids = append([]PidRef(nil), h.Pids...)
			v.State, v.Position, v.Hold = h.State, i, &c
		}
	}
	return v
}

type statusResp struct {
	Phase     string         `json:"phase"`
	Why       string         `json:"why,omitempty"`
	Holds     []Hold         `json:"holds"`
	Detected  []Detected     `json:"detected"`
	InFlight  []*Flight      `json:"in_flight"`
	Running   []string       `json:"running"`
	RunningOK bool           `json:"running_ok"`
	Unloading bool           `json:"unloading"`
	Waiting   map[string]int `json:"waiting"`
	Engines   []PidRef       `json:"engines_outside_llama_swap,omitempty"`
	Upstream  string         `json:"upstream"`
	Now       time.Time      `json:"now"`
}

func (g *Gate) statusLocked() statusResp {
	phase, why := g.phaseLocked()
	fl := make([]*Flight, 0, len(g.flights)) // never changed after creation
	for _, f := range g.flights {
		fl = append(fl, f)
	}
	sort.Slice(fl, func(i, j int) bool { return fl[i].Started.Before(fl[j].Started) })
	wt := map[string]int{}
	for k, n := range g.waiting {
		if n > 0 {
			wt[k] = n
		}
	}
	holds := make([]Hold, 0, len(g.holds))
	for _, h := range g.holds {
		c := *h
		c.Pids = append([]PidRef(nil), h.Pids...)
		holds = append(holds, c)
	}
	det := g.detected
	if det == nil {
		det = []Detected{}
	}
	run := g.running
	if run == nil {
		run = []string{}
	}
	return statusResp{Phase: phase, Why: why, Holds: holds, Detected: det, InFlight: fl, Running: run, RunningOK: g.runningOK,
		Unloading: g.unloading, Waiting: wt, Engines: g.engines, Upstream: g.up.String(), Now: time.Now()}
}

func (g *Gate) api(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/hold/status":
		if g.cfg.HeldRecheck > 0 && time.Since(g.runningAtSafe()) > 3*time.Second && !g.isUnloading() {
			running, ok := g.fetchRunning()
			g.mu.Lock()
			g.running, g.runningOK, g.runningAt = running, ok, time.Now()
			g.mu.Unlock()
		}
		g.mu.Lock()
		s := g.statusLocked()
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, s)
	case "/hold/acquire":
		var req acquireReq
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "POST a JSON body {kind, reason, owner, pids, parent, ttl_seconds}")
			return
		}
		v, err := g.acquire(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, v)
	case "/hold/wait":
		g.apiWait(w, r)
	case "/hold/wait-open":
		g.apiWaitOpen(w, r)
	case "/hold/attach", "/hold/detach", "/hold/release":
		var req struct {
			ID   string `json:"id"`
			Pids []int  `json:"pids"`
			TTL  int    `json:"ttl_seconds"`
			Why  string `json:"why"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || req.ID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "POST a JSON body with id")
			return
		}
		g.mu.Lock()
		h := g.find(req.ID)
		if h == nil {
			g.mu.Unlock()
			writeError(w, http.StatusNotFound, "not_found", "no hold "+req.ID)
			return
		}
		switch r.URL.Path {
		case "/hold/attach":
			for _, pid := range req.Pids {
				p, ok := g.procs[pid]
				if !ok && g.refreshHas(pid) {
					p, ok = g.procs[pid]
				}
				if ok && !containsPid(h.Pids, pid) {
					h.Pids = append(h.Pids, PidRef{Pid: pid, Start: p.Start, Cmd: label(p.Cmd)})
					g.logf("hold %s: pid %d attached (%s)", h.ID, pid, trunc(p.Cmd, 80))
				}
			}
		case "/hold/detach":
			h.Pids = nil
			if req.TTL > 0 {
				h.Expires = time.Now().Add(time.Duration(req.TTL) * time.Second)
			}
			g.logf("hold %s is now manual (%s)", h.ID, describe(h))
		case "/hold/release":
			g.removeLocked(h, "released"+suffix(req.Why))
		}
		g.persistLocked()
		g.notifyLocked()
		v := g.viewLocked(req.ID)
		g.mu.Unlock()
		g.kick()
		writeJSON(w, http.StatusOK, v)
	default:
		writeError(w, http.StatusNotFound, "not_found", "hold API: /hold/status, acquire, wait, wait-open, attach, detach, release")
	}
}

func suffix(why string) string {
	if why == "" {
		return ""
	}
	return " (" + why + ")"
}

func (g *Gate) runningAtSafe() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runningAt
}

func (g *Gate) isUnloading() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unloading
}

// refreshHas rescans the process table for a pid started after the last scan (called with g.mu held).
func (g *Gate) refreshHas(pid int) bool {
	t, err := g.scan()
	if err != nil {
		return false
	}
	g.procs = t
	_, ok := t[pid]
	return ok
}

func (g *Gate) removeLocked(h *Hold, why string) {
	for i, x := range g.holds {
		if x == h {
			g.holds = append(g.holds[:i], g.holds[i+1:]...)
			break
		}
	}
	held := ""
	if !h.GrantAt.IsZero() {
		held = " after " + time.Since(h.GrantAt).Round(time.Second).String()
	}
	g.logf("hold %s %s%s: %s", h.ID, why, held, describe(h))
}

func (g *Gate) acquire(req acquireReq) (holdView, error) {
	t, err := g.scan() // fresh: the caller may have started a moment ago
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.procs = t
	}
	var refs []PidRef
	for _, pid := range req.Pids {
		p, ok := g.procs[pid]
		if !ok {
			return holdView{}, fmt.Errorf("pid %d is not running", pid)
		}
		refs = append(refs, PidRef{Pid: pid, Start: p.Start, Cmd: label(p.Cmd)})
	}
	if h := g.grantedFor(req.Parent, req.Pids); h != nil {
		for _, r := range refs {
			if !containsPid(h.Pids, r.Pid) {
				h.Pids = append(h.Pids, r)
			}
		}
		g.persistLocked()
		v := g.viewLocked(h.ID)
		v.Nested = true
		return v, nil
	}
	kind := req.Kind
	if kind == "" {
		kind = "job"
	}
	h := &Hold{ID: newID(), Kind: kind, Reason: req.Reason, Owner: req.Owner, Pids: refs, Created: time.Now(), State: "queued", KeepLoaded: req.Keep}
	if req.TTL > 0 {
		h.Expires = h.Created.Add(time.Duration(req.TTL) * time.Second)
	}
	g.holds = append(g.holds, h)
	who := req.Owner
	if who == "" && len(refs) > 0 {
		who = fmt.Sprintf("pid %d", refs[0].Pid)
	}
	g.logf("hold %s requested by %s: %s (position %d)", h.ID, who, describe(h), len(g.holds)-1)
	g.persistLocked()
	g.notifyLocked()
	g.kick()
	return g.viewLocked(h.ID), nil
}

func containsPid(refs []PidRef, pid int) bool {
	for _, r := range refs {
		if r.Pid == pid {
			return true
		}
	}
	return false
}

func waitTimeout(r *http.Request, def time.Duration) time.Duration {
	if s := r.URL.Query().Get("timeout"); s != "" {
		if n, err := strconv.ParseFloat(s, 64); err == nil && n >= 0 {
			d := time.Duration(n * float64(time.Second))
			if d > 120*time.Second {
				d = 120 * time.Second
			}
			return d
		}
	}
	return def
}

// /hold/wait?id=&timeout=: returns when the hold is granted or gone, or at the timeout with its current state.
func (g *Gate) apiWait(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	deadline := time.Now().Add(waitTimeout(r, 30*time.Second))
	for {
		g.mu.Lock()
		v := g.viewLocked(id)
		ch := g.changed
		g.mu.Unlock()
		if v.State == "granted" || v.State == "gone" || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, v)
			return
		}
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
		case <-r.Context().Done():
			return
		}
	}
}

// /hold/wait-open?timeout=&who=: returns when model calls may pass again, or at the timeout; `who` is counted in
// status so the human can see what is waiting.
func (g *Gate) apiWaitOpen(w http.ResponseWriter, r *http.Request) {
	who := r.URL.Query().Get("who")
	if who == "" {
		who = "unnamed"
	}
	deadline := time.Now().Add(waitTimeout(r, 30*time.Second))
	counted := false
	defer func() {
		if counted {
			g.mu.Lock()
			g.waiting[who]--
			g.mu.Unlock()
		}
	}()
	for {
		g.mu.Lock()
		phase, why := g.phaseLocked()
		ch := g.changed
		if phase != "open" && !counted && time.Now().Before(deadline) {
			g.waiting[who]++
			counted = true
		}
		g.mu.Unlock()
		if phase == "open" || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, map[string]any{"phase": phase, "why": why})
			return
		}
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
		case <-r.Context().Done():
			return
		}
	}
}

func newID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return "h" + hex.EncodeToString(b)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
