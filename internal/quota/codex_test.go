package quota

import (
	"net/http"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestCodexParsesBothWindows(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "28")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	h.Set("X-Codex-Secondary-Used-Percent", "62.5")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Reset-At", "1759924800") // 2025-10-08T12:00:00Z

	snap, ok := Codex{}.Parse(h, t0)
	if !ok {
		t.Fatal("expected a snapshot")
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(snap.Windows))
	}
	p, s := snap.Windows[0], snap.Windows[1]
	if p.Name != "primary" || p.Remaining != 0.72 || p.Length != 5*time.Hour || !p.ResetAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("primary = %+v", p)
	}
	if s.Name != "secondary" || s.Remaining != 0.375 || s.Length != 7*24*time.Hour {
		t.Errorf("secondary = %+v", s)
	}
	if want := time.Unix(1759924800, 0); !s.ResetAt.Equal(want) {
		t.Errorf("secondary reset = %v, want %v", s.ResetAt, want)
	}
	if snap.LimitReached {
		t.Error("limit should not be reached")
	}
}

func TestCodexResetAtWinsAndAcceptsMilliseconds(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "10")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "60")
	h.Set("X-Codex-Primary-Reset-At", "1759924800000")
	snap, _ := Codex{}.Parse(h, t0)
	if want := time.Unix(1759924800, 0); !snap.Windows[0].ResetAt.Equal(want) {
		t.Errorf("reset = %v, want %v", snap.Windows[0].ResetAt, want)
	}
}

func TestCodexLimitFlags(t *testing.T) {
	for name, h := range map[string]http.Header{
		"limit reached": {"X-Codex-Limit-Reached": {"true"}},
		"not allowed":   {"X-Codex-Allowed": {"False"}},
	} {
		snap, ok := Codex{}.Parse(h, t0)
		if !ok || !snap.LimitReached {
			t.Errorf("%s: ok=%v limitReached=%v", name, ok, snap.LimitReached)
		}
	}
	snap, ok := Codex{}.Parse(http.Header{"X-Codex-Allowed": {"true"}}, t0)
	if !ok || snap.LimitReached {
		t.Errorf("allowed=true: ok=%v limitReached=%v", ok, snap.LimitReached)
	}
}

func TestCodexIgnoresInvalidAndUnrelatedHeaders(t *testing.T) {
	h := http.Header{
		"X-Codex-Primary-Used-Percent":           {"140"},
		"X-Codex-Secondary-Used-Percent":         {"NaN"},
		"X-Codex-Bengalfox-Primary-Used-Percent": {"99"},
		"X-Codex-Additional-Spark-Limit-Reached": {"true"},
		"Authorization":                          {"Bearer not-a-real-token"},
	}
	if snap, ok := (Codex{}).Parse(h, t0); ok {
		t.Fatalf("expected no snapshot, got %+v", snap)
	}
}

func TestCodexUsesLastHeaderValue(t *testing.T) {
	h := http.Header{"X-Codex-Primary-Used-Percent": {"10", "40"}}
	snap, _ := Codex{}.Parse(h, t0)
	if got := snap.Windows[0].Remaining; got != 0.6 {
		t.Errorf("remaining = %v, want 0.6", got)
	}
}

func TestRegistryCanonicalizesKeys(t *testing.T) {
	r := DefaultRegistry()
	h := http.Header{"x-codex-primary-used-percent": {"50"}}
	snap, ok := r.Parse("Codex", h, t0)
	if !ok || snap.Windows[0].Remaining != 0.5 {
		t.Fatalf("ok=%v snap=%+v", ok, snap)
	}
	if _, ok := r.Parse("claude", h, t0); ok {
		t.Error("claude has no parser and must not produce a snapshot")
	}
	if !r.Supports("codex") || r.Supports("gemini") {
		t.Error("unexpected Supports result")
	}
}

func TestCodexStampsObservationTimes(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "10")
	snap, _ := Codex{}.Parse(h, t0)
	if !snap.Windows[0].ObservedAt.Equal(t0) || !snap.LimitObservedAt.IsZero() {
		t.Fatalf("snap = %+v", snap)
	}
	h.Set("X-Codex-Allowed", "true")
	if snap, _ = (Codex{}).Parse(h, t0); !snap.LimitObservedAt.Equal(t0) || snap.LimitReached {
		t.Fatalf("snap = %+v", snap)
	}
}

func TestMergeKeepsSignalsMissingFromPartialReports(t *testing.T) {
	t1 := t0.Add(time.Minute)
	prev := Snapshot{Provider: "codex", ObservedAt: t0, LimitReached: true, LimitObservedAt: t0, Windows: []Window{
		{Name: "primary", Remaining: 0.5, ObservedAt: t0},
		{Name: "secondary", Remaining: 0, ObservedAt: t0},
	}}
	got := prev.Merge(Snapshot{Provider: "codex", ObservedAt: t1, Windows: []Window{{Name: "primary", Remaining: 0.4, ObservedAt: t1}}})
	if len(got.Windows) != 2 || got.Windows[0].Remaining != 0.4 || got.Windows[1].Remaining != 0 {
		t.Fatalf("windows = %+v", got.Windows)
	}
	if !got.LimitReached || !got.LimitObservedAt.Equal(t0) || !got.ObservedAt.Equal(t1) {
		t.Fatalf("flag or time lost: %+v", got)
	}
	got = got.Merge(Snapshot{ObservedAt: t1, LimitObservedAt: t1})
	if got.LimitReached || len(got.Windows) != 2 {
		t.Fatalf("newer flag not applied: %+v", got)
	}
}

func TestMergeIgnoresOlderReports(t *testing.T) {
	t1 := t0.Add(time.Minute)
	prev := Snapshot{ObservedAt: t1, LimitObservedAt: t1, Windows: []Window{{Name: "primary", Remaining: 0.2, ObservedAt: t1}}}
	got := prev.Merge(Snapshot{ObservedAt: t0, LimitReached: true, LimitObservedAt: t0, Windows: []Window{
		{Name: "primary", Remaining: 0.9, ObservedAt: t0},
		{Name: "secondary", Remaining: 0.7, ObservedAt: t0},
	}})
	if got.Windows[0].Remaining != 0.2 || got.LimitReached || !got.ObservedAt.Equal(t1) {
		t.Fatalf("older report overwrote newer data: %+v", got)
	}
	// A window the newer report did not carry is still worth knowing.
	if len(got.Windows) != 2 || got.Windows[1].Name != "secondary" {
		t.Fatalf("windows = %+v", got.Windows)
	}
}
