package requestrecorder

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ModelOverridePolicy is copied once when a request is admitted. Changes from
// the local control panel cannot change an in-flight request's decision.
type ModelOverridePolicy struct {
	Enabled  bool   `json:"enabled"`
	Model    string `json:"model"`
	Revision uint64 `json:"revision"`
}

type ModelOverrideAudit struct {
	Policy          ModelOverridePolicy `json:"policy"`
	Eligible        bool                `json:"eligible_endpoint"`
	Applied         bool                `json:"applied"`
	Changed         bool                `json:"model_value_changed"`
	OriginalModel   string              `json:"original_model,omitempty"`
	ForwardedModel  string              `json:"forwarded_model,omitempty"`
	EncodingChanged bool                `json:"content_encoding_changed"`
}

type modelOverrideError struct {
	Status int
	Code   string
}

func (e *modelOverrideError) Error() string { return e.Code }
func overrideFailure(status int, code string) *modelOverrideError {
	return &modelOverrideError{Status: status, Code: code}
}
func validModelName(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validateModelOverride(enabled bool, model string) error {
	if (enabled || model != "") && !validModelName(model) {
		return errors.New("force_model must be a nonempty model identifier (maximum 256 bytes) when enabled")
	}
	return nil
}
func (e *Engine) ModelOverride() ModelOverridePolicy {
	e.overrideMu.RLock()
	defer e.overrideMu.RUnlock()
	return e.override
}

// SetModelOverride changes this process only. It neither persists settings nor
// contacts upstream, and recording pause is independent of this switch.
func (e *Engine) SetModelOverride(enabled bool, model string) (ModelOverridePolicy, error) {
	if err := validateModelOverride(enabled, model); err != nil {
		return ModelOverridePolicy{}, err
	}
	e.overrideMu.Lock()
	defer e.overrideMu.Unlock()
	e.override = ModelOverridePolicy{Enabled: enabled, Model: model, Revision: e.override.Revision + 1}
	return e.override, nil
}
func modelOverrideEndpoint(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/backend-api/codex/responses", "/backend-api/codex/responses/compact",
		"/backend-api/conversation", "/v1/responses", "/v1/responses/compact",
		"/v1/chat/completions", "/v1/completions":
		return true
	}
	return false
}

// replaceTopLevelModel parses the JSON structure and splices only the value of
// the unique top-level model key. All other bytes, ordering, numeric precision,
// whitespace, prompts and nested model keys remain unchanged. Missing/duplicate
// keys fail closed instead of guessing or silently forwarding the wrong model.
func replaceTopLevelModel(data []byte, target string) ([]byte, string, *modelOverrideError) {
	if !validModelName(target) {
		return nil, "", overrideFailure(400, "model_override_target_invalid")
	}
	if !utf8.Valid(data) || !json.Valid(data) {
		return nil, "", overrideFailure(400, "model_override_invalid_json")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, "", overrideFailure(400, "model_override_object_required")
	}
	found, start, end := false, 0, 0
	original := ""
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, "", overrideFailure(400, "model_override_invalid_json")
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, "", overrideFailure(400, "model_override_invalid_json")
		}
		if key != "model" {
			continue
		}
		if found {
			return nil, "", overrideFailure(400, "model_override_duplicate_model")
		}
		found = true
		if json.Unmarshal(value, &original) != nil || !validModelName(original) {
			return nil, "", overrideFailure(400, "model_override_model_string_required")
		}
		end = int(d.InputOffset())
		start = end - len(value)
	}
	if _, err := d.Token(); err != nil {
		return nil, "", overrideFailure(400, "model_override_invalid_json")
	}
	if !found {
		return nil, "", overrideFailure(400, "model_override_model_missing")
	}
	if original == target {
		return data, original, nil
	}
	encoded, _ := json.Marshal(target)
	out := make([]byte, 0, len(data)-(end-start)+len(encoded))
	out = append(out, data[:start]...)
	out = append(out, encoded...)
	out = append(out, data[end:]...)
	return out, original, nil
}

func (e *Engine) prepareModelOverride(w http.ResponseWriter, r *http.Request, policy ModelOverridePolicy, started time.Time, captureLimit int) (*http.Request, captured, ModelOverrideAudit, *modelOverrideError) {
	audit := ModelOverrideAudit{Policy: policy, Eligible: true}
	sample := newSample(started, captureLimit)
	fail := func(status int, code string) (*http.Request, captured, ModelOverrideAudit, *modelOverrideError) {
		return nil, sample.freeze(), audit, overrideFailure(status, code)
	}
	// Body signatures/digests cannot remain valid after mutation. Do not strip
	// integrity protection or forward a signature over different bytes.
	for _, key := range []string{"Content-MD5", "Digest", "Content-Digest", "Signature", "Signature-Input"} {
		if len(r.Header.Values(key)) > 0 {
			return fail(400, "model_override_signed_body_unsupported")
		}
	}
	if len(r.Trailer) > 0 {
		return fail(400, "model_override_request_trailers_unsupported")
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return fail(415, "model_override_json_content_type_required")
	}
	if r.Body == nil {
		return fail(400, "model_override_model_missing")
	}
	defer r.Body.Close()
	maxBytes := int64(e.config.MaxRequestMiB) << 20
	reader := &tap{source: http.MaxBytesReader(w, r.Body, maxBytes), sample: sample}
	raw, err := io.ReadAll(reader)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fail(413, "model_override_request_too_large")
		}
		return fail(400, "model_override_request_read_error")
	}
	decoded, note := decodeBody(raw, r.Header.Get("Content-Encoding"))
	if note != "" {
		status := 400
		if note == "decoded_body_limit" {
			status = 413
		}
		if note == "unsupported_content_encoding" {
			status = 415
		}
		return fail(status, "model_override_"+note)
	}
	if len(decoded) > decodedLimit || int64(len(decoded)) > maxBytes {
		return fail(413, "model_override_decoded_body_limit")
	}
	output, original, rewriteErr := replaceTopLevelModel(decoded, policy.Model)
	if rewriteErr != nil {
		return nil, sample.freeze(), audit, rewriteErr
	}
	if len(output) > decodedLimit || int64(len(output)) > maxBytes {
		return fail(413, "model_override_rewritten_body_limit")
	}
	audit.Applied, audit.Changed = true, original != policy.Model
	audit.OriginalModel, audit.ForwardedModel = original, policy.Model
	next := r.Clone(r.Context())
	if audit.Changed {
		audit.EncodingChanged = next.Header.Get("Content-Encoding") != "" && !strings.EqualFold(next.Header.Get("Content-Encoding"), "identity")
		next.Header.Del("Content-Encoding")
	} else {
		output = raw // even the original compression is retained for a no-op override
	}
	next.Body = io.NopCloser(bytes.NewReader(output))
	next.ContentLength = int64(len(output))
	next.GetBody = nil
	next.TransferEncoding = nil
	next.Header.Del("Transfer-Encoding")
	next.Header.Del("Content-Length")
	return next, sample.freeze(), audit, nil
}
