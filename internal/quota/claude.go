package quota

import (
	"net/http"
	"strings"
	"time"
)

// Claude parses the unified rate-limit headers Anthropic attaches to Claude
// subscription (OAuth) responses. CLIProxyAPI's Claude executor records the
// upstream headers of every attempt, successful or not, and forwards them to
// usage plugins unchanged.
//
// Header families, as read by Claude Code and CLIProxyAPI:
//
//	anthropic-ratelimit-unified-{5h,7d}-utilization  used share, 0..1 (can exceed 1)
//	anthropic-ratelimit-unified-{5h,7d}-reset        unix seconds
//	anthropic-ratelimit-unified-{5h,7d}-status       allowed | allowed_warning | rejected
//	anthropic-ratelimit-unified-status               status of the claim that governed this request
//	anthropic-ratelimit-unified-representative-claim five_hour | seven_day | seven_day_overage_included | overage
//	anthropic-ratelimit-unified-reset                reset of that claim
//	anthropic-ratelimit-unified-overage-status       extra usage availability
//
// The 5-hour and 7-day windows are shared by every model on the account and
// become windows here. The 7d_oi ("seven_day_overage_included") bucket and the
// overage claim are per-model or paid overflow: a rejection on them leaves the
// account usable for other models, and the host already cools the rejected
// model down per credential, so they never block the whole credential.
type Claude struct{}

type claudeWindowSpec struct {
	abbr   string // header infix
	claim  string // representative-claim value naming this window
	length time.Duration
}

var claudeWindows = []claudeWindowSpec{
	{abbr: "5h", claim: "five_hour", length: 5 * time.Hour},
	{abbr: "7d", claim: "seven_day", length: 7 * 24 * time.Hour},
}

const claudePrefix = "Anthropic-Ratelimit-Unified-"

// Provider implements Parser.
func (Claude) Provider() string { return "claude" }

// Parse implements Parser.
func (Claude) Parse(h http.Header, observedAt time.Time) (Snapshot, bool) {
	snap := Snapshot{Provider: "claude", ObservedAt: observedAt}
	found := false
	unified := claudeStatus(h, "Status")
	claim := claudeStatus(h, "Representative-Claim")

	windowRejected := false
	sharedHealthy := true
	for _, spec := range claudeWindows {
		status := claudeStatus(h, spec.abbr+"-Status")
		rejected := status == "rejected" || (unified == "rejected" && claim == spec.claim)
		util, hasUtil := lastFloat(h, claudePrefix+spec.abbr+"-Utilization")
		hasUtil = hasUtil && util >= 0
		if !(status == "allowed" || status == "allowed_warning" || (status == "" && hasUtil && util < 1)) {
			sharedHealthy = false
		}
		if !hasUtil && !rejected {
			continue
		}
		w := Window{Name: spec.abbr, Remaining: clamp01(1 - util), Length: spec.length, ObservedAt: observedAt}
		w.ResetAt = claudeTime(h, spec.abbr+"-Reset")
		if rejected {
			w.Remaining = 0
			windowRejected = true
			if w.ResetAt.IsZero() && claim == spec.claim {
				w.ResetAt = claudeTime(h, "Reset")
			}
		}
		snap.Windows = append(snap.Windows, w)
		found = true
	}

	if unified != "" {
		found = true
		snap.LimitObservedAt = observedAt
		// Same rule as CLIProxyAPI's ClaudeHeadersIndicateUnifiedRateLimitRejection:
		// a rejection on the per-model or overage bucket only counts against the
		// credential when the shared windows are not known to be healthy.
		modelScoped := sharedHealthy && (claim == "seven_day_overage_included" ||
			strings.Contains(claim, "overage") ||
			claudeStatus(h, "7d_oi-Status") == "rejected" ||
			claudeStatus(h, "Overage-Status") == "rejected" ||
			claudeStatus(h, "Overage-Disabled-Reason") != "")
		if unified == "rejected" && !windowRejected && !modelScoped {
			snap.LimitReached = true
			snap.LimitUntil = claudeTime(h, "Reset")
		}
	}

	if overage := claudeStatus(h, "Overage-Status"); overage != "" {
		found = true
		snap.ExtraUsage = overage == "allowed" || overage == "allowed_warning"
		snap.ExtraUsageObservedAt = observedAt
	}
	return snap, found
}

func claudeStatus(h http.Header, suffix string) string {
	v, _ := lastValue(h, claudePrefix+suffix)
	return strings.ToLower(v)
}

// claudeTime parses a reset header, or returns zero. Anthropic sends unix
// seconds; RFC 3339 is accepted as CLIProxyAPI does.
func claudeTime(h http.Header, suffix string) time.Time {
	if sec, ok := lastFloat(h, claudePrefix+suffix); ok {
		if sec <= 0 {
			return time.Time{}
		}
		return unixTime(sec)
	}
	raw, ok := lastValue(h, claudePrefix+suffix)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
