//go:build quotarouter_smoke

// This file is not part of the plugin module: the go tool skips testdata
// directories. scripts/host-smoke.sh copies it into internal/pluginhost of a
// CLIProxyAPI checkout, so the host's own loader opens the built library
// (dlopen, cliproxy_plugin_init, the C call table and shutdown) instead of a
// test double.
package pluginhost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

const quotaRouterID = "cliproxyapi-quota-router"

// quotaRouterConfig parses a host config enabling the plugin with settings.
// The settings map is merged over the defaults and marshalled as one mapping,
// so overriding a key cannot produce a duplicate YAML key.
func quotaRouterConfig(t *testing.T, dir string, settings map[string]any) *config.Config {
	t.Helper()
	plugin := map[string]any{
		"enabled":          true,
		"priority":         10,
		"log_decisions":    true,
		"refresh_interval": "10s",
	}
	for k, v := range settings {
		plugin[k] = v
	}
	raw, err := yaml.Marshal(map[string]any{"plugins": map[string]any{
		"enabled": true,
		"dir":     dir,
		"configs": map[string]any{quotaRouterID: plugin},
	}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, raw)
	}
	return &cfg
}

// smokeAuthManager returns a host auth manager holding Codex credentials with
// the given IDs, so host.auth.list answers with the same IDs the test feeds
// usage for and a refresh keeps their quota snapshots. claude maps further
// auth IDs to the API key the fake Anthropic upstream at claudeURL expects.
func smokeAuthManager(t *testing.T, claudeURL string, claude map[string]string, ids ...string) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(&config.Config{}))
	register := func(auth *coreauth.Auth) {
		auth.EnsureIndex()
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}
	for _, id := range ids {
		register(&coreauth.Auth{
			ID:         id,
			Provider:   "codex",
			FileName:   id + ".json",
			Status:     coreauth.StatusActive,
			Metadata:   map[string]any{"type": "codex"},
			Attributes: map[string]string{"path": "/nonexistent/" + id + ".json"},
		})
	}
	reg := registry.GetGlobalRegistry()
	for id, key := range claude {
		// host.auth.list only reports auths backed by a file, as Claude OAuth
		// logins are, so give these a path like one.
		register(&coreauth.Auth{
			ID:         id,
			Provider:   "claude",
			FileName:   id + ".json",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"api_key": key, "base_url": claudeURL, "path": "/nonexistent/" + id + ".json"},
		})
		reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: claudeSmokeModel}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}
	return manager
}

const claudeSmokeModel = "claude-sonnet-5-5"

// fakeAnthropic answers /v1/messages with the unified rate-limit headers of a
// Claude subscription account, chosen by the API key the executor sends, and
// counts the requests each key received.
type fakeAnthropic struct {
	mu     sync.Mutex
	hits   map[string]int
	limits map[string][2]float64 // key -> 5h and 7d utilization
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The executor sends x-api-key only to api.anthropic.com and a bearer token
	// to any other base URL.
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	f.mu.Lock()
	f.hits[key]++
	util, ok := f.limits[key]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unknown key", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", strconv.FormatFloat(util[0], 'f', 2, 64))
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", strconv.FormatFloat(util[1], 'f', 2, 64))
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	_, _ = w.Write([]byte(`{"id":"msg_smoke","type":"message","role":"assistant","model":"` + claudeSmokeModel + `","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
}

func (f *fakeAnthropic) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

type smokeCredential struct {
	Auth     string `json:"auth"`
	Provider string `json:"provider"`
	Windows  []struct {
		Name      string  `json:"name"`
		Remaining float64 `json:"remaining"`
	} `json:"windows"`
}

// smokeStatus reads the plugin's status route through the host.
func smokeStatus(t *testing.T, h *Host, into any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/quota-router/status", nil)
	if !h.ServeManagementHTTP(rec, req) {
		t.Fatal("status route not registered")
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode status: %v: %s", err, rec.Body.String())
	}
}

// awaitClaudeWindows waits until the usage pipeline has delivered quota for id.
func awaitClaudeWindows(t *testing.T, h *Host, id string) smokeCredential {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var status struct {
			Credentials []smokeCredential `json:"credentials"`
		}
		smokeStatus(t, h, &status)
		for _, c := range status.Credentials {
			if c.Auth == id && len(c.Windows) == 2 {
				return c
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no Claude quota windows for %s reached the plugin", id)
	return smokeCredential{}
}

type smokeStoreStats struct {
	HostAuths        int       `json:"host_auths"`
	LastHostSync     time.Time `json:"last_host_sync"`
	LastRefreshError string    `json:"last_refresh_error"`
}

// awaitHostSync polls the plugin's status route through the host until its
// refresher has applied a host.auth.list answer newer than after.
func awaitHostSync(t *testing.T, ctx context.Context, h *Host, after time.Time, wantAuths int) time.Time {
	t.Helper()
	h.RegisterManagementRoutes(ctx, nil)
	deadline := time.Now().Add(20 * time.Second)
	var last smokeStoreStats
	for time.Now().Before(deadline) {
		var status struct {
			Store smokeStoreStats `json:"store"`
		}
		smokeStatus(t, h, &status)
		last = status.Store
		if last.LastHostSync.After(after) && last.HostAuths == wantAuths {
			return last.LastHostSync
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("refresher never synced host auths: %+v", last)
	return time.Time{}
}

func quotaHeaders(usedPercent string, resetSeconds string) map[string][]string {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", usedPercent)
	h.Set("X-Codex-Primary-Reset-After-Seconds", resetSeconds)
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("Set-Cookie", "must-be-ignored")
	return h
}

func quotaCandidates(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Status: "active"})
	}
	return out
}

func TestQuotaRouterLoaderSmoke(t *testing.T) {
	lib := os.Getenv("QUOTA_ROUTER_LIB")
	if lib == "" {
		t.Skip("QUOTA_ROUTER_LIB is not set")
	}
	dir := t.TempDir()
	src, err := os.Open(lib)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create(filepath.Join(dir, quotaRouterID+filepath.Ext(lib)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	upstream := &fakeAnthropic{hits: map[string]int{}, limits: map[string][2]float64{
		"key-busy": {0.90, 0.20}, // 10% of the 5h window left
		"key-idle": {0.10, 0.30}, // 90% left, resets just as soon
	}}
	anthropic := httptest.NewServer(upstream)
	defer anthropic.Close()
	manager := smokeAuthManager(t, anthropic.URL, map[string]string{"claude-busy": "key-busy", "claude-idle": "key-idle"}, "a", "b", "c")

	h := New()
	h.SetAuthManager(manager)
	h.ApplyConfig(ctx, quotaRouterConfig(t, dir, map[string]any{"reveal_auth_ids": true, "session_affinity": false}))
	if !h.PluginRegistered(quotaRouterID) {
		t.Fatal("plugin did not register through the host loader")
	}
	if !h.HasScheduler() {
		t.Fatal("scheduler capability not active")
	}
	var meta pluginapi.Metadata
	for _, info := range h.RegisteredPlugins() {
		if info.ID == quotaRouterID {
			meta = info.Metadata
		}
	}
	if meta.Name != quotaRouterID || meta.Version == "" || len(meta.ConfigFields) == 0 {
		t.Fatalf("metadata = %+v", meta)
	}

	// The refresher's first host.auth.list runs right after registration. Wait
	// for it so it cannot race the usage records below; the matching IDs keep
	// their snapshots on every later refresh as well.
	synced := awaitHostSync(t, ctx, h, time.Time{}, 5)

	usage := h.currentUsagePlugin(quotaRouterID)
	if usage == nil {
		t.Fatal("usage capability not active")
	}
	usage.HandleUsage(ctx, pluginapi.UsageRecord{AuthID: "a", Provider: "codex", ResponseHeaders: quotaHeaders("28", "50400")}) // 72% left, 14h
	usage.HandleUsage(ctx, pluginapi.UsageRecord{AuthID: "b", Provider: "codex", ResponseHeaders: quotaHeaders("97", "28800")}) // 3% left, 8h
	usage.HandleUsage(ctx, pluginapi.UsageRecord{AuthID: "c", Provider: "codex", ResponseHeaders: quotaHeaders("100", "3600")}) // empty

	pick := func(ids ...string) pluginapi.SchedulerPickResponse {
		t.Helper()
		resp, handled, err := h.PickAuth(ctx, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5.5", Candidates: quotaCandidates(ids...)})
		if err != nil || !handled {
			t.Fatalf("pick %v: handled=%v err=%v", ids, handled, err)
		}
		return resp
	}
	if resp := pick("b", "a", "c"); resp.AuthID != "a" {
		t.Fatalf("pick = %+v, want a (most quota at risk)", resp)
	}
	if resp := pick("c"); !resp.Reject || resp.RejectCode != "quota_exhausted" {
		t.Fatalf("pick = %+v, want quota_exhausted rejection", resp)
	}

	// Claude end to end: the host's auth manager asks the plugin for a
	// credential, the host's Claude executor calls the fake Anthropic upstream,
	// and the host's usage pipeline hands the response headers back to the
	// plugin. Nothing here feeds the plugin directly.
	h.RegisterUsagePlugins()
	manager.SetPluginScheduler(h)
	execute := func() {
		t.Helper()
		_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{
			Model:   claudeSmokeModel,
			Payload: []byte(`{"model":"` + claudeSmokeModel + `","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		if err != nil {
			t.Fatalf("claude execute: %v", err)
		}
	}
	// Neither account has quota data, so the first two requests probe one each.
	execute()
	busy := awaitClaudeWindows(t, h, "claude-busy")
	execute()
	idle := awaitClaudeWindows(t, h, "claude-idle")
	if upstream.count("key-busy") != 1 || upstream.count("key-idle") != 1 {
		t.Fatalf("probe hits = busy %d, idle %d; want one each", upstream.count("key-busy"), upstream.count("key-idle"))
	}
	for _, c := range []smokeCredential{busy, idle} {
		if c.Provider != "claude" || c.Windows[0].Name != "5h" || c.Windows[1].Name != "7d" {
			t.Fatalf("credential = %+v", c)
		}
	}
	if r := busy.Windows[0].Remaining; r < 0.099 || r > 0.101 {
		t.Fatalf("busy 5h remaining = %v, want 0.10", r)
	}
	// Ranked: both 5h windows reset in two hours, and the idle account has far
	// more quota that would expire unused.
	for range 3 {
		execute()
	}
	if got := upstream.count("key-idle"); got != 4 {
		t.Fatalf("idle hits = %d (busy %d), want all ranked traffic on the idle account", got, upstream.count("key-busy"))
	}

	// Reconfigure restarts the refresher, which calls host.auth.list.
	h.ApplyConfig(ctx, quotaRouterConfig(t, dir, map[string]any{"refresh_interval": "15s", "mode": "headroom"}))
	awaitHostSync(t, ctx, h, synced, 5)
	if resp := pick("b", "a", "c"); resp.AuthID != "a" {
		t.Fatalf("pick after reconfigure = %+v", resp)
	}

	// Shutdown must wait for in-flight host callbacks before the host frees
	// its API table; a crash here aborts the test binary.
	h.ShutdownAll()
	if h.HasScheduler() {
		t.Fatal("scheduler still active after shutdown")
	}
	time.Sleep(200 * time.Millisecond)

	// Linux maps Go c-shared libraries with -z nodelete, so the host's dlclose
	// leaves the library loaded and a second load initializes it again.
	if runtime.GOOS == "linux" {
		h2 := New()
		h2.ApplyConfig(ctx, quotaRouterConfig(t, dir, nil))
		if !h2.HasScheduler() {
			t.Fatal("reload after shutdown did not register")
		}
		resp, handled, err := h2.PickAuth(ctx, pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: quotaCandidates("a")})
		if err != nil || !handled || resp.AuthID != "a" {
			t.Fatalf("pick after reload: %+v handled=%v err=%v", resp, handled, err)
		}
		h2.ShutdownAll()
	}
}
