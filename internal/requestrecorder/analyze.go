package requestrecorder

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gylive/ccodex-sleep-state/internal/modelaudit"
	"github.com/klauspost/compress/zstd"
)

const decodedLimit = 16 << 20
const eventLimit = 256 << 10
const maxEvents = 1024

type Event struct {
	Sequence int             `json:"sequence"`
	AtMS     int64           `json:"at_ms"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data,omitempty"`
	Note     string          `json:"note,omitempty"`
}
type Body struct {
	Observed      int64           `json:"observed_bytes"`
	Captured      int64           `json:"captured_bytes"`
	EOF           bool            `json:"observed_eof"`
	Truncated     bool            `json:"truncated"`
	TimingLimited bool            `json:"timing_limited"`
	Encoding      string          `json:"content_encoding,omitempty"`
	Note          string          `json:"analysis_note,omitempty"`
	JSON          json.RawMessage `json:"json,omitempty"`
	Events        []Event         `json:"events,omitempty"`
	Raw           []Chunk         `json:"raw_chunks,omitempty"`
	EventCount    int             `json:"event_count,omitempty"`
	EventsLimited bool            `json:"events_limited,omitempty"`
	StreamOutcome string          `json:"stream_outcome,omitempty"`
}

func decodeBody(data []byte, enc string) ([]byte, string) {
	enc = strings.ToLower(strings.TrimSpace(enc))
	if enc == "" || enc == "identity" {
		return data, ""
	}
	var r io.Reader
	var closeFn func()
	switch enc {
	case "gzip":
		x, e := gzip.NewReader(bytes.NewReader(data))
		if e != nil {
			return nil, "invalid_compressed_body"
		}
		r = x
		closeFn = func() { _ = x.Close() }
	case "deflate":
		x, e := zlib.NewReader(bytes.NewReader(data))
		if e != nil {
			x = flate.NewReader(bytes.NewReader(data))
		}
		r = x
		closeFn = func() { _ = x.Close() }
	case "br":
		r = brotli.NewReader(bytes.NewReader(data))
		closeFn = func() {}
	case "zstd":
		x, e := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(decodedLimit), zstd.WithDecoderMaxWindow(decodedLimit))
		if e != nil {
			return nil, "invalid_compressed_body"
		}
		r = x
		closeFn = x.Close
	default:
		return nil, "unsupported_content_encoding"
	}
	defer closeFn()
	decoded, e := io.ReadAll(io.LimitReader(r, decodedLimit+1))
	if len(decoded) > decodedLimit {
		return nil, "decoded_body_limit"
	}
	if e != nil {
		return nil, "incomplete_compressed_body"
	}
	return decoded, ""
}

// walkEvents accepts LF, CRLF and bare CR, multi-line data and UTF-8 BOM.
// Missing final blank line remains an incomplete event, not an invented event.
func walkEvents(data []byte, emit func(string, []byte, int, int, bool)) bool {
	originalLength := len(data)
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	prefixLength := originalLength - len(data)
	name := ""
	var payload []byte
	total, seq := 0, 0
	dropping := false
	for pos := 0; pos < len(data); {
		i := bytes.IndexAny(data[pos:], "\r\n")
		if i < 0 {
			return true
		}
		end := pos + i
		next := end + 1
		if data[end] == '\r' && next < len(data) && data[next] == '\n' {
			next++
		}
		line := data[pos:end]
		pos = next
		if len(line) == 0 {
			if dropping || len(payload) > 0 {
				seq++
				emit(name, bytes.TrimSuffix(payload, []byte{'\n'}), seq, pos+prefixLength, dropping)
			}
			name = ""
			payload = nil
			total = 0
			dropping = false
			continue
		}
		total += len(line)
		if total > eventLimit {
			dropping = true
			payload = nil
			continue
		}
		if dropping {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "event":
			if len(value) <= 128 {
				name = string(value)
			}
		case "data":
			payload = append(payload, value...)
			payload = append(payload, '\n')
		}
	}
	return total > 0 || dropping
}

func eventName(name string, data []byte) string {
	var e struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &e) == nil && e.Type != "" && len(e.Type) <= 128 {
		return e.Type
	}
	return name
}
func analyseBody(c captured, h http.Header, mode string, d *modelaudit.Detector, response bool, observers ...func(string, []byte, int, int64)) Body {
	b := Body{Observed: c.Observed, Captured: c.Captured, EOF: c.EOF, Truncated: c.Truncated, TimingLimited: c.TimingLimited, Encoding: h.Get("Content-Encoding")}
	if mode == "full" {
		b.Raw = c.Chunks
	}
	data, note := decodeBody(c.bytes(), b.Encoding)
	if note != "" {
		b.Note = note
		if d != nil {
			d.Limited()
		}
		return b
	}
	if c.Truncated || !c.EOF {
		if d != nil {
			d.Limited()
		}
	}
	contentType, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	if contentType != "text/event-stream" {
		if json.Valid(data) {
			if mode != "metadata" {
				if mode == "full" {
					b.JSON = append(json.RawMessage{}, data...)
				} else {
					b.JSON = redactJSON(data)
				}
			}
			if response && d != nil {
				d.JSON(data, "response_json", "", 0, lastMS(c))
			}
		} else if len(data) > 0 {
			b.Note = "non_json_or_incomplete_body_not_rendered"
			if d != nil {
				d.Limited()
			}
		}
		return b
	}
	complete, failed := false, false
	compressed := b.Encoding != "" && !strings.EqualFold(b.Encoding, "identity")
	if compressed {
		b.TimingLimited = true
		b.Note = "compressed_event_times_use_capture_end"
	}
	partial := walkEvents(data, func(name string, p []byte, seq, end int, oversized bool) {
		b.EventCount++
		at := lastMS(c)
		if !compressed {
			offset := 0
			for _, ch := range c.Chunks {
				offset += len(ch.Data)
				if offset >= end {
					at = ch.AtMS
					break
				}
			}
		}
		e := Event{Sequence: seq, AtMS: at, Type: eventName(name, p)}
		if oversized {
			e.Note = "event_size_limit"
			b.EventsLimited = true
			if d != nil {
				d.Limited()
			}
		} else if bytes.Equal(bytes.TrimSpace(p), []byte("[DONE]")) {
			e.Type = "[DONE]"
			complete = true
		} else if !json.Valid(p) || !bytes.HasPrefix(bytes.TrimSpace(p), []byte("{")) {
			e.Note = "invalid_json_event"
			b.EventsLimited = true
		} else {
			switch e.Type {
			case "response.completed":
				complete = true
			case "response.failed", "response.incomplete", "error":
				failed = true
			}
			for _, observe := range observers {
				observe(e.Type, p, seq, at)
			}
			if d != nil {
				d.JSON(p, "response_sse", e.Type, seq, at)
			}
			if mode != "metadata" {
				if mode == "full" && json.Valid(p) {
					e.Data = append(json.RawMessage{}, p...)
				} else {
					e.Data = redactJSON(p)
				}
				if e.Data == nil {
					e.Note = "non_json_data_omitted"
				}
			}
		}
		if len(b.Events) < maxEvents {
			b.Events = append(b.Events, e)
		} else {
			b.EventsLimited = true
		}
	})
	if partial {
		b.EventsLimited = true
		b.Note = "incomplete_final_sse_event"
		if d != nil {
			d.Limited()
		}
	}
	if b.EventsLimited && d != nil {
		d.Limited()
	}
	b.StreamOutcome = "incomplete"
	if complete {
		b.StreamOutcome = "completed"
	}
	if failed {
		b.StreamOutcome = "failed"
	}
	if b.Truncated || b.EventsLimited || !b.EOF {
		if !failed {
			b.StreamOutcome = "observation_limited"
		}
	}
	return b
}
func lastMS(c captured) int64 {
	if len(c.Chunks) == 0 {
		return 0
	}
	return c.Chunks[len(c.Chunks)-1].AtMS
}

// AnalyzeSaved reruns model rules against the structured captured evidence. It
// never contacts upstream and cannot reconstruct bytes omitted at capture time.
func AnalyzeSaved(r Record) modelaudit.Result {
	requested := r.Model.Requested
	if r.IncomingRequest != nil {
		if v := modelaudit.RequestModel(r.IncomingRequest.JSON); v != "" {
			requested = v
		}
	} else if !r.ModelOverride.Applied {
		if v := modelaudit.RequestModel(r.Request.JSON); v != "" {
			requested = v
		}
	}
	forwarded := modelaudit.RequestModel(r.Request.JSON)
	if forwarded == "" {
		forwarded = r.Model.Forwarded
	}
	if r.ModelOverride.Applied {
		requested, forwarded = r.ModelOverride.OriginalModel, r.ModelOverride.ForwardedModel
	} else if forwarded == "" {
		forwarded = requested
	}
	// Historical alias maps are deliberately ignored; compare raw identifiers.
	d := modelaudit.New(requested, forwarded)
	d.Headers(r.ResponseHeaders, "response_header", 0, r.HeadersMS)
	if len(r.Response.Raw) > 0 {
		c := captured{Chunks: r.Response.Raw, Observed: r.Response.Observed, Captured: r.Response.Captured, EOF: r.Response.EOF, Truncated: r.Response.Truncated, TimingLimited: r.Response.TimingLimited}
		_ = analyseBody(c, r.ResponseHeaders, "metadata", d, true)
	} else {
		d.JSON(r.Response.JSON, "response_json", "", 0, r.DurationMS)
		for _, e := range r.Response.Events {
			d.JSON(e.Data, "response_sse", e.Type, e.Sequence, e.AtMS)
		}
		if r.Mode == "metadata" {
			d.Limited()
		}
	}
	if r.Response.Truncated || r.Response.EventsLimited || r.Response.Note != "" || !r.Response.EOF {
		d.Limited()
	}
	return d.Result()
}
