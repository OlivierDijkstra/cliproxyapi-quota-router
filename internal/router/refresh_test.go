package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeLister struct {
	mu      sync.Mutex
	entries []pluginapi.HostAuthFileEntry
	err     error
	calls   int
}

func (f *fakeLister) ListAuths(context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.entries, f.err
}

func (f *fakeLister) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestRefreshAppliesHostStateAndPrunes(t *testing.T) {
	lister := &fakeLister{entries: []pluginapi.HostAuthFileEntry{
		{ID: "a", Status: "active"},
		{ID: "b", Disabled: true},
		{ID: "c", NextRetryAfter: t0.Add(time.Hour)},
	}}
	lister.entries = append(lister.entries, pluginapi.HostAuthFileEntry{ID: "gone"})
	r, _ := newTestRouter(WithAuthLister(lister))
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	observe(r, "a", 50, time.Hour)
	observe(r, "gone", 50, time.Hour)
	observe(r, "config-key", 50, time.Hour) // never listed, like a claude-api-key entry
	lister.entries = lister.entries[:3]
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Store().Snapshot("config-key"); !ok {
		t.Error("snapshot of an auth the host never lists dropped")
	}
	if _, ok := r.Store().Snapshot("gone"); ok {
		t.Error("snapshot of removed auth kept")
	}
	if _, ok := r.Store().Snapshot("a"); !ok {
		t.Error("snapshot of live auth dropped")
	}
	if hs, _ := r.Store().HostState("b", t0, time.Hour); !hs.Disabled {
		t.Error("b should be disabled")
	}
	if hs, _ := r.Store().HostState("c", t0, time.Hour); !hs.NextRetryAfter.Equal(t0.Add(time.Hour)) {
		t.Error("c retry time lost")
	}
	if st := r.Store().Stats(); st.HostAuths != 3 || st.LastRefreshError != "" {
		t.Errorf("stats = %+v", st)
	}
}

func TestRefreshFailureKeepsState(t *testing.T) {
	lister := &fakeLister{entries: []pluginapi.HostAuthFileEntry{{ID: "a"}}}
	r, _ := newTestRouter(WithAuthLister(lister))
	observe(r, "a", 50, time.Hour)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	lister.err = errors.New("host busy")
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if _, ok := r.Store().Snapshot("a"); !ok {
		t.Error("failure dropped snapshots")
	}
	if st := r.Store().Stats(); st.LastRefreshError != "host busy" || st.HostAuths != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRefreshRejectsListingWithoutIDs(t *testing.T) {
	// The host falls back to reading auth files from disk without IDs.
	lister := &fakeLister{entries: []pluginapi.HostAuthFileEntry{{Name: "codex-user.json"}}}
	r, _ := newTestRouter(WithAuthLister(lister))
	observe(r, "a", 50, time.Hour)
	if err := r.Refresh(context.Background()); !errors.Is(err, errNoAuthIDs) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := r.Store().Snapshot("a"); !ok {
		t.Error("ID-less listing pruned snapshots")
	}
}

func TestRefreshPrunesExpiredSessions(t *testing.T) {
	r, clock := newTestRouter()
	r.aff.bind("k", "a", clock.Now(), time.Minute)
	clock.Advance(2 * time.Minute)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.aff.size() != 0 {
		t.Fatal("expired binding kept")
	}
}

func TestRefresherRunsPeriodicallyAndStops(t *testing.T) {
	lister := &fakeLister{entries: []pluginapi.HostAuthFileEntry{{ID: "a"}}}
	r := New(WithAuthLister(lister))
	cfg := DefaultConfig()
	cfg.RefreshInterval = 10 * time.Second
	r.SetConfig(cfg)
	// Shorten the ticker below the config floor for the test.
	r.mu.Lock()
	r.cfg.RefreshInterval = 20 * time.Millisecond
	r.mu.Unlock()

	r.StartRefresher()
	r.StartRefresher() // idempotent
	deadline := time.Now().Add(2 * time.Second)
	for lister.Calls() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.StopRefresher()
	calls := lister.Calls()
	if calls < 3 {
		t.Fatalf("refresh ran %d times", calls)
	}
	time.Sleep(60 * time.Millisecond)
	if lister.Calls() != calls {
		t.Fatal("refresher kept running after stop")
	}
	r.StopRefresher() // safe when stopped
}

func TestConfigChangeRestartsRunningRefresher(t *testing.T) {
	lister := &fakeLister{}
	r := New(WithAuthLister(lister))
	r.StartRefresher()
	defer r.StopRefresher()
	cfg := DefaultConfig()
	cfg.RefreshInterval = time.Minute
	r.SetConfig(cfg)
	if !r.refreshing() {
		t.Fatal("refresher not running after reconfigure")
	}
}
