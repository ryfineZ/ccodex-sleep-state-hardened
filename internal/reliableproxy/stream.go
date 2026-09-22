package reliableproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const eventLimit = 256 << 10

var errStreamIdle = errors.New("upstream stream idle timeout")

// eventParser bounds memory per event. Original bytes are never rewritten.
// Incomplete events at EOF are deliberately not dispatched.
type eventParser struct {
	line, data      []byte
	name            string
	total           int
	dropping, nonCR bool
	oversized       int
	emit            func(string, []byte)
}

func (p *eventParser) feed(chunk []byte) {
	for _, ch := range chunk {
		if ch != '\n' {
			if ch != '\r' {
				p.nonCR = true
			}
			if !p.dropping {
				p.total++
				if p.total > eventLimit {
					p.dropping = true
					p.oversized++
					p.line = nil
					p.data = nil
				} else {
					p.line = append(p.line, ch)
				}
			}
			continue
		}
		if !p.nonCR {
			if !p.dropping && len(p.data) > 0 && p.emit != nil {
				p.emit(p.name, bytes.TrimSuffix(p.data, []byte{'\n'}))
			}
			p.data = p.data[:0]
			p.name = ""
			p.total = 0
			p.dropping = false
		} else if !p.dropping {
			line := bytes.TrimSuffix(p.line, []byte{'\r'})
			field, value, ok := bytes.Cut(line, []byte{':'})
			if !ok {
				value = nil
			}
			value = bytes.TrimPrefix(value, []byte{' '})
			switch string(field) {
			case "data":
				p.data = append(p.data, value...)
				p.data = append(p.data, '\n')
			case "event":
				if len(value) <= 128 {
					p.name = string(value)
				}
			}
		}
		p.line = p.line[:0]
		p.nonCR = false
	}
}

type streamResult struct {
	complete    bool
	failed      bool
	pauseStatus int
	oversized   int
	kind        string
}

func parseEvent(name string, data []byte, result *streamResult, onState func(string), onPause func(int)) {
	var event struct {
		Type    string                     `json:"type"`
		Code    string                     `json:"code"`
		Headers map[string]json.RawMessage `json:"headers"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
		Response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	if event.Type != "" {
		name = event.Type
	}
	switch name {
	case "response.metadata":
		for key, value := range event.Headers {
			if !strings.EqualFold(key, stateHeader) {
				continue
			}
			var state string
			if json.Unmarshal(value, &state) != nil {
				var values []string
				if json.Unmarshal(value, &values) == nil && len(values) > 0 {
					state = values[0]
				}
			}
			if validState(state) && onState != nil {
				onState(state)
			}
		}
	case "response.completed":
		result.complete = true
	case "response.failed", "response.incomplete", "error":
		result.failed = true
		code := event.Response.Error.Code
		if code == "" {
			code = event.Error.Code
		}
		if code == "" {
			code = event.Code
		}
		if code == "rate_limit_exceeded" || code == "insufficient_quota" {
			result.pauseStatus = http.StatusTooManyRequests
			if onPause != nil {
				onPause(http.StatusTooManyRequests)
			}
		}
	}
}

type observedBody struct {
	idle      time.Duration
	closeOnce sync.Once
	closeErr  error
	source    io.ReadCloser
	mu        sync.Mutex
	once      sync.Once
	parser    *eventParser
	result    streamResult
	finished  bool
	onFinish  func(streamResult, error)
}

func (b *observedBody) Read(p []byte) (int, error) {
	var timer *time.Timer
	var expired chan struct{}
	if b.idle > 0 {
		expired = make(chan struct{})
		timer = time.AfterFunc(b.idle, func() { b.closeSource(); close(expired) })
	}
	n, err := b.source.Read(p)
	if timer != nil && !timer.Stop() {
		<-expired
		err = errStreamIdle
	}
	b.mu.Lock()
	if !b.finished && n > 0 && b.parser != nil {
		b.parser.feed(p[:n])
		b.result.oversized = b.parser.oversized
	}
	if err != nil {
		b.finishLocked(err)
	}
	b.mu.Unlock()
	return n, err
}
func (b *observedBody) Close() error {
	err := b.closeSource()
	b.mu.Lock()
	b.finishLocked(nil)
	b.mu.Unlock()
	return err
}
func (b *observedBody) finishLocked(err error) {
	b.once.Do(func() {
		b.finished = true
		if b.onFinish != nil {
			b.onFinish(b.result, err)
		}
	})
}

func (b *observedBody) closeSource() error {
	b.closeOnce.Do(func() { b.closeErr = b.source.Close() })
	return b.closeErr
}
