package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Plugin identity. Version is overridden at release time with -ldflags -X.
var Version = "0.1.0"

const (
	PluginName = "cliproxyapi-quota-router"
	repository = "https://github.com/OlivierDijkstra/cliproxyapi-quota-router"
	logoURL    = "https://raw.githubusercontent.com/OlivierDijkstra/cliproxyapi-quota-router/main/assets/icon.svg"

	// StatusPath is registered under /v0/management, so it requires the
	// management key. Resource routes are unauthenticated and are not used.
	StatusPath = "/quota-router/status"
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities uses the host's JSON keys (internal/pluginhost/rpc_schema.go).
type capabilities struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities,omitempty"`
	UsagePlugin               bool `json:"usage_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

type managementRoute struct {
	Method      string
	Path        string
	Description string
}

type managementRegistration struct {
	Routes []managementRoute `json:"routes"`
}

// Handler dispatches host RPC methods to the router.
type Handler struct {
	Router *Router
	// OnConfigured runs after a successful register or reconfigure.
	OnConfigured func()
}

// Handle returns the JSON envelope for one host call. Malformed requests become
// error envelopes; Handle never panics on input.
//
// A scheduler.pick failure is reported as an unhandled pick rather than an
// error, because the host fails the client request on scheduler errors. A
// panic must not escape either: in a c-shared library it would abort the host.
func (h *Handler) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if p := recover(); p != nil {
			out = h.failure(method, fmt.Errorf("internal error: %v", p))
		}
	}()
	result, err := h.dispatch(method, request)
	if err != nil {
		return h.failure(method, err)
	}
	return result
}

func (h *Handler) failure(method string, err error) []byte {
	if h.Router != nil && h.Router.log != nil {
		h.Router.log("warn", "quota-router call failed", map[string]any{"method": method, "error": err.Error()})
	}
	if method == pluginabi.MethodSchedulerPick {
		if raw, errEnv := okEnvelope(pluginapi.SchedulerPickResponse{}); errEnv == nil {
			return raw
		}
	}
	return errorEnvelope("plugin_error", err.Error())
}

func (h *Handler) dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cfg, err := ParseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		h.Router.SetConfig(cfg)
		if h.OnConfigured != nil {
			h.OnConfigured()
		}
		return okEnvelope(h.registration(cfg))
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		h.Router.StopRefresher()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode scheduler pick: %w", err)
		}
		resp, _ := h.Router.Pick(req)
		return okEnvelope(resp)
	case pluginabi.MethodUsageHandle:
		var rec pluginapi.UsageRecord
		if err := json.Unmarshal(request, &rec); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		h.Router.ObserveUsage(rec)
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{Routes: []managementRoute{{
			Method:      http.MethodGet,
			Path:        StatusPath,
			Description: "Quota router configuration, cached quota and recent routing decisions",
		}}})
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return okEnvelope(h.management(req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func (h *Handler) registration(cfg Config) registration {
	field := func(name string, typ pluginapi.ConfigFieldType, desc string, enum ...string) pluginapi.ConfigField {
		return pluginapi.ConfigField{Name: name, Type: typ, Description: desc, EnumValues: enum}
	}
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           "Olivier Dijkstra",
			GitHubRepository: repository,
			Logo:             logoURL,
			ConfigFields: []pluginapi.ConfigField{
				field("mode", pluginapi.ConfigFieldTypeEnum, "Ranking: smart (usable quota per hour until it resets; expiring-first is another name for it) or headroom (most quota left).",
					string(ModeSmart), string(ModeExpiringFirst), string(ModeHeadroom)),
				field("minimum_headroom", pluginapi.ConfigFieldTypeNumber, "Skip credentials while any window has less than this fraction left. An empty window is always skipped. Default 0.02."),
				field("fallback", pluginapi.ConfigFieldTypeEnum, "Strategy when quota data cannot rank candidates. none leaves the pick to the host only while no candidate is excluded.",
					string(FallbackRoundRobin), string(FallbackFillFirst), string(FallbackNone)),
				field("refresh_interval", pluginapi.ConfigFieldTypeString, "How often to sync host auth state, e.g. 5m."),
				field("stale_after", pluginapi.ConfigFieldTypeString, "A credential with any quota window reported longer ago than this is re-probed, e.g. 30m."),
				field("probe_interval", pluginapi.ConfigFieldTypeString, "Minimum gap between probe picks of one credential without data, e.g. 1m."),
				field("min_reset_horizon", pluginapi.ConfigFieldTypeString, "Floor for time-to-reset in scoring so near resets cannot dominate, e.g. 1h."),
				field("switch_margin", pluginapi.ConfigFieldTypeNumber, "Keep the current credential unless another scores this fraction higher. Default 0.15."),
				field("tie_tolerance", pluginapi.ConfigFieldTypeNumber, "Scores within this fraction of the best rotate. Default 0.05."),
				field("session_affinity", pluginapi.ConfigFieldTypeBoolean, "Keep a client session on the same credential while it stays eligible."),
				field("session_affinity_ttl", pluginapi.ConfigFieldTypeString, "How long a session binding lasts, e.g. 1h."),
				field("across_priorities", pluginapi.ConfigFieldTypeBoolean, "Receive candidates from all priority tiers instead of only the highest."),
				field("log_decisions", pluginapi.ConfigFieldTypeBoolean, "Log one line per routing decision through the host logger."),
				field("reveal_auth_ids", pluginapi.ConfigFieldTypeBoolean, "Show raw auth IDs in diagnostics instead of hashed labels. Auth IDs can contain e-mail addresses."),
			},
		},
		Capabilities: capabilities{
			Scheduler:                 true,
			SchedulerAcrossPriorities: cfg.AcrossPriorities,
			UsagePlugin:               true,
			ManagementAPI:             true,
		},
	}
}

// Status is the body of the management status route.
type Status struct {
	Plugin      string           `json:"plugin"`
	Version     string           `json:"version"`
	Config      ConfigView       `json:"config"`
	Providers   []string         `json:"quota_providers"`
	Store       StoreStats       `json:"store"`
	Sessions    int              `json:"session_bindings"`
	Credentials []CredentialView `json:"credentials"`
	Decisions   []Decision       `json:"recent_decisions"`
}

// CredentialView is the cached quota of one credential, labelled like CandidateView.
type CredentialView struct {
	Auth            string       `json:"auth"`
	Provider        string       `json:"provider"`
	ObservedAt      time.Time    `json:"observed_at"`
	LimitReached    bool         `json:"limit_reached,omitempty"`
	LimitObservedAt time.Time    `json:"limit_observed_at,omitzero"`
	LimitUntil      time.Time    `json:"limit_until,omitzero"`
	ExtraUsage      bool         `json:"extra_usage,omitempty"`
	Windows         []WindowView `json:"windows"`
}

// WindowView is one quota window for diagnostics.
type WindowView struct {
	Name       string    `json:"name"`
	Remaining  float64   `json:"remaining"`
	Length     string    `json:"length,omitempty"`
	ResetAt    time.Time `json:"reset_at,omitzero"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// Status assembles the diagnostic snapshot.
func (r *Router) Status() Status {
	cfg := r.Config()
	providers := r.registry.Providers()
	sort.Strings(providers)
	st := Status{
		Plugin:    PluginName,
		Version:   Version,
		Config:    cfg.View(),
		Providers: providers,
		Store:     r.store.Stats(),
		Sessions:  r.aff.size(),
		Decisions: r.Decisions(),
	}
	for id, snap := range r.store.snapshotsCopy() {
		cv := CredentialView{
			Auth:            r.label(id, cfg),
			Provider:        snap.Provider,
			ObservedAt:      snap.ObservedAt,
			LimitReached:    snap.LimitReached,
			LimitObservedAt: snap.LimitObservedAt,
			LimitUntil:      snap.LimitUntil,
			ExtraUsage:      snap.ExtraUsage,
		}
		for _, w := range snap.Windows {
			wv := WindowView{Name: w.Name, Remaining: w.Remaining, ResetAt: w.ResetAt, ObservedAt: w.ObservedAt}
			if w.Length > 0 {
				wv.Length = w.Length.String()
			}
			cv.Windows = append(cv.Windows, wv)
		}
		st.Credentials = append(st.Credentials, cv)
	}
	sort.Slice(st.Credentials, func(i, j int) bool { return st.Credentials[i].Auth < st.Credentials[j].Auth })
	return st
}

func (h *Handler) management(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	jsonHeaders := http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}
	if !strings.HasSuffix(strings.TrimRight(req.Path, "/"), StatusPath) {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Headers: jsonHeaders, Body: []byte(`{"error":"not found"}`)}
	}
	if req.Method != "" && !strings.EqualFold(req.Method, http.MethodGet) {
		return pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: jsonHeaders, Body: []byte(`{"error":"method not allowed"}`)}
	}
	body, err := json.MarshalIndent(h.Router.Status(), "", "  ")
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError, Headers: jsonHeaders, Body: []byte(`{"error":"encode status"}`)}
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders, Body: body}
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := pluginabi.NewErrorEnvelope(code, message)
	return raw
}
