package quota

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// claudeOK is a successful Claude OAuth response: both shared windows, the
// governing claim, and extra usage blocked by the org spend cap. Header names
// and values follow the shapes Claude Code reads and CLIProxyAPI issue #5915
// reported. Unrelated headers prove they are ignored.
func claudeOK() http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-Reset", unix(t0.Add(3*time.Hour)))
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", unix(t0.Add(3*time.Hour)))
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.69")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", unix(t0.Add(50*time.Hour)))
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "org_spend_cap_reached")
	h.Set("Request-Id", "req_fixture")
	h.Set("Set-Cookie", "session=fixture-cookie-value")
	return h
}

func window(t *testing.T, s Snapshot, name string) Window {
	t.Helper()
	for _, w := range s.Windows {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no %s window in %+v", name, s.Windows)
	return Window{}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestClaudeParsesSharedWindows(t *testing.T) {
	snap, ok := Claude{}.Parse(claudeOK(), t0)
	if !ok || len(snap.Windows) != 2 {
		t.Fatalf("ok=%v windows=%+v", ok, snap.Windows)
	}
	five, week := window(t, snap, "5h"), window(t, snap, "7d")
	if !near(five.Remaining, 0.58) || five.Length != 5*time.Hour || !five.ResetAt.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("5h = %+v", five)
	}
	if !near(week.Remaining, 0.31) || week.Length != 7*24*time.Hour || !week.ResetAt.Equal(t0.Add(50*time.Hour)) {
		t.Errorf("7d = %+v", week)
	}
	if snap.LimitReached || snap.LimitObservedAt.IsZero() {
		t.Errorf("limit = %v at %v, want a cleared flag", snap.LimitReached, snap.LimitObservedAt)
	}
	if snap.ExtraUsage || snap.ExtraUsageObservedAt.IsZero() {
		t.Errorf("extra usage = %v at %v, want reported unavailable", snap.ExtraUsage, snap.ExtraUsageObservedAt)
	}
}

// The 429 from CLIProxyAPI issue #5915: an overage-only model was rejected while
// the shared windows were fine. The account must stay usable.
func TestClaudeOverageOnlyRejectionIsNotCredentialWide(t *testing.T) {
	h := http.Header{
		"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
		"Anthropic-Ratelimit-Unified-Representative-Claim":    {"seven_day_overage_included"},
		"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":          {"0.69"},
		"Anthropic-Ratelimit-Unified-5h-Utilization":          {"0.00"},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":            {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization":       {"1.02"},
		"Anthropic-Ratelimit-Unified-Overage-Status":          {"rejected"},
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"},
		"Retry-After": {"121180"},
	}
	snap, ok := Claude{}.Parse(h, t0)
	if !ok || snap.LimitReached {
		t.Fatalf("ok=%v limitReached=%v, want a usable account", ok, snap.LimitReached)
	}
	if len(snap.Windows) != 2 || window(t, snap, "5h").Remaining != 1 || !near(window(t, snap, "7d").Remaining, 0.31) {
		t.Errorf("windows = %+v", snap.Windows)
	}
}

func TestClaudeFableOnlyRejectionIsNotCredentialWide(t *testing.T) {
	h := http.Header{
		"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed_warning"},
		"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
	}
	snap, ok := Claude{}.Parse(h, t0)
	if !ok || snap.LimitReached || len(snap.Windows) != 0 {
		t.Fatalf("ok=%v snap=%+v", ok, snap)
	}
}

func TestClaudeRejectedWindow(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.97") // rejection wins over the share
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", unix(t0.Add(2*time.Hour)))
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.40")
	snap, _ := Claude{}.Parse(h, t0)
	five := window(t, snap, "5h")
	if five.Remaining != 0 || !five.ResetAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("5h = %+v", five)
	}
	// The depleted window explains the rejection; no separate flag.
	if snap.LimitReached {
		t.Error("limit flag set alongside a rejected window")
	}
}

// A rejection whose claim names a shared window, with no per-window headers,
// still depletes that window until the unified reset.
func TestClaudeClaimOnlyRejection(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	h.Set("Anthropic-Ratelimit-Unified-Reset", unix(t0.Add(30*time.Hour)))
	snap, _ := Claude{}.Parse(h, t0)
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	week := window(t, snap, "7d")
	if week.Remaining != 0 || !week.ResetAt.Equal(t0.Add(30*time.Hour)) {
		t.Errorf("7d = %+v", week)
	}
}

func TestClaudeUnexplainedRejectionCarriesReset(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Reset", unix(t0.Add(4*time.Hour)))
	snap, ok := Claude{}.Parse(h, t0)
	if !ok || !snap.LimitReached || !snap.LimitUntil.Equal(t0.Add(4*time.Hour)) {
		t.Fatalf("ok=%v snap=%+v", ok, snap)
	}
}

func TestClaudeExtraUsage(t *testing.T) {
	for status, want := range map[string]bool{"allowed": true, "allowed_warning": true, "rejected": false} {
		snap, ok := Claude{}.Parse(http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": {status}}, t0)
		if !ok || snap.ExtraUsage != want || len(snap.Windows) != 0 {
			t.Errorf("%s: ok=%v snap=%+v", status, ok, snap)
		}
	}
}

func TestClaudeRejectsBadValues(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.3")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "2026-10-01T15:00:00Z")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "-0.2")
	snap, ok := Claude{}.Parse(h, t0)
	if !ok || len(snap.Windows) != 1 {
		t.Fatalf("ok=%v windows=%+v", ok, snap.Windows)
	}
	five := window(t, snap, "5h")
	if five.Remaining != 0 || !five.ResetAt.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("5h = %+v", five)
	}
	for _, bad := range []string{"NaN", "Inf", "abc", ""} {
		h := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {bad}}
		if _, ok := (Claude{}).Parse(h, t0); ok {
			t.Errorf("utilization %q produced a snapshot", bad)
		}
	}
}

func TestClaudeIgnoresUnrelatedHeaders(t *testing.T) {
	h := http.Header{
		"Anthropic-Ratelimit-Requests-Remaining": {"40"},
		"Retry-After":                            {"30"},
		"X-Codex-Primary-Used-Percent":           {"50"},
	}
	if snap, ok := (Claude{}).Parse(h, t0); ok {
		t.Fatalf("got %+v from non-quota headers", snap)
	}
}

// Headers cross a JSON boundary and arrive in whatever case the host used.
func TestRegistryParsesLowercaseClaudeHeaders(t *testing.T) {
	h := http.Header{
		"anthropic-ratelimit-unified-5h-utilization": {"0.25"},
		"anthropic-ratelimit-unified-5h-reset":       {unix(t0.Add(time.Hour))},
		"anthropic-ratelimit-unified-7d-utilization": {"0.5"},
	}
	snap, ok := DefaultRegistry().Parse("Claude", h, t0)
	if !ok || len(snap.Windows) != 2 || window(t, snap, "5h").Remaining != 0.75 {
		t.Fatalf("ok=%v snap=%+v", ok, snap)
	}
}

func TestMergeKeepsLimitResetAndExtraUsage(t *testing.T) {
	first := Snapshot{LimitReached: true, LimitObservedAt: t0, LimitUntil: t0.Add(time.Hour), ExtraUsage: true, ExtraUsageObservedAt: t0}
	windowsOnly := Snapshot{ObservedAt: t0.Add(time.Minute), Windows: []Window{{Name: "5h", Remaining: 0.5, ObservedAt: t0.Add(time.Minute)}}}
	got := first.Merge(windowsOnly)
	if !got.LimitReached || !got.LimitUntil.Equal(t0.Add(time.Hour)) || !got.ExtraUsage {
		t.Fatalf("merge dropped signals: %+v", got)
	}
	cleared := got.Merge(Snapshot{LimitObservedAt: t0.Add(2 * time.Minute), ExtraUsageObservedAt: t0.Add(2 * time.Minute)})
	if cleared.LimitReached || !cleared.LimitUntil.IsZero() || cleared.ExtraUsage {
		t.Fatalf("newer report did not replace signals: %+v", cleared)
	}
}
