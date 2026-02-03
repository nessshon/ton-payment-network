package testnode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	cfgpkg "github.com/xssnick/ton-payment-network/tonpayments/config"
)

// API models (subset) copied to avoid importing server package in tests
// Keep minimal fields for assertions and easy future extension.
type apiSide struct {
	Key string `json:"key"`
}

type apiChannel struct {
	Address          string  `json:"address"`
	Our              apiSide `json:"our"`
	Their            apiSide `json:"their"`
	Status           string  `json:"status"`
	AcceptingActions bool    `json:"accepting_actions"`
}

type apiOpenReq struct {
	WithNode string `json:"with_node"`
}

type apiOpenResp struct {
	Address string `json:"address"`
}

type testNode struct {
	Idx        int
	Dir        string
	ConfigPath string
	APIAddr    string
	Cmd        *exec.Cmd
	Cancel     context.CancelFunc
	KeyBase64  string
	DBPath     string
	LogPath    string

	outRB *ringBuf
	errRB *ringBuf
}

// ringBuf is a fixed-size ring buffer for recent log lines.
// It is goroutine-safe for appends and snapshots.
// Not optimized for performance — intended for test diagnostics only.
type ringBuf struct {
	mu    sync.Mutex
	buf   []string
	cap   int
	count int // total lines written, used for rotation index
}

func newRingBuf(capacity int) *ringBuf {
	if capacity <= 0 {
		capacity = 1
	}
	return &ringBuf{cap: capacity, buf: make([]string, 0, capacity)}
}

func (r *ringBuf) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, s)
	} else {
		pos := r.count % r.cap
		r.buf[pos] = s
	}
	r.count++
}

// last returns up to max latest lines in chronological order.
func (r *ringBuf) last(max int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return nil
	}
	if max <= 0 || max > len(r.buf) {
		max = len(r.buf)
	}
	// Determine start index in circular buffer.
	total := len(r.buf)
	start := 0
	if r.count >= total {
		start = r.count % total
	}
	res := make([]string, 0, max)
	for i := 0; i < total && len(res) < max; i++ {
		idx := (start + i) % total
		res = append(res, r.buf[idx])
	}
	// We only want the last 'max' lines
	if len(res) > max {
		res = res[len(res)-max:]
	}
	return res
}

func TestIntegration_ThreeNodes_InitPairs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	root := repoRoot(t)
	baseTestDir := filepath.Join(root, "test")
	must(os.MkdirAll(baseTestDir, 0o755))

	// Allocate unique ports so runs don't conflict with anything else.
	// You can adjust base ports below if needed.
	baseAPI := 19080
	baseADNL := 17600
	baseMetrics := 18100

	nodes := make([]*testNode, 0, 3)
	for i := 1; i <= 3; i++ {
		n := &testNode{Idx: i}
		n.Dir = filepath.Join(baseTestDir, fmt.Sprint(i))
		n.ConfigPath = filepath.Join(n.Dir, "payment-network-config.json")
		n.APIAddr = fmt.Sprintf("127.0.0.1:%d", baseAPI+i)
		n.LogPath = filepath.Join(n.Dir, "payment-network.log")
		must(os.MkdirAll(n.Dir, 0o755))

		t.Logf("[%d] preparing config dir=%s", i, n.Dir)
		cfg := ensureNodeConfig(t, n, baseADNL, baseMetrics)
		// Capture key for API use
		pub := ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey).Public().(ed25519.PublicKey)
		n.KeyBase64 = base64.StdEncoding.EncodeToString(pub)
		// DB path absolute for safe cleanup
		n.DBPath = cfg.DBPath
		t.Logf("[%d] API=%s ADNL=%s Metrics=%s DB=%s Key=%s", i, n.APIAddr, cfg.NodeListenAddr, cfg.MetricsListenAddr, n.DBPath, n.KeyBase64)
		nodes = append(nodes, n)
	}

	// Wipe only DBs; configs untouched between runs
	for _, n := range nodes {
		t.Logf("[%d] wiping DB at %s", n.Idx, n.DBPath)
		wipeDir(t, n.DBPath)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start nodes
	for _, n := range nodes {
		t.Logf("[%d] starting node (api=%s, log=%s)...", n.Idx, n.APIAddr, n.LogPath)
		startNodeProcess(t, ctx, root, n)
	}
	defer func() {
		for _, n := range nodes {
			stopNodeProcess(n)
		}
	}()

	// Wait for API readiness
	for _, n := range nodes {
		t.Logf("[%d] waiting for API readiness at http://%s", n.Idx, n.APIAddr)
		waitFor(t, 2*time.Minute, 500*time.Millisecond, func() (bool, string) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := apiListChannels(ctx, n.APIAddr, "", "")
			if err != nil {
				return false, fmt.Sprintf("node %d API not ready: %v", n.Idx, err)
			}
			return true, fmt.Sprintf("node %d API ready", n.Idx)
		})
	}

	// Small grace period to let transports announce into DHT before first open
	t.Log("grace period before open to let DHT propagate (2s)")
	time.Sleep(2 * time.Second)

	// Init pair: 1 with 2 (retry until peer is discoverable)
	addr12, err := openChannelWithRetry(t, nodes[0], nodes[1], "", "")
	if err != nil {
		t.Fatalf("open 1->2 failed: %v", err)
	}
	t.Logf("channel 1<->2 initiated at %s", addr12)

	// Init pair: 3 with 2 (retry until peer is discoverable)
	addr32, err := openChannelWithRetry(t, nodes[2], nodes[1], "", "")
	if err != nil {
		t.Fatalf("open 3->2 failed: %v", err)
	}
	t.Logf("channel 3<->2 initiated at %s", addr32)

	// Verify channels visible from both sides (1<->2 and 3<->2)
	assertChannelEstablished := func(a, b *testNode) {
		// a should see b, and b should see a
		waitFor(t, 3*time.Minute, 1*time.Second, func() (bool, string) {
			ctx1, cancel1 := context.WithTimeout(context.Background(), 3*time.Second)
			alist, err := apiListChannels(ctx1, a.APIAddr, "", "")
			cancel1()
			if err != nil {
				return false, fmt.Sprintf("list on node %d failed: %v", a.Idx, err)
			}
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			blist, err := apiListChannels(ctx2, b.APIAddr, "", "")
			cancel2()
			if err != nil {
				return false, fmt.Sprintf("list on node %d failed: %v", b.Idx, err)
			}

			seenAB := channelWithTheirKey(alist, b.KeyBase64)
			seenBA := channelWithTheirKey(blist, a.KeyBase64)
			if !seenAB || !seenBA {
				return false, fmt.Sprintf("pair %d<->%d not fully established yet (a->b:%v, b->a:%v)", a.Idx, b.Idx, seenAB, seenBA)
			}
			return true, fmt.Sprintf("pair %d<->%d established", a.Idx, b.Idx)
		})
	}

	assertChannelEstablished(nodes[0], nodes[1]) // 1<->2
	assertChannelEstablished(nodes[2], nodes[1]) // 3<->2
}

// ensureNodeConfig creates config if absent; keeps existing config untouched otherwise.
// When creating, it sets unique ports and absolute DB path under node directory.
func ensureNodeConfig(t *testing.T, n *testNode, baseADNL, baseMetrics int) *cfgpkg.Config {
	t.Helper()
	absDB := filepath.Join(n.Dir, "payment-node-db")

	// If config exists, load it and only normalize DBPath to absolute (if relative) for cleanup and node usage.
	if _, err := os.Stat(n.ConfigPath); err == nil {
		cfg, err := cfgpkg.LoadConfig(n.ConfigPath)
		if err != nil {
			t.Fatalf("failed to load existing config %s: %v", n.ConfigPath, err)
		}
		// Make DBPath absolute if not already
		if !filepath.IsAbs(cfg.DBPath) {
			cfg.DBPath = absDB
			// Save only if changed; considered acceptable as it does not alter logical settings.
			if err := cfgpkg.SaveConfig(cfg, n.ConfigPath); err != nil {
				t.Fatalf("failed to update config DBPath: %v", err)
			}
		}
		return cfg
	}

	cfg, err := cfgpkg.Generate()
	if err != nil {
		t.Fatalf("failed to generate config: %v", err)
	}

	// Use localhost networking for tests; unique listen ports per node
	cfg.ExternalIP = "127.0.0.1"
	cfg.NodeListenAddr = fmt.Sprintf("127.0.0.1:%d", baseADNL+n.Idx) // ADNL
	cfg.APIListenAddr = n.APIAddr
	cfg.MetricsListenAddr = fmt.Sprintf("127.0.0.1:%d", baseMetrics+n.Idx)
	// Keep default NetworkConfigUrl; change to testnet by editing here if needed.

	// Ensure DB path absolute under node dir
	cfg.DBPath = absDB

	if err := cfgpkg.SaveConfig(cfg, n.ConfigPath); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}
	return cfg
}

func wipeDir(t *testing.T, p string) {
	t.Helper()
	if p == "" {
		t.Fatal("empty path for wipeDir")
	}
	_ = os.RemoveAll(p)
}

func startNodeProcess(t *testing.T, ctx context.Context, repoRoot string, n *testNode) {
	t.Helper()
	// Build command: go run ./cmd/node with flags
	args := []string{
		"run", "./cmd/node",
		"-config", n.ConfigPath,
		"-daemon=true",
		"-api", n.APIAddr,
	}
	if v := os.Getenv("TEST_NODE_V"); v != "" {
		args = append(args, "-v", v)
	} else {
		args = append(args, "-v", "3") // default verbosity
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = repoRoot
	// Inherit environment, but ensure Go can use module mode
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	t.Logf("[%d] exec: go %s", n.Idx, strings.Join(args, " "))

	// Stream stdout/stderr in real-time with per-node prefixes.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("[%d] failed to get StdoutPipe: %v", n.Idx, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("[%d] failed to get StderrPipe: %v", n.Idx, err)
	}

	n.outRB = newRingBuf(400)
	n.errRB = newRingBuf(400)

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start node %d: %v", n.Idx, err)
	}
	// Attach a cancel function that kills the process
	cctx, cancel := context.WithCancel(ctx)
	n.Cancel = cancel
	n.Cmd = cmd

	prefixOut := fmt.Sprintf("[%d][stdout]", n.Idx)
	prefixErr := fmt.Sprintf("[%d][stderr]", n.Idx)

	go streamPipeLines(t, prefixOut, stdout, n.outRB)
	go streamPipeLines(t, prefixErr, stderr, n.errRB)

	// Background waiter to surface early exits
	go func() {
		err := cmd.Wait()
		if ctx.Err() == nil { // not canceled by test
			if err != nil {
				fmt.Printf("[%d] process exited with error: %v\n", n.Idx, err)
			} else {
				fmt.Printf("[%d] process exited normally\n", n.Idx)
			}
			// Dump last lines
			outLines := n.outRB.last(80)
			errLines := n.errRB.last(80)
			if len(outLines) > 0 {
				fmt.Printf("[%d] last stdout lines (%d):\n%s\n", n.Idx, len(outLines), strings.Join(outLines, "\n"))
			}
			if len(errLines) > 0 {
				fmt.Printf("[%d] last stderr lines (%d):\n%s\n", n.Idx, len(errLines), strings.Join(errLines, "\n"))
			}
		}
		_ = cctx.Err()
	}()

	// On test cleanup, if test failed, dump recent logs for each node
	t.Cleanup(func() {
		if t.Failed() {
			outLines := n.outRB.last(120)
			errLines := n.errRB.last(120)
			if len(outLines) > 0 {
				fmt.Printf("[%d] recent stdout (cleanup dump):\n%s\n", n.Idx, strings.Join(outLines, "\n"))
			}
			if len(errLines) > 0 {
				fmt.Printf("[%d] recent stderr (cleanup dump):\n%s\n", n.Idx, strings.Join(errLines, "\n"))
			}
		}
	})
}

func stopNodeProcess(n *testNode) {
	if n == nil || n.Cmd == nil {
		return
	}
	if n.Cancel != nil {
		n.Cancel()
	}
	_ = n.Cmd.Process.Kill()
	_ = n.Cmd.Wait()
}

func streamPipeLines(t *testing.T, prefix string, r io.Reader, rb *ringBuf) {
	t.Helper()
	s := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	s.Buffer(buf, 1*1024*1024) // allow long lines
	for s.Scan() {
		line := s.Text()
		if rb != nil {
			rb.add(line)
		}
		// Note: t.Log output is visible with `go test -v`
		t.Logf("%s %s", prefix, line)
	}
}

func apiOpenChannel(ctx context.Context, addr, withKey, login, password string) (string, error) {
	u := fmt.Sprintf("http://%s/api/v1/channel/onchain/open", addr)
	body, _ := json.Marshal(apiOpenReq{WithNode: withKey})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if login != "" || password != "" {
		req.SetBasicAuth(login, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("open failed: status=%s body=%s", resp.Status, strings.TrimSpace(string(b)))
	}
	var r apiOpenResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.Address, nil
}

func apiListChannels(ctx context.Context, addr, login, password string) ([]apiChannel, error) {
	u := fmt.Sprintf("http://%s/api/v1/channel/onchain/list", addr)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if login != "" || password != "" {
		req.SetBasicAuth(login, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list failed: %s", strings.TrimSpace(string(b)))
	}
	var r []apiChannel
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r, nil
}

func channelWithTheirKey(list []apiChannel, keyBase64 string) bool {
	for _, ch := range list {
		if ch.Their.Key == keyBase64 {
			return true
		}
	}
	return false
}

// waitFor polls check() until it returns ok or context/timeout expires.
func waitFor(t *testing.T, timeout, interval time.Duration, check func() (bool, string)) {
	t.Helper()
	dl := time.Now().Add(timeout)
	var lastMsg string
	var lastLog time.Time
	for time.Now().Before(dl) {
		ok, msg := check()
		if msg != "" {
			lastMsg = msg
		}
		if ok {
			if lastMsg != "" {
				t.Log(lastMsg)
			}
			return
		}
		// throttle progress every ~2 seconds
		if time.Since(lastLog) > 2*time.Second {
			remaining := time.Until(dl).Truncate(time.Second)
			if lastMsg != "" {
				t.Logf("waiting... %s (left=%s)", lastMsg, remaining)
			} else {
				t.Logf("waiting... (left=%s)", remaining)
			}
			lastLog = time.Now()
		}
		time.Sleep(interval)
	}
	t.Fatalf("timeout: %s", lastMsg)
}

// Helpers
func must(err error) {
	if err != nil {
		panic(err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// Resolve repo root from this file location
	_, file, _, _ := runtime.Caller(0)
	// cmd/test-node/integration_test.go -> repo root is up 2 levels
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}


// openChannelWithRetry tries to open a channel from node a to node b, retrying with
// exponential backoff until success or overall timeout. It logs each attempt and
// returns the address on success.
func openChannelWithRetry(t *testing.T, a, b *testNode, login, password string) (string, error) {
	t.Helper()
	withKey := b.KeyBase64
	pair := fmt.Sprintf("%d->%d", a.Idx, b.Idx)

	overall := 2 * time.Minute
	deadline := time.Now().Add(overall)
	backoff := 500 * time.Millisecond
	maxBackoff := 4 * time.Second
	attempt := 0
	var lastErr error

	t.Logf("[open %s] starting retry loop (with=%s, timeout=%s)", pair, withKey, overall)

	for time.Now().Before(deadline) {
		attempt++
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		addr, err := apiOpenChannel(ctx, a.APIAddr, withKey, login, password)
		cancel()
		if err == nil {
			if attempt > 1 {
				t.Logf("[open %s] success on attempt %d", pair, attempt)
			}
			return addr, nil
		}

		lastErr = err
		msg := tail(err.Error(), 500)
		t.Logf("[open %s] attempt %d failed: %s", pair, attempt, msg)

		// sleep with exponential backoff
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	return "", fmt.Errorf("open %s failed after %d attempts (timeout %s). last error: %v", pair, attempt, overall, lastErr)
}
