package reliableproxy

import (
	"strings"
	"testing"
)

func TestFragmentedSSEOversizeResynchronization(t *testing.T) {
	result := streamResult{}
	p := &eventParser{emit: func(name string, b []byte) { parseEvent(name, b, &result, nil, nil) }}
	wire := "data: " + strings.Repeat("x", eventLimit*3) + "\r\n\r\n" + doneEvent
	for i := 0; i < len(wire); i++ {
		p.feed([]byte(wire[i : i+1]))
	}
	if p.oversized != 1 || !result.complete || len(p.line) > eventLimit || len(p.data) > eventLimit {
		t.Fatal("unbounded memory or terminal lost")
	}
}
func TestErrorIsNotClearedByLaterCompletion(t *testing.T) {
	r := streamResult{}
	parseEvent("error", []byte(`{"code":"rate_limit_exceeded"}`), &r, nil, nil)
	parseEvent("response.completed", []byte(`{}`), &r, nil, nil)
	if !r.failed || r.pauseStatus != 429 {
		t.Fatal(r)
	}
}
