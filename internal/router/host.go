package router

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HostCall performs one raw host callback through the C ABI and returns the
// response envelope and the C return code.
type HostCall func(method string, payload []byte) ([]byte, int)

// HostClient adapts HostCall to the router's AuthLister and LogFunc.
type HostClient struct {
	Call HostCall
}

type hostAuthList struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostLogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// ListAuths calls host.auth.list. The C call cannot be cancelled, so ctx is
// only checked before the call.
func (h HostClient) ListAuths(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := h.invoke(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return nil, err
	}
	var out hostAuthList
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", pluginabi.MethodHostAuthList, err)
	}
	return out.Files, nil
}

// Log forwards a diagnostic line to host.log. Failures are dropped: logging
// must never affect routing.
func (h HostClient) Log(level, msg string, fields map[string]any) {
	_, _ = h.invoke(pluginabi.MethodHostLog, hostLogRequest{Level: level, Message: msg, Fields: fields})
}

func (h HostClient) invoke(method string, payload any) (json.RawMessage, error) {
	if h.Call == nil {
		return nil, fmt.Errorf("%s: host callbacks unavailable", method)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, code := h.Call(method, body)
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: empty host response (code %d)", method, code)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s: host returned an error", method)
	}
	if code != 0 {
		return nil, fmt.Errorf("%s: host returned code %d", method, code)
	}
	return env.Result, nil
}
