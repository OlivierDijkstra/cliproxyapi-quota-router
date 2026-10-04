// Package quota turns provider rate-limit signals into a provider-neutral snapshot.
//
// The host forwards upstream response headers to usage plugins. Each provider
// parser extracts only the quota headers it understands; every other header is
// discarded and never stored.
package quota

import (
	"net/http"
	"strings"
	"time"
)

// Window is one rolling quota window, such as Codex's 5-hour or weekly limit.
type Window struct {
	// Name is a short label for diagnostics ("primary", "secondary").
	Name string
	// Remaining is the unused share of the window in [0, 1].
	Remaining float64
	// Length is the window duration. Zero when the provider did not report it.
	Length time.Duration
	// ResetAt is when the window refills. Zero when unknown.
	ResetAt time.Time
	// ObservedAt is when this window was last reported.
	ObservedAt time.Time
}

// Snapshot is the quota state of one credential. ObservedAt is the latest
// observation that contributed to it.
type Snapshot struct {
	Provider   string
	ObservedAt time.Time
	Windows    []Window
	// LimitReached reports that the provider refused or flagged the credential as
	// out of quota, even if no window shows it.
	LimitReached bool
	// LimitObservedAt is when LimitReached was last reported. Zero when the
	// observation carried no limit flag.
	LimitObservedAt time.Time
	// LimitUntil is when the provider said a LimitReached block lifts. Zero
	// when it gave no time.
	LimitUntil time.Time
	// ExtraUsage reports that the provider serves the credential from paid
	// overflow (Anthropic "extra usage") once its windows are exhausted.
	ExtraUsage bool
	// ExtraUsageObservedAt is when ExtraUsage was last reported.
	ExtraUsageObservedAt time.Time
}

// Merge folds next into s and returns the result. A response can carry only
// some of a credential's quota headers, so each window and the limit flag are
// replaced only by a newer report of the same signal. A window missing from next
// keeps its last known value: a depleted weekly window must keep blocking even
// when a response only mentions the 5-hour window.
func (s Snapshot) Merge(next Snapshot) Snapshot {
	out := Snapshot{
		Provider:             s.Provider,
		ObservedAt:           s.ObservedAt,
		LimitReached:         s.LimitReached,
		LimitObservedAt:      s.LimitObservedAt,
		LimitUntil:           s.LimitUntil,
		ExtraUsage:           s.ExtraUsage,
		ExtraUsageObservedAt: s.ExtraUsageObservedAt,
	}
	if next.Provider != "" {
		out.Provider = next.Provider
	}
	if next.ObservedAt.After(out.ObservedAt) {
		out.ObservedAt = next.ObservedAt
	}
	if !next.LimitObservedAt.IsZero() && !next.LimitObservedAt.Before(s.LimitObservedAt) {
		out.LimitReached, out.LimitObservedAt, out.LimitUntil = next.LimitReached, next.LimitObservedAt, next.LimitUntil
	}
	if !next.ExtraUsageObservedAt.IsZero() && !next.ExtraUsageObservedAt.Before(s.ExtraUsageObservedAt) {
		out.ExtraUsage, out.ExtraUsageObservedAt = next.ExtraUsage, next.ExtraUsageObservedAt
	}
	out.Windows = append(out.Windows, s.Windows...)
	for _, w := range next.Windows {
		replaced := false
		for i := range out.Windows {
			if out.Windows[i].Name == w.Name {
				if !w.ObservedAt.Before(out.Windows[i].ObservedAt) {
					out.Windows[i] = w
				}
				replaced = true
				break
			}
		}
		if !replaced {
			out.Windows = append(out.Windows, w)
		}
	}
	return out
}

// Parser extracts a snapshot from upstream response headers for one provider.
type Parser interface {
	// Provider returns the lower-case host provider key, e.g. "codex".
	Provider() string
	// Parse returns false when the headers carry no usable quota signal.
	Parse(headers http.Header, observedAt time.Time) (Snapshot, bool)
}

// Registry maps provider keys to parsers.
type Registry struct {
	parsers map[string]Parser
}

// NewRegistry returns a registry containing the given parsers.
func NewRegistry(parsers ...Parser) *Registry {
	r := &Registry{parsers: make(map[string]Parser, len(parsers))}
	for _, p := range parsers {
		r.parsers[normalizeProvider(p.Provider())] = p
	}
	return r
}

// DefaultRegistry returns the parsers shipped with the plugin.
func DefaultRegistry() *Registry {
	return NewRegistry(Codex{}, Claude{})
}

// Supports reports whether a parser exists for provider.
func (r *Registry) Supports(provider string) bool {
	if r == nil {
		return false
	}
	_, ok := r.parsers[normalizeProvider(provider)]
	return ok
}

// Providers lists the registered provider keys.
func (r *Registry) Providers() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.parsers))
	for key := range r.parsers {
		out = append(out, key)
	}
	return out
}

// Parse runs the provider's parser. Header keys are canonicalized first because
// the headers crossed a JSON boundary and may not be in canonical form.
func (r *Registry) Parse(provider string, headers http.Header, observedAt time.Time) (Snapshot, bool) {
	if r == nil || len(headers) == 0 {
		return Snapshot{}, false
	}
	p, ok := r.parsers[normalizeProvider(provider)]
	if !ok {
		return Snapshot{}, false
	}
	canonical := make(http.Header, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		name := http.CanonicalHeaderKey(strings.TrimSpace(key))
		canonical[name] = append(canonical[name], values...)
	}
	return p.Parse(canonical, observedAt)
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
