// Package modelaudit reports observable upstream declarations, never an assertion
// about hidden model execution. Only allowlisted structural fields are inspected.
package modelaudit

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

type Evidence struct {
	Source       string `json:"source"`
	Path         string `json:"path"`
	Value        string `json:"value"`
	Event        string `json:"event,omitempty"`
	Sequence     int    `json:"sequence,omitempty"`
	AtMS         int64  `json:"at_ms"`
	Supplemental bool   `json:"supplemental,omitempty"`
}
type Result struct {
	RequestRewritten     bool       `json:"request_model_rewritten"`
	RawMismatch          bool       `json:"raw_identifier_mismatch"`
	SupplementalConflict bool       `json:"supplemental_conflict"`
	Requested            string     `json:"requested_model,omitempty"`
	Forwarded            string     `json:"forwarded_model,omitempty"`
	Declared             []string   `json:"upstream_declared_models"`
	Verdict              string     `json:"verdict"`
	Comparison           string     `json:"comparison"`
	ActualVerified       bool       `json:"actual_execution_verified"`
	Evidence             []Evidence `json:"evidence"`
	Limited              bool       `json:"evidence_limited"`
}
type Detector struct {
	requested, forwarded string
	evidence             []Evidence
	limited              bool
}

func valid(s string) bool {
	if s == "" || len(s) > 256 || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func RequestModel(data []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &v) != nil || !valid(v.Model) {
		return ""
	}
	return v.Model
}
func New(requested, forwarded string) *Detector {
	if !valid(requested) {
		requested = ""
	}
	if !valid(forwarded) {
		forwarded = ""
	}
	return &Detector{requested: requested, forwarded: forwarded}
}
func (d *Detector) Limited() { d.limited = true }
func (d *Detector) add(e Evidence) {
	if !valid(e.Value) {
		return
	}
	// Keep the first location of each declaration per source/path/event. Repeated
	// chat-completion chunks cannot consume unbounded memory or hide a late conflict.
	for _, old := range d.evidence {
		if old.Source == e.Source && old.Path == e.Path && old.Value == e.Value && old.Event == e.Event {
			return
		}
	}
	if len(d.evidence) >= 128 {
		d.limited = true
		return
	}
	d.evidence = append(d.evidence, e)
}
func (d *Detector) Headers(h http.Header, source string, sequence int, at int64) {
	// Nonstandard headers are corroborating hints, not proof of the actual model.
	for _, key := range []string{"OpenAI-Model", "X-Model", "X-Actual-Model", "X-Served-Model"} {
		for _, value := range h.Values(key) {
			d.add(Evidence{Source: source, Path: key, Value: value, Sequence: sequence, AtMS: at, Supplemental: true})
		}
	}
}
func (d *Detector) JSON(data []byte, source, event string, sequence int, at int64) {
	// Only protocol envelope events may contribute model evidence.
	switch event {
	case "", "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete", "response.metadata", "message", "message_delta":
	default:
		return
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(data, &v) != nil {
		return
	}
	read := func(raw json.RawMessage, path string, supp bool) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			d.add(Evidence{Source: source, Path: path, Value: s, Event: event, Sequence: sequence, AtMS: at, Supplemental: supp})
		}
	}
	read(v["model"], "$.model", false)
	var response map[string]json.RawMessage
	if json.Unmarshal(v["response"], &response) == nil {
		read(response["model"], "$.response.model", false)
	}
	// These are explicitly labeled observed ChatGPT fields, not a guaranteed API.
	var message struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(v["message"], &message) == nil {
		read(message.Metadata["model_slug"], "$.message.metadata.model_slug", false)
		read(message.Metadata["default_model_slug"], "$.message.metadata.default_model_slug", true)
	}
	if event == "response.metadata" {
		var headers map[string]json.RawMessage
		if json.Unmarshal(v["headers"], &headers) == nil {
			h := make(http.Header)
			for k, raw := range headers {
				var one string
				var many []string
				if json.Unmarshal(raw, &one) == nil {
					h.Set(k, one)
				} else if json.Unmarshal(raw, &many) == nil {
					h[http.CanonicalHeaderKey(k)] = many
				}
			}
			d.Headers(h, "sse_metadata_header", sequence, at)
		}
	}
}
func (d *Detector) Result() Result {
	r := Result{Requested: d.requested, Forwarded: d.forwarded, Declared: []string{}, Evidence: append([]Evidence{}, d.evidence...), Verdict: "unknown", Comparison: "exact_identifier_only", Limited: d.limited}
	r.RequestRewritten = r.Requested != "" && r.Forwarded != "" && r.Requested != r.Forwarded
	seen := map[string]bool{}
	for _, e := range r.Evidence {
		if !e.Supplemental && !seen[e.Value] {
			seen[e.Value] = true
			r.Declared = append(r.Declared, e.Value)
		}
	}
	for _, e := range r.Evidence {
		if e.Supplemental && len(seen) > 0 && !seen[e.Value] {
			r.SupplementalConflict = true
		}
	}
	sort.Strings(r.Declared)
	switch len(r.Declared) {
	case 0:
	case 1:
		if r.Forwarded == "" {
			r.Verdict = "observed"
		} else if r.Forwarded == r.Declared[0] {
			r.Verdict = "consistent"
		} else {
			r.RawMismatch = true
			r.Verdict = "mismatch"
		}
	default:
		r.Verdict = "conflict"
	}
	return r
}
