package requestrecorder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

var safeHeaders = map[string]bool{
	"accept": true, "accept-encoding": true, "content-type": true, "content-length": true, "content-encoding": true,
	"cache-control": true, "retry-after": true, "transfer-encoding": true, "openai-model": true, "x-model": true, "x-actual-model": true, "x-served-model": true,
}

func fingerprint(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:12]) }
func redactHeaders(h http.Header, full bool) http.Header {
	out := make(http.Header)
	for k, vs := range h {
		if strings.EqualFold(k, "X-Recorder-Token") {
			out[k] = []string{"[redacted]"}
			continue
		}
		if full || safeHeaders[strings.ToLower(k)] || quotaHeaderName(k) {
			out[k] = append([]string(nil), vs...)
		} else {
			out[k] = []string{"[redacted sha256:" + fingerprint(strings.Join(vs, "\x00")) + "]"}
		}
	}
	return out
}
func sensitive(k string) bool {
	n := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(k))
	switch n {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "xcodexturnstate", "state", "token", "accesstoken", "refreshtoken", "idtoken", "apikey", "xapikey", "password", "secret", "clientsecret", "credentials", "encryptedcontent", "chatgptaccountid", "accountid":
		return true
	}
	return strings.Contains(n, "password") || strings.HasSuffix(n, "secret")
}
func redactJSON(data []byte) json.RawMessage {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || !json.Valid(data) {
		return nil
	}
	var walk func(any, int) any
	walk = func(v any, depth int) any {
		if depth > 64 {
			return "[depth limit]"
		}
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				if sensitive(k) {
					x[k] = "[redacted]"
				} else if strings.EqualFold(k, "headers") {
					if h, ok := vv.(map[string]any); ok {
						for name, val := range h {
							if !safeHeaders[strings.ToLower(name)] && !quotaHeaderName(name) {
								h[name] = "[redacted]"
							} else {
								h[name] = walk(val, depth+1)
							}
						}
					} else {
						x[k] = "[unrecognized headers omitted]"
					}
				} else {
					x[k] = walk(vv, depth+1)
				}
			}
		case []any:
			for i := range x {
				x[i] = walk(x[i], depth+1)
			}
		}
		return v
	}
	out, e := json.Marshal(walk(v, 0))
	if e != nil {
		return nil
	}
	return out
}
func recordedURI(u *url.URL, full bool) string {
	if full {
		return u.RequestURI()
	}
	if u.RawQuery == "" {
		return u.EscapedPath()
	}
	// Query names/values may both contain secrets. Keep only a correlation hash.
	return u.EscapedPath() + "?[redacted-query sha256:" + fingerprint(u.RawQuery) + "]"
}
