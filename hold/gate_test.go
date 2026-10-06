package main

// Tests against a stand-in llama-swap (2026-10-04). They prove the gate's contract with machine ground
// truth: while a hold is pending or held, no request reaches a model; calls in flight finish first; the unload
// happens before the grant; holds queue in order and die with their processes; streams pass through unbuffered.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- a stand-in llama-swap ----------

type stub struct {
	mu        sync.Mutex
	loaded    []string
	loads     int
	unloads   int
	cancelled int
	delay     time.Duration
	cut       chan struct{}
}

func newStub() *stub { return &stub{delay: 50 * time.Millisecond, cut: make(chan struct{})} }

func (s *stub) load(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.loaded {
		if x == m {
			return
		}
	}
	s.loaded = append(s.loaded, m)
	s.loads++
}

func (s *stub) snapshot() (loaded []string, loads, unloads, cancelled int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.loaded...), s.loads, s.unloads, s.cancelled
}

func (s *stub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var rs []map[string]string
		for _, m := range s.loaded {
			rs = append(rs, map[string]string{"model": m, "state": "ready"})
		}
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"running": rs})
	})
	mux.HandleFunc("/unload", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.unloads++
		s.loaded = nil
		close(s.cut) // like a model process exiting: its streams end
		s.cut = make(chan struct{})
		s.mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		io.WriteString(w, "OK")
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"qwen38"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		s.load("qwen38")
		n := 5
		fmt.Sscan(r.URL.Query().Get("n"), &n)
		s.mu.Lock()
		cut, delay := s.cut, s.delay
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: {\"i\":%d}\n\n", i)
			f.Flush()
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				s.mu.Lock()
				s.cancelled++
				s.mu.Unlock()
				return
			case <-cut:
				return
			}
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/upstream/", func(w http.ResponseWriter, r *http.Request) {
		s.load(strings.Split(strings.TrimPrefix(r.URL.Path, "/upstream/"), "/")[0])
		io.WriteString(w, "logs")
	})
	return mux
}

// ---------- harness ----------

type harness struct {
	t     *testing.T
	stub  *stub
	up    *httptest.Server
	gate  *Gate
	srv   *httptest.Server
	stop  context.CancelFunc
	log   *syncBuf
	state string
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newHarness(t *testing.T, mut func(*Config), scan func() (ProcTable, error)) *harness {
	t.Helper()
	h := &harness{t: t, stub: newStub(), log: &syncBuf{}}
	h.up = httptest.NewServer(h.stub.handler())
	h.state = filepath.Join(t.TempDir(), "state.json")
	cfg := Config{Upstream: h.up.URL, StateFile: h.state, DrainTimeout: 5 * time.Second, UnloadTimeout: 5 * time.Second,
		Implicit: false, Tick: 20 * time.Millisecond, ScanEvery: 60 * time.Millisecond, HeldRecheck: 150 * time.Millisecond}
	if mut != nil {
		mut(&cfg)
	}
	g, err := NewGate(cfg, h.log)
	if err != nil {
		t.Fatal(err)
	}
	if scan != nil {
		g.scan = scan
	}
	h.gate = g
	h.srv = httptest.NewServer(g)
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	go g.Run(ctx)
	t.Cleanup(func() {
		cancel()
		h.srv.Close()
		h.up.Close()
		if t.Failed() {
			t.Log("gate log:\n" + h.log.String())
		}
	})
	return h
}

func (h *harness) chat(n int) (*http.Response, error) {
	return http.Post(fmt.Sprintf("%s/v1/chat/completions?n=%d", h.srv.URL, n), "application/json", strings.NewReader(`{"model":"qwen38","stream":true}`))
}

func (h *harness) acquire(req acquireReq) holdView {
	h.t.Helper()
	var v holdView
	b, _ := json.Marshal(req)
	resp, err := http.Post(h.srv.URL+"/hold/acquire", "application/json", strings.NewReader(string(b)))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("acquire: %d %s", resp.StatusCode, body)
	}
	json.NewDecoder(resp.Body).Decode(&v)
	return v
}

func (h *harness) post(path string, body any) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(h.srv.URL+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
}

func (h *harness) status() statusResp {
	h.t.Helper()
	resp, err := http.Get(h.srv.URL + "/hold/status")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var s statusResp
	json.NewDecoder(resp.Body).Decode(&s)
	return s
}

func (h *harness) holdState(id string) string {
	h.gate.mu.Lock()
	defer h.gate.mu.Unlock()
	return h.gate.viewLocked(id).State
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// ---------- tests ----------

func TestStreamsPassThroughUnbuffered(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.stub.delay = 200 * time.Millisecond
	t0 := time.Now()
	resp, err := h.chat(5)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	rd := bufio.NewReader(resp.Body)
	first, _ := rd.ReadString('\n')
	if el := time.Since(t0); el > 150*time.Millisecond {
		t.Fatalf("first chunk after %s: the gate buffered the stream", el)
	}
	rest, _ := io.ReadAll(rd)
	all := first + string(rest)
	if strings.Count(all, "data: {") != 5 || !strings.Contains(all, "[DONE]") {
		t.Fatalf("stream changed in transit: %q", all)
	}
}

func TestClientAbortCancelsUpstream(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.stub.delay = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", h.srv.URL+"/v1/chat/completions?n=50", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	eventually(t, "the upstream sees the cancel", 2*time.Second, func() bool { _, _, _, c := h.stub.snapshot(); return c == 1 })
	eventually(t, "the call leaves in-flight", time.Second, func() bool { return len(h.status().InFlight) == 0 })
}

func TestHoldDrainsUnloadsThenGrants(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.stub.delay = 100 * time.Millisecond
	// a reply in flight when the hold is asked for
	resp, err := h.chat(10) // about 1 s
	if err != nil {
		t.Fatal(err)
	}
	inflightDone := make(chan string)
	go func() { b, _ := io.ReadAll(resp.Body); resp.Body.Close(); inflightDone <- string(b) }()
	eventually(t, "the call is in flight", time.Second, func() bool { return len(h.status().InFlight) == 1 })

	v := h.acquire(acquireReq{Kind: "render", Reason: "test film", Pids: []int{os.Getpid()}})
	eventually(t, "draining", time.Second, func() bool { return h.holdState(v.ID) == "draining" })

	// while draining: model calls are refused, read-only calls pass, nothing new loads
	r2, _ := h.chat(1)
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 503 || !strings.Contains(string(body), "llm_held") || !strings.Contains(string(body), "Service Unavailable") {
		t.Fatalf("a call while draining got %d %s", r2.StatusCode, body)
	}
	for _, p := range []string{"/running", "/v1/models"} {
		r, err := http.Get(h.srv.URL + p)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("%s while draining: %v %v", p, err, r)
		}
		r.Body.Close()
	}
	if s := h.holdState(v.ID); s != "draining" {
		t.Fatalf("granted while a call was still in flight: %s", s)
	}
	// the call in flight finishes whole, then the model is unloaded, then the hold is granted
	if out := <-inflightDone; !strings.Contains(out, "[DONE]") {
		t.Fatalf("the reply in flight was cut: %q", out)
	}
	eventually(t, "granted", 3*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
	loaded, loads, unloads, _ := h.stub.snapshot()
	if len(loaded) != 0 || unloads != 1 || loads != 1 {
		t.Fatalf("at the grant: loaded %v, loads %d, unloads %d", loaded, loads, unloads)
	}
	// held: /upstream/<model>/... is gated too (it starts the model in llama-swap)
	ru, _ := http.Get(h.srv.URL + "/upstream/qwen38/logs")
	ru.Body.Close()
	if ru.StatusCode != 503 {
		t.Fatalf("/upstream while held: %d", ru.StatusCode)
	}
	if _, loads, _, _ := h.stub.snapshot(); loads != 1 {
		t.Fatalf("a model loaded while held (loads %d)", loads)
	}
	// release: calls pass again and the model comes back on demand
	h.post("/hold/release", map[string]string{"id": v.ID})
	r3, _ := h.chat(1)
	io.ReadAll(r3.Body)
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("after release: %d", r3.StatusCode)
	}
	if _, loads, _, _ := h.stub.snapshot(); loads != 2 {
		t.Fatalf("the model did not come back after release (loads %d)", loads)
	}
}

func sleeper(t *testing.T) int {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c.Process.Pid
}

func TestHoldsQueueInOrder(t *testing.T) {
	h := newHarness(t, nil, nil)
	// two jobs, two processes (one process asking twice is re-entrant: it joins its own hold)
	a := h.acquire(acquireReq{Kind: "render", Reason: "A", Pids: []int{sleeper(t)}})
	b := h.acquire(acquireReq{Kind: "m3d", Reason: "B", Pids: []int{sleeper(t)}})
	if b.Nested || b.ID == a.ID {
		t.Fatalf("B joined A: %+v", b)
	}
	eventually(t, "A granted", 2*time.Second, func() bool { return h.holdState(a.ID) == "granted" })
	time.Sleep(200 * time.Millisecond)
	if s := h.holdState(b.ID); s != "queued" {
		t.Fatalf("B is %s while A holds", s)
	}
	h.post("/hold/release", map[string]string{"id": a.ID})
	eventually(t, "B granted after A", 2*time.Second, func() bool { return h.holdState(b.ID) == "granted" })
	// the gate stayed closed between A and B: no call slipped in
	r, _ := h.chat(1)
	r.Body.Close()
	if r.StatusCode != 503 {
		t.Fatalf("call during B: %d", r.StatusCode)
	}
}

func TestHoldEndsWithItsProcess(t *testing.T) {
	h := newHarness(t, nil, nil)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	v := h.acquire(acquireReq{Kind: "render", Reason: "crashes", Pids: []int{child.Process.Pid}})
	eventually(t, "granted", 2*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
	child.Process.Kill()
	child.Wait()
	eventually(t, "released when its process died", 2*time.Second, func() bool { return h.holdState(v.ID) == "gone" })
	if s := h.status(); s.Phase != "open" {
		t.Fatalf("phase %s after the owner died", s.Phase)
	}
	if !strings.Contains(h.log.String(), "released: its process exited") {
		t.Fatalf("log does not say why:\n%s", h.log.String())
	}
}

func TestManualHoldNeedsHoldOff(t *testing.T) {
	h := newHarness(t, nil, nil)
	v := h.acquire(acquireReq{Kind: "manual", Reason: "benchmark", Pids: []int{os.Getpid()}})
	eventually(t, "granted", 2*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
	h.post("/hold/detach", map[string]any{"id": v.ID})
	time.Sleep(300 * time.Millisecond)
	if s := h.holdState(v.ID); s != "granted" {
		t.Fatalf("a manual hold ended by itself: %s", s)
	}
	h.post("/hold/release", map[string]string{"id": v.ID})
	if s := h.status(); s.Phase != "open" {
		t.Fatalf("after hold off: %s", s.Phase)
	}
}

func TestExpiry(t *testing.T) {
	h := newHarness(t, nil, nil)
	v := h.acquire(acquireReq{Kind: "manual", Reason: "short", Pids: []int{os.Getpid()}, TTL: 1})
	eventually(t, "expired", 3*time.Second, func() bool { return h.holdState(v.ID) == "gone" })
}

// fake process tables for ancestry and implicit jobs
type fakeProcs struct {
	mu sync.Mutex
	t  ProcTable
}

func (f *fakeProcs) set(rows ...Proc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = ProcTable{}
	for _, r := range rows {
		f.t[r.Pid] = r
	}
}
func (f *fakeProcs) scan() (ProcTable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := ProcTable{}
	for k, v := range f.t {
		c[k] = v
	}
	return c, nil
}

const st = "Sun Oct  4 18:00:00 2026"

func TestNestedByAncestryAndHoldID(t *testing.T) {
	fp := &fakeProcs{}
	fp.set(Proc{Pid: 100, Ppid: 1, Start: st, Cmd: "pi"},
		Proc{Pid: 200, Ppid: 100, Start: st, Cmd: "zsh /x/local-video/stories/run-queue.sh p.txt"},
		Proc{Pid: 300, Ppid: 200, Start: st, Cmd: "zsh /x/local-video/stories/story.sh p.txt"},
		Proc{Pid: 400, Ppid: 1, Start: st, Cmd: "zsh"})
	h := newHarness(t, func(c *Config) { c.Implicit = true }, fp.scan)
	v := h.acquire(acquireReq{Kind: "render", Reason: "film", Pids: []int{100}})
	eventually(t, "granted", 2*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
	// run-queue.sh and story.sh under the director's hold join it instead of queueing behind it (a deadlock)
	n := h.acquire(acquireReq{Kind: "render", Reason: "story", Pids: []int{300}})
	if !n.Nested || n.ID != v.ID {
		t.Fatalf("a child of the holder was not nested: %+v", n)
	}
	// an unrelated process with the hold's id in its environment joins it too
	m := h.acquire(acquireReq{Kind: "render", Pids: []int{400}, Parent: v.ID})
	if !m.Nested || m.ID != v.ID {
		t.Fatalf("HOLD_ID was not honoured: %+v", m)
	}
	// and its GPU jobs are not "outside any hold"
	time.Sleep(200 * time.Millisecond)
	if s := h.status(); len(s.Detected) != 0 {
		t.Fatalf("the hold's own render was counted as an outside job: %+v", s.Detected)
	}
}

func TestImplicitJobClosesTheGateAndUnloads(t *testing.T) {
	fp := &fakeProcs{}
	fp.set(Proc{Pid: 10, Ppid: 1, Start: st, Cmd: "zsh"})
	h := newHarness(t, func(c *Config) { c.Implicit = true }, fp.scan)
	r, _ := h.chat(1) // qwen38 is loaded
	io.ReadAll(r.Body)
	r.Body.Close()
	// a render started by hand, with no hold
	fp.set(Proc{Pid: 10, Ppid: 1, Start: st, Cmd: "zsh"},
		Proc{Pid: 11, Ppid: 10, Start: st, Cmd: "/home/x/repos/ltx-2-mlx/.venv/bin/python /home/x/repos/ltx-2-mlx/.venv/bin/ltx-2-mlx generate --prompt a cat"})
	eventually(t, "held by the outside job", 2*time.Second, func() bool { return h.status().Phase == "held" })
	r2, _ := h.chat(1)
	r2.Body.Close()
	if r2.StatusCode != 503 {
		t.Fatalf("a call beside an outside render: %d", r2.StatusCode)
	}
	eventually(t, "the model is unloaded for it", 2*time.Second, func() bool { l, _, _, _ := h.stub.snapshot(); return len(l) == 0 })
	// a hold asked for now, by an unrelated process, waits for the outside job
	fp.set(Proc{Pid: 10, Ppid: 1, Start: st, Cmd: "zsh"},
		Proc{Pid: 11, Ppid: 10, Start: st, Cmd: "/home/x/repos/ltx-2-mlx/.venv/bin/python /home/x/repos/ltx-2-mlx/.venv/bin/ltx-2-mlx generate --prompt a cat"},
		Proc{Pid: 20, Ppid: 1, Start: st, Cmd: "python3 /home/x/.local/bin/m3d mesh x.png"})
	v := h.acquire(acquireReq{Kind: "m3d", Pids: []int{20}})
	time.Sleep(300 * time.Millisecond)
	if s := h.holdState(v.ID); s == "granted" {
		t.Fatal("granted beside a running outside GPU job")
	}
	fp.set(Proc{Pid: 10, Ppid: 1, Start: st, Cmd: "zsh"}, Proc{Pid: 20, Ppid: 1, Start: st, Cmd: "python3 /home/x/.local/bin/m3d mesh x.png"})
	eventually(t, "granted once it ended", 2*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
}

func TestKeepLoadedHoldClosesButDoesNotUnload(t *testing.T) {
	h := newHarness(t, nil, nil)
	r, _ := h.chat(1)
	io.ReadAll(r.Body)
	r.Body.Close()
	v := h.acquire(acquireReq{Kind: "render", Reason: "resident", Pids: []int{os.Getpid()}, Keep: true})
	eventually(t, "granted", 2*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
	time.Sleep(400 * time.Millisecond) // past HeldRecheck: the recheck must leave it loaded too
	if l, _, unloads, _ := h.stub.snapshot(); len(l) != 1 || unloads != 0 {
		t.Fatalf("keep_loaded unloaded the model: loaded %v, unloads %d", l, unloads)
	}
	r2, _ := h.chat(1)
	r2.Body.Close()
	if r2.StatusCode != 503 {
		t.Fatalf("a call during a keep_loaded hold: %d", r2.StatusCode)
	}
}

func TestDrainTimeoutCutsTheStragglers(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DrainTimeout = 300 * time.Millisecond }, nil)
	h.stub.delay = time.Second
	resp, err := h.chat(100) // would take 100 s
	if err != nil {
		t.Fatal(err)
	}
	go func() { io.ReadAll(resp.Body); resp.Body.Close() }()
	eventually(t, "in flight", time.Second, func() bool { return len(h.status().InFlight) == 1 })
	v := h.acquire(acquireReq{Kind: "render", Pids: []int{os.Getpid()}})
	eventually(t, "granted after the drain timeout", 3*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
	if !strings.Contains(h.log.String(), "drain timed out") {
		t.Fatal("the cut is not in the log")
	}
}

func TestLoadBehindTheGateIsUndone(t *testing.T) {
	h := newHarness(t, nil, nil)
	v := h.acquire(acquireReq{Kind: "render", Pids: []int{os.Getpid()}})
	eventually(t, "granted", 2*time.Second, func() bool { return h.holdState(v.ID) == "granted" })
	h.stub.load("qwen38") // someone reached llama-swap directly on its own port
	eventually(t, "unloaded again", 2*time.Second, func() bool { l, _, _, _ := h.stub.snapshot(); return len(l) == 0 })
}

func TestHoldsSurviveAGateRestartButNotAReboot(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	g1, _ := NewGate(Config{Upstream: "http://127.0.0.1:1", StateFile: state}, io.Discard)
	g1.mu.Lock()
	g1.holds = []*Hold{{ID: "hx", Kind: "manual", Reason: "keep", State: "granted", Created: time.Now()}}
	g1.persistLocked()
	g1.mu.Unlock()
	g2, _ := NewGate(Config{Upstream: "http://127.0.0.1:1", StateFile: state}, io.Discard)
	if len(g2.holds) != 1 || g2.holds[0].ID != "hx" {
		t.Fatalf("not restored: %+v", g2.holds)
	}
	b, _ := os.ReadFile(state)
	os.WriteFile(state, []byte(strings.Replace(string(b), g2.boot, "1", 1)), 0o644)
	g3, _ := NewGate(Config{Upstream: "http://127.0.0.1:1", StateFile: state}, io.Discard)
	if len(g3.holds) != 0 {
		t.Fatalf("a hold survived a reboot: %+v", g3.holds)
	}
}

func TestSafeRequests(t *testing.T) {
	cases := map[string]bool{
		"GET /running": true, "GET /unload": true, "GET /v1/models": true, "GET /logs/stream/upstream": true,
		"GET /ui/index.html": true, "GET /api/events": true, "POST /api/models/unload": true, "GET /health": true,
		"POST /v1/chat/completions": false, "POST /v1/completions": false, "POST /v1/embeddings": false,
		"GET /upstream/qwen38/logs": false, "POST /api/v1/chat/completions": false, "POST /api/profiles/active": false,
		"POST /completion": false, "POST /v1/messages": false, "POST /v1/responses": false, "GET /api/mcp": false,
	}
	for c, want := range cases {
		f := strings.SplitN(c, " ", 2)
		r := httptest.NewRequest(f[0], f[1], nil)
		if got := safeRequest(r); got != want {
			t.Errorf("%s: safe=%v, want %v", c, got, want)
		}
	}
}

func TestGpuJob(t *testing.T) {
	cases := map[string]string{
		"/home/m/repos/ltx-2-mlx/.venv/bin/python /home/m/repos/ltx-2-mlx/.venv/bin/ltx-2-mlx generate --prompt x": "ltx-2-mlx",
		"/home/m/repos/ltx-2-mlx/.venv/bin/python -c from multiprocessing.resource_tracker import main;main(7)":     "ltx-2-mlx",
		"zsh /home/m/repos/local-video/stories/run-queue.sh /p.txt":                                                 "run-queue.sh",
		"zsh /home/m/repos/local-video/stories/story.sh /p.txt":                                                     "story.sh",
		"/opt/homebrew/bin/python3 /home/m/.local/bin/vidgen --no-open -o x.mp4 a cat":                              "vidgen",
		"/opt/homebrew/bin/python3 /home/m/repos/local-video/bin/still -o s.png x":                                  "still",
		"/home/m/.local/share/uv/tools/mflux/bin/python /home/m/.local/bin/mflux-generate-qwen --prompt x":         "mflux-generate",
		"/usr/bin/time -l hy3d generate --image x.png":                                                               "hy3d",
		"grep -r story.sh /home/m/repos/local-video":                                                                "",
		"zsh -c until ! pgrep -f ltx-2-mlx; do sleep 5; done":                                                        "",
		"less /home/m/repos/local-video/stories/story.sh":                                                           "",
		"git -C /home/m/repos/ltx-2-mlx pull":                                                                       "",
		"/home/m/repos/ds4/ds4-server --chdir /home/m/repos/ds4 -m x.gguf":                                         "",
		"/home/m/repos/mlx-audio/.venv/bin/python /home/m/.pi/agent/skills/dictate/scripts/sttd.py serve":          "",
		"hy3d --help": "",
		"tail -f /home/m/repos/local-video/logs/story-x.log": "",
	}
	for cmd, want := range cases {
		if got := gpuJob(cmd); got != want {
			t.Errorf("%q: %q, want %q", cmd, got, want)
		}
	}
}

func TestParsePs(t *testing.T) {
	out := []byte("    1     0 Mon Sep 21 15:58:38 2026     /sbin/launchd\n42532 35699 Sun Oct  4 18:58:01 2026 /opt/homebrew/bin/python3 /home/m/.local/bin/vidgen --no-open\n 99   1 Sun Oct  4 18:58:01 2026\n")
	tb := parsePs(out)
	if p := tb[42532]; p.Ppid != 35699 || p.Start != "Sun Oct 4 18:58:01 2026" || !strings.HasSuffix(p.Cmd, "vidgen --no-open") {
		t.Fatalf("%+v", p)
	}
	if !tb.alive(PidRef{Pid: 42532, Start: "Sun Oct 4 18:58:01 2026"}) || tb.alive(PidRef{Pid: 42532, Start: "Sun Oct 4 18:00:00 2026"}) {
		t.Fatal("liveness must compare start times (pids are reused)")
	}
	if _, ok := tb[99]; !ok {
		t.Fatal("a process with an empty command line was dropped")
	}
}
