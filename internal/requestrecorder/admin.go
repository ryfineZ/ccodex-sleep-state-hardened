package requestrecorder

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

//go:embed web/*
var assets embed.FS

func (e *Engine) admin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	switch r.URL.Path {
	case "/__recorder/", "/__recorder/app.js", "/__recorder/style.css", "/__recorder/core.js":
		if r.Method != "GET" {
			localError(w, 405, "method_not_allowed")
			return
		}
		name, kind := "index.html", "text/html; charset=utf-8"
		if strings.HasSuffix(r.URL.Path, "app.js") {
			name, kind = "app.js", "text/javascript; charset=utf-8"
		}
		if strings.HasSuffix(r.URL.Path, "core.js") {
			name, kind = "core.js", "text/javascript; charset=utf-8"
		}
		if strings.HasSuffix(r.URL.Path, "style.css") {
			name, kind = "style.css", "text/css; charset=utf-8"
		}
		b, _ := assets.ReadFile("web/" + name)
		w.Header().Set("Content-Type", kind)
		_, _ = w.Write(b)
		return
	}
	if r.URL.Path == "/__recorder/api/launch" {
		e.exchangeLaunch(w, r)
		return
	}
	origin := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	if (origin != "" && origin != "http://"+e.config.Listen) || (site != "" && site != "same-origin" && site != "none") || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Recorder-Token")), []byte(e.token)) != 1 {
		localError(w, 403, "recorder_token_required")
		return
	}
	if e.setup != nil && strings.HasPrefix(r.URL.Path, "/__recorder/api/setup/") {
		e.setup.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__recorder/api/state-core/") {
		e.coreAPI(w, r)
		return
	}
	switch {
	case r.URL.Path == "/__recorder/api/new-launch" && r.Method == "POST":
		responseJSON(w, 200, map[string]string{"ticket": e.launch.renew()})
	case r.URL.Path == "/__recorder/api/status" && r.Method == "GET":
		responseJSON(w, 200, e.Status())
	case r.URL.Path == "/__recorder/api/observations" && r.Method == "GET":
		responseJSON(w, 200, e.store.Observations())
	case r.URL.Path == "/__recorder/api/records" && r.Method == "GET":
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		responseJSON(w, 200, e.store.List(limit))
	case strings.HasPrefix(r.URL.Path, "/__recorder/api/records/") && r.Method == "GET":
		id := strings.TrimPrefix(r.URL.Path, "/__recorder/api/records/")
		rec, err := e.store.Read(id)
		if err != nil {
			localError(w, 404, "record_not_found")
			return
		}
		responseJSON(w, 200, rec)
	case r.URL.Path == "/__recorder/api/model-override" && r.Method == "POST":
		var v struct {
			Enabled *bool  `json:"enabled"`
			Model   string `json:"model"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || v.Enabled == nil || d.Decode(new(any)) != io.EOF {
			localError(w, 400, "model_override_settings_invalid")
			return
		}
		policy, err := e.SetModelOverride(*v.Enabled, v.Model)
		if err != nil {
			localError(w, 400, "model_override_target_invalid")
			return
		}
		responseJSON(w, 200, map[string]any{"model_override": policy, "inflight_requests_unchanged": true, "persisted": false})
	case r.URL.Path == "/__recorder/api/capture" && r.Method == "POST":
		var v struct {
			Enabled *bool `json:"enabled"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || v.Enabled == nil || d.Decode(new(any)) != io.EOF {
			localError(w, 400, "enabled_boolean_required")
			return
		}
		e.enabled.Store(*v.Enabled)
		responseJSON(w, 200, map[string]any{"recording": *v.Enabled, "inflight_records_unchanged": true})
	default:
		localError(w, 404, "recorder_endpoint_not_found")
	}
}
