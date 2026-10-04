package router

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fakeClock is a settable clock shared by a router and its test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestRouter(opts ...Option) (*Router, *fakeClock) {
	clock := &fakeClock{now: t0}
	r := New(append([]Option{WithClock(clock.Now)}, opts...)...)
	return r, clock
}

// win describes one Codex window: used percent and time until reset.
type win struct {
	used    float64
	resetIn time.Duration
	length  time.Duration
}

// codexHeaders builds the headers CLIProxyAPI forwards for a Codex response.
// Unrelated headers are included to prove they are ignored.
func codexHeaders(primary win, secondary *win) http.Header {
	h := http.Header{}
	set := func(prefix string, w win) {
		h.Set(prefix+"Used-Percent", fmt.Sprint(w.used))
		h.Set(prefix+"Reset-After-Seconds", fmt.Sprint(int(w.resetIn.Seconds())))
		if w.length > 0 {
			h.Set(prefix+"Window-Minutes", fmt.Sprint(int(w.length.Minutes())))
		}
	}
	set("X-Codex-Primary-", primary)
	if secondary != nil {
		set("X-Codex-Secondary-", *secondary)
	}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Set-Cookie", "session=fixture-cookie-value")
	return h
}

// observe records a single-window Codex snapshot remaining% / resetIn.
func observe(r *Router, id string, remainingPct float64, resetIn time.Duration) {
	r.ObserveUsage(pluginapi.UsageRecord{
		AuthID:          id,
		Provider:        "codex",
		ResponseHeaders: codexHeaders(win{used: 100 - remainingPct, resetIn: resetIn}, nil),
	})
}

func codexCandidates(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Status: "active"})
	}
	return out
}

func pickReq(cands ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5.5", Candidates: cands}
}
