package router

import (
	"sort"
	"testing"
	"time"

	"github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota"
)

const day = 24 * time.Hour

func single(remaining float64, resetIn time.Duration) quota.Snapshot {
	return quota.Snapshot{
		Provider:   "codex",
		ObservedAt: t0,
		Windows:    []quota.Window{{Name: "primary", Remaining: remaining, ResetAt: t0.Add(resetIn)}},
	}
}

func rank(t *testing.T, mode Mode, fixtures map[string]quota.Snapshot) []string {
	t.Helper()
	cfg := DefaultConfig()
	scores := map[string]float64{}
	names := make([]string, 0, len(fixtures))
	for name, snap := range fixtures {
		a := assess(snap, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
		if !a.BlockedUntil.IsZero() {
			t.Fatalf("%s unexpectedly blocked", name)
		}
		scores[name] = score(mode, a)
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return scores[names[i]] > scores[names[j]] })
	return names
}

var fixtures = map[string]quota.Snapshot{
	"72%/14h": single(0.72, 14*time.Hour),
	"3%/8h":   single(0.03, 8*time.Hour),
	"38%/2d":  single(0.38, 2*day),
	"91%/5d":  single(0.91, 5*day),
}

func TestSmartRanksQuotaAtRiskNotEarliestReset(t *testing.T) {
	got := rank(t, ModeSmart, fixtures)
	if got[0] != "72%/14h" {
		t.Fatalf("smart order = %v, want 72%%/14h first", got)
	}
	if got[len(got)-1] != "3%/8h" {
		t.Fatalf("smart order = %v, want 3%%/8h last: little quota is at risk", got)
	}
}

func TestExpiringFirstIsSmart(t *testing.T) {
	cfg, err := ParseConfig([]byte("mode: expiring-first"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeSmart {
		t.Fatalf("mode = %q, want smart", cfg.Mode)
	}
	// Even unvalidated, the alias must not fall back to earliest-reset order.
	got := rank(t, ModeExpiringFirst, fixtures)
	if got[0] != "72%/14h" || got[len(got)-1] != "3%/8h" {
		t.Fatalf("expiring-first order = %v, want smart order", got)
	}
}

func TestNearEmptyAccountNeverPreferred(t *testing.T) {
	// However soon it resets, a nearly empty account loses to one that is about
	// to waste most of its quota, and one below headroom is not eligible at all.
	cfg := DefaultConfig()
	big := assess(single(0.72, 14*time.Hour), t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	for _, resetIn := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 8 * time.Hour, 2 * day} {
		near := assess(single(0.03, resetIn), t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
		if score(ModeSmart, near) >= score(ModeSmart, big) {
			t.Errorf("3%% resetting in %v (%v) outranks 72%%/14h (%v)", resetIn, score(ModeSmart, near), score(ModeSmart, big))
		}
		if out := assess(single(0.019, resetIn), t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter); out.BlockedUntil.IsZero() {
			t.Errorf("1.9%% resetting in %v is not blocked", resetIn)
		}
	}
}

func TestZeroHeadroomStillBlocksEmptyWindow(t *testing.T) {
	cfg := DefaultConfig()
	empty := assess(single(0, 2*time.Hour), t0, 0, cfg.MinResetHorizon, cfg.StaleAfter)
	if !empty.BlockedUntil.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("empty window with headroom 0: blocked until %v, want reset", empty.BlockedUntil)
	}
	if a := assess(single(0, 2*time.Hour), t0.Add(2*time.Hour), 0, cfg.MinResetHorizon, cfg.StaleAfter); !a.BlockedUntil.IsZero() {
		t.Fatalf("still blocked after reset: %v", a.BlockedUntil)
	}
	thin := assess(single(0.001, 2*time.Hour), t0, 0, cfg.MinResetHorizon, cfg.StaleAfter)
	if !thin.BlockedUntil.IsZero() || thin.Urgency <= 0 {
		t.Fatalf("0.1%% left with headroom 0 should be usable: %+v", thin)
	}
}

func TestHeadroomRanksByRemaining(t *testing.T) {
	got := rank(t, ModeHeadroom, fixtures)
	if got[0] != "91%/5d" || got[3] != "3%/8h" {
		t.Fatalf("headroom order = %v", got)
	}
}

func TestMinResetHorizonDampensImminentResets(t *testing.T) {
	cfg := DefaultConfig()
	soon := assess(single(0.05, time.Minute), t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	big := assess(single(0.72, 14*time.Hour), t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if soon.Urgency >= big.Urgency {
		t.Fatalf("5%% resetting in 1m (%v) must not outrank 72%%/14h (%v)", soon.Urgency, big.Urgency)
	}
}

func TestLongerWindowLimitsUsableQuota(t *testing.T) {
	cfg := DefaultConfig()
	// Plenty in the 5h window but the weekly window is nearly spent.
	tight := quota.Snapshot{ObservedAt: t0, Windows: []quota.Window{
		{Name: "primary", Remaining: 1, ResetAt: t0.Add(2 * time.Hour)},
		{Name: "secondary", Remaining: 0.06, ResetAt: t0.Add(5 * day)},
	}}
	healthy := quota.Snapshot{ObservedAt: t0, Windows: []quota.Window{
		{Name: "primary", Remaining: 0.5, ResetAt: t0.Add(4 * time.Hour)},
		{Name: "secondary", Remaining: 0.8, ResetAt: t0.Add(5 * day)},
	}}
	a := assess(tight, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	b := assess(healthy, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if a.Remaining != 0.06 {
		t.Errorf("tightest remaining = %v, want 0.06", a.Remaining)
	}
	if a.Urgency >= b.Urgency {
		t.Fatalf("weekly-limited account (%v) must rank below healthy one (%v)", a.Urgency, b.Urgency)
	}
}

func TestBelowHeadroomBlocksUntilReset(t *testing.T) {
	cfg := DefaultConfig()
	snap := quota.Snapshot{ObservedAt: t0, Windows: []quota.Window{
		{Name: "primary", Remaining: 0.6, ResetAt: t0.Add(time.Hour)},
		{Name: "secondary", Remaining: 0.01, ResetAt: t0.Add(3 * day)},
	}}
	a := assess(snap, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if !a.BlockedUntil.Equal(t0.Add(3 * day)) {
		t.Fatalf("blocked until %v, want secondary reset", a.BlockedUntil)
	}
	later := assess(snap, t0.Add(3*day+time.Minute), cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if !later.BlockedUntil.IsZero() {
		t.Fatalf("still blocked after reset: %v", later.BlockedUntil)
	}
}

func TestResetCreditRefillsAndProjectsNextReset(t *testing.T) {
	w := []quota.Window{{Name: "primary", Remaining: 0, ResetAt: t0, Length: 5 * time.Hour}}
	got := creditResets(w, t0.Add(6*time.Hour))
	if got[0].remaining != 1 {
		t.Errorf("remaining = %v, want 1", got[0].remaining)
	}
	if want := t0.Add(10 * time.Hour); !got[0].resetAt.Equal(want) {
		t.Errorf("next reset = %v, want %v", got[0].resetAt, want)
	}
}

func TestLimitReachedWithoutDepletedWindowBlocksUntilStale(t *testing.T) {
	cfg := DefaultConfig()
	snap := single(0.5, 3*time.Hour)
	snap.LimitReached, snap.LimitObservedAt = true, t0
	a := assess(snap, t0.Add(time.Minute), cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if !a.BlockedUntil.Equal(t0.Add(cfg.StaleAfter)) {
		t.Fatalf("blocked until %v, want observed+stale_after", a.BlockedUntil)
	}
	a = assess(snap, t0.Add(cfg.StaleAfter+time.Second), cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if !a.BlockedUntil.IsZero() {
		t.Fatal("flag should expire with staleness")
	}
}

func TestWindowWithoutResetStillLimits(t *testing.T) {
	cfg := DefaultConfig()
	snap := quota.Snapshot{ObservedAt: t0, Windows: []quota.Window{{Name: "primary", Remaining: 0.4}}}
	a := assess(snap, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if a.Remaining != 0.4 || a.Urgency != 0 || !a.NextReset.IsZero() {
		t.Fatalf("assessment = %+v", a)
	}
}

func TestDepletedWindowWithoutResetUsesItsOwnObservation(t *testing.T) {
	cfg := DefaultConfig()
	// The secondary window was last reported ten minutes before the snapshot's
	// latest observation, so its block runs from when it was seen.
	seen := t0.Add(-10 * time.Minute)
	snap := quota.Snapshot{ObservedAt: t0, Windows: []quota.Window{
		{Name: "primary", Remaining: 0.5, ResetAt: t0.Add(time.Hour), ObservedAt: t0},
		{Name: "secondary", Remaining: 0, ObservedAt: seen},
	}}
	a := assess(snap, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter)
	if want := seen.Add(cfg.StaleAfter); !a.BlockedUntil.Equal(want) {
		t.Fatalf("blocked until %v, want %v", a.BlockedUntil, want)
	}
}

func TestStaleIsJudgedPerWindow(t *testing.T) {
	cfg := DefaultConfig()
	snap := quota.Snapshot{ObservedAt: t0, Windows: []quota.Window{
		{Name: "primary", Remaining: 0.5, ResetAt: t0.Add(time.Hour), ObservedAt: t0},
		{Name: "secondary", Remaining: 0.5, ResetAt: t0.Add(3 * day), ObservedAt: t0.Add(-cfg.StaleAfter - time.Second)},
	}}
	if a := assess(snap, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter); !a.Stale {
		t.Fatal("old secondary window should make the snapshot stale")
	}
	snap.Windows[1].ObservedAt = t0
	if a := assess(snap, t0, cfg.MinimumHeadroom, cfg.MinResetHorizon, cfg.StaleAfter); a.Stale {
		t.Fatal("fresh windows reported stale")
	}
}
