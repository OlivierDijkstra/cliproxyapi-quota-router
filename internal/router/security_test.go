package router

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Fixture values only; none of these are real credentials.
const (
	fakeToken     = "fixture-access-token-0000"
	fakeAPIKey    = "fixture-client-key-1111"
	fakeCookie    = "fixture-cookie-2222"
	fakeSession   = "fixture-session-3333"
	fakeAccountID = "codex-person@example.com.json"
)

func TestDiagnosticsDoNotLeakSecrets(t *testing.T) {
	var logs []string
	r, _ := newTestRouter(WithLogger(func(level, msg string, fields map[string]any) {
		raw, _ := json.Marshal(fields)
		logs = append(logs, level+" "+msg+" "+string(raw))
	}))
	cfg := DefaultConfig()
	cfg.LogDecisions = true
	r.SetConfig(cfg)
	h := &Handler{Router: r}

	headers := codexHeaders(win{used: 30, resetIn: 3 * time.Hour}, nil)
	headers.Set("Authorization", "Bearer "+fakeToken)
	headers.Set("Set-Cookie", fakeCookie)
	r.ObserveUsage(pluginapi.UsageRecord{
		AuthID: fakeAccountID, Provider: "codex", APIKey: fakeAPIKey, SessionID: fakeSession, ResponseHeaders: headers,
	})

	req := pickReq(pluginapi.SchedulerAuthCandidate{
		ID: fakeAccountID, Provider: "codex", Status: "active",
		Attributes: map[string]string{"api_key": fakeAPIKey},
		Metadata:   map[string]any{"access_token": fakeToken},
	})
	req.Options.Headers = map[string][]string{"Authorization": {"Bearer " + fakeAPIKey}, "Session-Id": {fakeSession}}
	req.Options.Metadata = map[string]any{"refresh_token": fakeToken}
	raw, _ := json.Marshal(req)
	h.Handle("scheduler.pick", raw)

	status, _ := json.Marshal(r.Status())
	out := string(status) + strings.Join(logs, "\n")
	for _, secret := range []string{fakeToken, fakeAPIKey, fakeCookie, fakeSession, "person@example.com"} {
		if strings.Contains(out, secret) {
			t.Errorf("diagnostics contain %q", secret)
		}
	}
	if !strings.Contains(out, authLabel(fakeAccountID)) {
		t.Error("expected hashed auth label in diagnostics")
	}
}

func TestRevealAuthIDsIsOptIn(t *testing.T) {
	r, _ := newTestRouter()
	cfg := DefaultConfig()
	cfg.RevealAuthIDs = true
	r.SetConfig(cfg)
	observe(r, fakeAccountID, 50, time.Hour)
	_, d := r.Pick(pickReq(codexCandidates(fakeAccountID)...))
	if d.Selected != fakeAccountID {
		t.Fatalf("selected label = %q", d.Selected)
	}
}

func TestStoreKeepsOnlyQuotaFields(t *testing.T) {
	r, _ := newTestRouter()
	headers := codexHeaders(win{used: 30, resetIn: time.Hour}, nil)
	headers.Set("Authorization", "Bearer "+fakeToken)
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex", ResponseHeaders: headers})
	snap, _ := r.Store().Snapshot("a")
	raw, _ := json.Marshal(snap)
	if strings.Contains(string(raw), fakeToken) || strings.Contains(string(raw), "fixture-cookie") {
		t.Fatalf("snapshot retained non-quota data: %s", raw)
	}
}
