package router

import (
	"math"
	"sort"
	"time"

	"github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota"
)

// assessment is the routing view of one snapshot at a point in time.
type assessment struct {
	// Remaining is the tightest window's unused share after reset credit.
	Remaining float64
	// BlockedUntil is set while a window is below headroom or the provider
	// reported the limit as reached.
	BlockedUntil time.Time
	// NextReset is the earliest future reset among windows with usable quota.
	NextReset time.Time
	// Urgency is usable quota per hour that would expire unused (smart mode).
	Urgency float64
	// Stale is set when any window was last reported more than stale_after ago.
	// Each window keeps its own observation time, so a response that refreshes
	// only the 5-hour window or only a limit flag does not make an old weekly
	// window fresh again.
	Stale bool
}

// window is a quota.Window after reset credit was applied for now.
type window struct {
	remaining  float64
	resetAt    time.Time
	observedAt time.Time
}

// depleted reports whether a window with this remaining share may not be used.
// An empty window blocks even when minimum_headroom is 0.
func depleted(remaining, headroom float64) bool {
	return remaining <= 0 || remaining < headroom
}

// creditResets returns windows as they stand at now. A window whose reset time
// has passed has refilled: it is credited back to full and, when its length is
// known, its next reset is projected forward. Its fullness is known as of the
// latest reset, so that is when it counts as observed; usage after that is
// unknown and ages out like any other report.
func creditResets(windows []quota.Window, now time.Time) []window {
	out := make([]window, 0, len(windows))
	for _, w := range windows {
		cw := window{remaining: w.Remaining, resetAt: w.ResetAt, observedAt: w.ObservedAt}
		if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
			cw.remaining = 1
			cw.resetAt = time.Time{}
			cw.observedAt = w.ResetAt
			if w.Length > 0 {
				periods := now.Sub(w.ResetAt)/w.Length + 1
				cw.resetAt = w.ResetAt.Add(periods * w.Length)
				cw.observedAt = cw.resetAt.Add(-w.Length)
			}
		}
		out = append(out, cw)
	}
	return out
}

// assess evaluates a snapshot. staleAfter bounds how long a depleted window
// without a reset time, or a LimitReached flag without a matching depleted
// window, keeps the credential blocked, and marks windows too old to rank on.
// A depleted window with a future reset keeps blocking however old it is.
func assess(snap quota.Snapshot, now time.Time, headroom float64, horizon, staleAfter time.Duration) assessment {
	observed := func(at time.Time) time.Time {
		if at.IsZero() {
			return snap.ObservedAt
		}
		return at
	}
	windows := creditResets(snap.Windows, now)
	a := assessment{Remaining: 1}
	for _, w := range windows {
		a.Remaining = math.Min(a.Remaining, w.remaining)
		if now.Sub(observed(w.observedAt)) > staleAfter {
			a.Stale = true
		}
		if depleted(w.remaining, headroom) {
			until := w.resetAt
			if until.IsZero() {
				until = observed(w.observedAt).Add(staleAfter)
			}
			if until.After(a.BlockedUntil) {
				a.BlockedUntil = until
			}
		}
	}
	if snap.LimitReached {
		// When a depleted window explains the flag, that window's reset already
		// governs the block above. Otherwise hold the block until the reset the
		// provider gave with the flag, or until the flag is stale.
		explained := false
		for _, w := range snap.Windows {
			explained = explained || depleted(w.Remaining, headroom)
		}
		if !explained {
			until := snap.LimitUntil
			if until.IsZero() {
				until = observed(snap.LimitObservedAt).Add(staleAfter)
			}
			if until.After(a.BlockedUntil) {
				a.BlockedUntil = until
			}
		}
	}
	if !a.BlockedUntil.After(now) {
		a.BlockedUntil = time.Time{}
	}

	// A window's quota can only be spent while every longer-lived window still
	// has room, so its usable share is capped by the windows that reset later.
	// Remaining fractions of different windows are compared directly; the host
	// does not report how many requests each window holds.
	order := make([]int, 0, len(windows))
	for i, w := range windows {
		if !w.resetAt.IsZero() {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(i, j int) bool { return windows[order[i]].resetAt.Before(windows[order[j]].resetAt) })
	for idx, i := range order {
		usable := windows[i].remaining
		for _, j := range order[idx+1:] {
			usable = math.Min(usable, windows[j].remaining)
		}
		for k, w := range windows {
			if w.resetAt.IsZero() && k != i {
				usable = math.Min(usable, w.remaining)
			}
		}
		if depleted(usable, headroom) {
			continue
		}
		if a.NextReset.IsZero() {
			a.NextReset = windows[i].resetAt
		}
		hours := math.Max(windows[i].resetAt.Sub(now).Hours(), horizon.Hours())
		a.Urgency = math.Max(a.Urgency, (usable-headroom)/hours)
	}
	return a
}

// score ranks an eligible assessment under mode; higher is better.
// ModeExpiringFirst never reaches here: config validation maps it to ModeSmart.
func score(mode Mode, a assessment) float64 {
	if mode == ModeHeadroom {
		return a.Remaining
	}
	return a.Urgency
}
