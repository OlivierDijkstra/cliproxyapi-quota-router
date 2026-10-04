package quota

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Codex parses the rate-limit headers that ChatGPT Codex attaches to responses.
// CLIProxyAPI forwards them verbatim for HTTP responses and synthesizes the same
// names from websocket "codex.rate_limits" events.
//
// Only the account-wide primary and secondary windows are read. Per-model
// additional limits (X-Codex-<name>-Primary-* and X-Codex-Additional-*) are
// ignored because the header names do not identify which model they govern.
type Codex struct{}

// Provider implements Parser.
func (Codex) Provider() string { return "codex" }

// Parse implements Parser.
func (Codex) Parse(h http.Header, observedAt time.Time) (Snapshot, bool) {
	snap := Snapshot{Provider: "codex", ObservedAt: observedAt}
	found := false
	for _, name := range []string{"Primary", "Secondary"} {
		if w, ok := codexWindow(h, name, observedAt); ok {
			snap.Windows = append(snap.Windows, w)
			found = true
		}
	}
	if v, ok := lastBool(h, "X-Codex-Limit-Reached"); ok {
		found = true
		snap.LimitReached = snap.LimitReached || v
		snap.LimitObservedAt = observedAt
	}
	if v, ok := lastBool(h, "X-Codex-Allowed"); ok {
		found = true
		snap.LimitReached = snap.LimitReached || !v
		snap.LimitObservedAt = observedAt
	}
	return snap, found
}

func codexWindow(h http.Header, name string, observedAt time.Time) (Window, bool) {
	prefix := "X-Codex-" + name + "-"
	used, ok := lastFloat(h, prefix+"Used-Percent")
	if !ok || used < 0 || used > 100 {
		return Window{}, false
	}
	w := Window{Name: strings.ToLower(name), Remaining: clamp01(1 - used/100), ObservedAt: observedAt}
	if minutes, ok := lastFloat(h, prefix+"Window-Minutes"); ok && minutes > 0 {
		w.Length = time.Duration(minutes * float64(time.Minute))
	}
	if at, ok := lastFloat(h, prefix+"Reset-At"); ok && at > 0 {
		w.ResetAt = unixTime(at)
	} else if after, ok := lastFloat(h, prefix+"Reset-After-Seconds"); ok && after >= 0 {
		w.ResetAt = observedAt.Add(time.Duration(after * float64(time.Second)))
	}
	return w, true
}

// unixTime converts unix seconds, or milliseconds when the value is too large
// to be seconds, to a time.
func unixTime(at float64) time.Time {
	if at > 1e12 {
		at /= 1000
	}
	sec, frac := math.Modf(at)
	return time.Unix(int64(sec), int64(frac*1e9))
}

func lastValue(h http.Header, key string) (string, bool) {
	values := h.Values(key)
	if len(values) == 0 {
		return "", false
	}
	v := strings.TrimSpace(values[len(values)-1])
	return v, v != ""
}

func lastFloat(h http.Header, key string) (float64, bool) {
	raw, ok := lastValue(h, key)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func lastBool(h http.Header, key string) (bool, bool) {
	raw, ok := lastValue(h, key)
	if !ok {
		return false, false
	}
	b, err := strconv.ParseBool(strings.ToLower(raw))
	return b, err == nil
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
