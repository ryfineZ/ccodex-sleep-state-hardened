package requestrecorder

import (
	"net/http"
	"strings"
)

type ProtocolContext struct {
	Scope                    string `json:"credential_scope_fingerprint"`
	Route                    string `json:"configured_route_label"`
	StateFingerprint         string `json:"request_state_fingerprint,omitempty"`
	StateLength              int    `json:"request_state_length"`
	CookieFingerprint        string `json:"request_cookie_fingerprint,omitempty"`
	ResponseStateFingerprint string `json:"response_header_state_fingerprint,omitempty"`
	ResponseStateLength      int    `json:"response_header_state_length"`
	SetCookieFingerprint     string `json:"set_cookie_fingerprint,omitempty"`
}

func contextFrom(h http.Header, route string) ProtocolContext {
	values := []string{h.Get("Authorization"), h.Get("ChatGPT-Account-Id"), h.Get("OpenAI-Organization"), h.Get("OpenAI-Project")}
	c := ProtocolContext{Scope: "anonymous", Route: route, StateLength: len(h.Get("X-Codex-Turn-State"))}
	if strings.Join(values, "") != "" {
		c.Scope = fingerprint(strings.Join(values, "\x00"))
	}
	if v := h.Get("X-Codex-Turn-State"); v != "" {
		c.StateFingerprint = fingerprint(v)
	}
	if v := strings.Join(h.Values("Cookie"), "\x00"); v != "" {
		c.CookieFingerprint = fingerprint(v)
	}
	return c
}
func (c *ProtocolContext) response(h http.Header) {
	v := h.Get("X-Codex-Turn-State")
	c.ResponseStateLength = len(v)
	if v != "" {
		c.ResponseStateFingerprint = fingerprint(v)
	}
	if v := strings.Join(h.Values("Set-Cookie"), "\x00"); v != "" {
		c.SetCookieFingerprint = fingerprint(v)
	}
}
