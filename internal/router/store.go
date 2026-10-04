package router

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota"
)

// HostAuthState is the subset of host.auth.list used for eligibility.
type HostAuthState struct {
	Disabled       bool
	NextRetryAfter time.Time
}

// Store caches quota snapshots and host auth state per auth ID. All fields are
// guarded by mu; picks, usage callbacks and the refresher run concurrently.
type Store struct {
	registry *quota.Registry

	mu          sync.Mutex
	snapshots   map[string]quota.Snapshot
	probedAt    map[string]time.Time
	host        map[string]HostAuthState
	hostAt      time.Time
	hostErr     string
	lastRefresh time.Time
}

// NewStore returns an empty store that parses headers with registry.
func NewStore(registry *quota.Registry) *Store {
	return &Store{
		registry:  registry,
		snapshots: make(map[string]quota.Snapshot),
		probedAt:  make(map[string]time.Time),
		host:      make(map[string]HostAuthState),
	}
}

// Observe records the quota headers of one completed upstream attempt and
// returns true when they changed the cached snapshot. A response without quota
// headers leaves the snapshot in place. Partial headers update only the signals
// they carry (see quota.Snapshot.Merge), and a late, older report never
// overwrites a newer one.
func (s *Store) Observe(authID, provider string, headers http.Header, at time.Time) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	next, ok := s.registry.Parse(provider, headers, at)
	if !ok {
		return false
	}
	// Window freshness is judged per window; never let one inherit a later
	// snapshot time through a merge.
	for i := range next.Windows {
		if next.Windows[i].ObservedAt.IsZero() {
			next.Windows[i].ObservedAt = next.ObservedAt
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, exists := s.snapshots[authID]
	merged := next
	if exists {
		merged = prev.Merge(next)
		if reflect.DeepEqual(merged, prev) {
			return false
		}
	}
	s.snapshots[authID] = merged
	// Only window data ends a probe. A bare limit flag says nothing about how
	// much quota is left, so the credential stays on the probe schedule.
	if len(next.Windows) > 0 {
		delete(s.probedAt, authID)
	}
	return true
}

// Snapshot returns the cached snapshot for authID.
func (s *Store) Snapshot(authID string) (quota.Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[authID]
	return snap, ok
}

func (s *Store) snapshotsCopy() map[string]quota.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]quota.Snapshot, len(s.snapshots))
	for id, snap := range s.snapshots {
		out[id] = snap
	}
	return out
}

// MarkProbed records that an auth without fresh data was picked to learn its quota.
func (s *Store) MarkProbed(authID string, at time.Time) {
	s.mu.Lock()
	s.probedAt[authID] = at
	s.mu.Unlock()
}

// ProbedAt returns when authID was last picked as a probe.
func (s *Store) ProbedAt(authID string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probedAt[authID]
}

// HostState returns the host auth state for authID if the last refresh is
// younger than maxAge.
func (s *Store) HostState(authID string, now time.Time, maxAge time.Duration) (HostAuthState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostAt.IsZero() || now.Sub(s.hostAt) > maxAge {
		return HostAuthState{}, false
	}
	state, ok := s.host[authID]
	return state, ok
}

// ApplyHostStates replaces the host auth cache and drops snapshots of auths
// that no longer exist.
func (s *Store) ApplyHostStates(states map[string]HostAuthState, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = states
	s.hostAt = at
	s.hostErr = ""
	s.lastRefresh = at
	for id := range s.snapshots {
		if _, ok := states[id]; !ok {
			delete(s.snapshots, id)
			delete(s.probedAt, id)
		}
	}
}

// RecordRefreshError keeps the previous host cache and remembers the failure.
func (s *Store) RecordRefreshError(msg string, at time.Time) {
	s.mu.Lock()
	s.hostErr = msg
	s.lastRefresh = at
	s.mu.Unlock()
}

// StoreStats is a point-in-time summary for diagnostics.
type StoreStats struct {
	Snapshots        int       `json:"snapshots"`
	HostAuths        int       `json:"host_auths"`
	LastRefresh      time.Time `json:"last_refresh,omitzero"`
	LastHostSync     time.Time `json:"last_host_sync,omitzero"`
	LastRefreshError string    `json:"last_refresh_error,omitempty"`
}

// Stats summarizes the store without exposing auth IDs.
func (s *Store) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StoreStats{
		Snapshots:        len(s.snapshots),
		HostAuths:        len(s.host),
		LastRefresh:      s.lastRefresh,
		LastHostSync:     s.hostAt,
		LastRefreshError: s.hostErr,
	}
}
