package router

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxAffinityEntries = 10000

// sessionMetadataKeys are host-populated scheduler metadata keys that carry a
// session identity (sdk/cliproxy/executor/types.go upstream), strongest first.
var sessionMetadataKeys = []string{
	"canonical_session_id",
	"execution_session_id",
	"lcp_affinity_session_id",
	"derived_session_id",
}

// sessionHeaders are client headers the host itself treats as session IDs
// (sdk/cliproxy/session/info.go upstream). Per-request IDs are excluded.
var sessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"Session-Id",
	"Session_id",
	"X-Session-Id",
	"X-Session-Affinity",
	"Thread-Id",
	"X-Thread-Id",
	"X-Conversation-Id",
}

// sessionKey derives an opaque key for the request's session. The raw session
// identifier is hashed immediately and never stored or logged.
func sessionKey(headers map[string][]string, metadata map[string]any) string {
	raw := ""
	for _, key := range sessionMetadataKeys {
		if v, ok := metadata[key].(string); ok && strings.TrimSpace(v) != "" {
			raw = "m:" + strings.TrimSpace(v)
			break
		}
	}
	if raw == "" && len(headers) > 0 {
		h := http.Header(headers)
		for _, name := range sessionHeaders {
			if v := firstHeader(h, name); v != "" {
				raw = "h:" + v
				break
			}
		}
	}
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// firstHeader matches case-insensitively because the map crossed a JSON
// boundary and may not use canonical keys.
func firstHeader(h http.Header, name string) string {
	for key, values := range h {
		if strings.EqualFold(key, name) {
			for _, v := range values {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

type binding struct {
	authID  string
	expires time.Time
}

// affinity maps session keys to the auth they were last routed to.
type affinity struct {
	mu       sync.Mutex
	bindings map[string]binding
}

func newAffinity() *affinity {
	return &affinity{bindings: make(map[string]binding)}
}

func (a *affinity) lookup(key string, now time.Time) (string, bool) {
	if key == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.bindings[key]
	if !ok || now.After(b.expires) {
		return "", false
	}
	return b.authID, true
}

func (a *affinity) bind(key, authID string, now time.Time, ttl time.Duration) {
	if key == "" || authID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.bindings[key]; !exists && len(a.bindings) >= maxAffinityEntries {
		a.evictLocked(now)
	}
	a.bindings[key] = binding{authID: authID, expires: now.Add(ttl)}
}

// evictLocked drops expired bindings, or the one closest to expiry if none are.
func (a *affinity) evictLocked(now time.Time) {
	oldestKey := ""
	var oldest time.Time
	for key, b := range a.bindings {
		if now.After(b.expires) {
			delete(a.bindings, key)
			continue
		}
		if oldestKey == "" || b.expires.Before(oldest) {
			oldestKey, oldest = key, b.expires
		}
	}
	if len(a.bindings) >= maxAffinityEntries && oldestKey != "" {
		delete(a.bindings, oldestKey)
	}
}

func (a *affinity) prune(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, b := range a.bindings {
		if now.After(b.expires) {
			delete(a.bindings, key)
		}
	}
}

func (a *affinity) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.bindings)
}
