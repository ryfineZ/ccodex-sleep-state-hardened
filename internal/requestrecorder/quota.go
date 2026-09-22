package requestrecorder

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// QuotaWindow records upstream fields, not a billing calculation.
// Primary and secondary are not assumed to mean weekly or five-hour.
type QuotaWindow struct {
	LimitID       string            `json:"limit_id"`
	Window        string            `json:"window"`
	Source        string            `json:"source"`
	Sequence      int               `json:"sequence,omitempty"`
	AtMS          int64             `json:"at_ms"`
	UsedPercent   *float64          `json:"used_percent,omitempty"`
	WindowMinutes *int64            `json:"window_minutes,omitempty"`
	ResetAt       *int64            `json:"reset_at_unix,omitempty"`
	ResetAfter    *int64            `json:"reset_after_seconds,omitempty"`
	Raw           map[string]string `json:"observed_numeric_fields"`
	Invalid       []string          `json:"invalid_fields,omitempty"`
}
type quotaCollector struct {
	Windows []QuotaWindow
	Limited bool
}

var quotaFields = []string{"used-percent", "window-minutes", "reset-at", "reset-after-seconds"}

func validLimitID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func quotaHeaderName(name string) bool {
	name = strings.ToLower(name)
	for _, window := range []string{"primary", "secondary"} {
		for _, field := range quotaFields {
			suffix := "-" + window + "-" + field
			if strings.HasPrefix(name, "x-codex-") && strings.HasSuffix(name, suffix) {
				return validLimitID(strings.TrimSuffix(strings.TrimPrefix(name, "x-"), suffix))
			}
		}
	}
	return false
}
func (q *quotaCollector) Headers(h http.Header, source string, seq int, at int64) {
	groups := map[string]http.Header{}
	for key, values := range h {
		name := strings.ToLower(key)
		if !quotaHeaderName(name) {
			continue
		}
		for _, window := range []string{"primary", "secondary"} {
			for _, field := range quotaFields {
				suffix := "-" + window + "-" + field
				if !strings.HasSuffix(name, suffix) {
					continue
				}
				limit := strings.TrimSuffix(strings.TrimPrefix(name, "x-"), suffix)
				k := limit + "\x00" + window
				if groups[k] == nil {
					groups[k] = make(http.Header)
				}
				groups[k][http.CanonicalHeaderKey(field)] = append(groups[k].Values(field), values...)
			}
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts := strings.Split(key, "\x00")
		h := groups[key]
		w := QuotaWindow{LimitID: parts[0], Window: parts[1], Source: source, Sequence: seq, AtMS: at, Raw: map[string]string{}}
		for _, field := range quotaFields {
			vs := h.Values(field)
			if len(vs) == 0 {
				continue
			}
			if len(vs) != 1 || len(vs[0]) > 64 {
				w.Invalid = append(w.Invalid, field)
				continue
			}
			value := strings.TrimSpace(vs[0])
			if field == "used-percent" {
				n, err := strconv.ParseFloat(value, 64)
				if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
					w.Invalid = append(w.Invalid, field)
					continue
				}
				w.UsedPercent = &n
			} else {
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n < 0 || (field == "window-minutes" && n == 0) {
					w.Invalid = append(w.Invalid, field)
					continue
				}
				switch field {
				case "window-minutes":
					w.WindowMinutes = &n
				case "reset-at":
					w.ResetAt = &n
				case "reset-after-seconds":
					w.ResetAfter = &n
				}
			}
			w.Raw[field] = value
		}
		if len(q.Windows) >= 64 {
			q.Limited = true
			return
		}
		q.Windows = append(q.Windows, w)
	}
}
func headersFromEvent(data []byte) http.Header {
	var event struct {
		Headers map[string]json.RawMessage `json:"headers"`
	}
	h := make(http.Header)
	if json.Unmarshal(data, &event) != nil {
		return h
	}
	for key, raw := range event.Headers {
		var one string
		var many []string
		if json.Unmarshal(raw, &one) == nil {
			h.Set(key, one)
		} else if json.Unmarshal(raw, &many) == nil {
			h[http.CanonicalHeaderKey(key)] = many
		}
	}
	return h
}
func (q *quotaCollector) Event(name string, data []byte, seq int, at int64) {
	if name == "response.metadata" {
		q.Headers(headersFromEvent(data), "sse_metadata", seq, at)
		return
	}
	if name != "codex.rate_limits" {
		return
	}
	var event struct {
		LimitID string                                `json:"metered_limit_name"`
		Name    string                                `json:"limit_name"`
		Limits  map[string]map[string]json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	id := event.LimitID
	if id == "" {
		id = event.Name
	}
	if id == "" {
		id = "codex"
	}
	id = strings.ReplaceAll(strings.ToLower(id), "_", "-")
	if !validLimitID(id) {
		q.Limited = true
		return
	}
	h := make(http.Header)
	prefix := "x-" + id
	if prefix != "x-codex" && !strings.HasPrefix(prefix, "x-codex-") {
		q.Limited = true
		return
	}
	for _, window := range []string{"primary", "secondary"} {
		for _, item := range []struct{ json, header string }{{"used_percent", "used-percent"}, {"window_minutes", "window-minutes"}, {"reset_at", "reset-at"}} {
			raw := event.Limits[window][item.json]
			if len(raw) > 0 && string(raw) != "null" {
				h.Set(prefix+"-"+window+"-"+item.header, string(raw))
			}
		}
	}
	q.Headers(h, "sse_rate_limits", seq, at)
}
