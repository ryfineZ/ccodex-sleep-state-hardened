package requestrecorder

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gylive/ccodex-sleep-state/internal/modelaudit"
	"github.com/klauspost/compress/zstd"
)

func snapshot(data string) captured {
	return captured{Chunks: []Chunk{{AtMS: 7, Data: []byte(data)}}, Observed: int64(len(data)), Captured: int64(len(data)), EOF: true}
}
func TestSSEMultilineCRLFMetadataAndFailurePrecedence(t *testing.T) {
	data := "\xef\xbb\xbfevent: response.metadata\r\ndata: {\"headers\":{\"X-Model\":\"hint\",\"X-Codex-Turn-State\":\"secret-state\"}}\r\n\r\ndata: {\"type\":\"response.created\",\r\ndata: \"response\":{\"model\":\"luna\"}}\r\n\r\ndata: {\"type\":\"response.failed\"}\r\rdata: {\"type\":\"response.completed\"}\r\r"
	d := modelaudit.New("astra", "astra")
	b := analyseBody(snapshot(data), http.Header{"Content-Type": {"text/event-stream"}}, "redacted", d, true)
	raw, _ := json.Marshal(b)
	if bytes.Contains(raw, []byte("secret-state")) {
		t.Fatal("metadata leaked state")
	}
	if d.Result().Verdict != "mismatch" || b.StreamOutcome != "failed" || len(b.Events) != 4 {
		t.Fatal(d.Result(), b.StreamOutcome, len(b.Events))
	}
}
func TestOversizedEventResyncAndBoundedCapture(t *testing.T) {
	data := "data: " + strings.Repeat("x", eventLimit+10) + "\n\n" + completeFixture
	d := modelaudit.New("astra", "astra")
	b := analyseBody(snapshot(data), http.Header{"Content-Type": {"text/event-stream"}}, "redacted", d, true)
	if !b.EventsLimited || !d.Result().Limited || len(d.Result().Declared) != 1 {
		t.Fatal("oversize lost following evidence")
	}
	s := newSample(time.Now(), 800)
	for i := 0; i < 1000; i++ {
		s.feed([]byte("x"), nil)
	}
	s.feed(nil, io.EOF)
	c := s.freeze()
	if c.Captured != 800 || c.Observed != 1000 || !c.Truncated || !c.TimingLimited || len(c.Chunks) != maxChunks {
		t.Fatal("unbounded sample", c.Captured, c.Observed, len(c.Chunks))
	}
}
func TestUnknownAndMalformedBodiesNeverLeakRedactedRaw(t *testing.T) {
	for _, data := range []string{"secret arbitrary text", `{"token":"secret-token"`, "data: arbitrary-secret\n\n"} {
		h := http.Header{"Content-Type": {"text/event-stream"}}
		if !strings.HasPrefix(data, "data:") {
			h.Set("Content-Type", "application/json")
		}
		b := analyseBody(snapshot(data), h, "redacted", modelaudit.New("", ""), true)
		encoded, _ := json.Marshal(b)
		if bytes.Contains(encoded, []byte("secret")) || len(b.Raw) > 0 {
			t.Fatal("raw fallback leaked", string(encoded))
		}
	}
}
func TestBodyCredentialRedaction(t *testing.T) {
	r := redactJSON([]byte(`{"api_key":"secret-one","input":"allowed prompt","nested":{"authorization":"secret-two","encrypted_content":"secret-three"},"headers":{"X-Private-Thing":"secret-four"}}`))
	if bytes.Contains(r, []byte("secret-")) || !bytes.Contains(r, []byte("allowed prompt")) {
		t.Fatal(string(r))
	}
}
func TestZstdAndBrotliAndExpansionLimit(t *testing.T) {
	for _, encoding := range []string{"zstd", "br"} {
		t.Run(encoding, func(t *testing.T) {
			var buf bytes.Buffer
			if encoding == "zstd" {
				w, _ := zstd.NewWriter(&buf)
				w.Write([]byte(`{"model":"luna"}`))
				w.Close()
			} else {
				w := brotli.NewWriter(&buf)
				w.Write([]byte(`{"model":"luna"}`))
				w.Close()
			}
			d := modelaudit.New("astra", "astra")
			b := analyseBody(snapshot(buf.String()), http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {encoding}}, "redacted", d, true)
			if b.Note != "" || d.Result().Verdict != "mismatch" {
				t.Fatal(b.Note, d.Result())
			}
		})
	}
	var buf bytes.Buffer
	z, _ := zstd.NewWriter(&buf)
	z.Write(bytes.Repeat([]byte("a"), decodedLimit+100))
	z.Close()
	if _, note := decodeBody(buf.Bytes(), "zstd"); note == "" {
		t.Fatal("decompression bomb accepted")
	}
}
