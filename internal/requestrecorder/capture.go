package requestrecorder

import (
	"errors"
	"io"
	"sync"
	"time"
)

const maxChunks = 512

var errIdle = errors.New("upstream stream idle timeout")

type Chunk struct {
	AtMS int64  `json:"at_ms"`
	Data []byte `json:"data_base64"`
}
type sample struct {
	mu                                    sync.Mutex
	start                                 time.Time
	limit                                 int
	chunks                                []Chunk
	observed, captured                    int64
	eof, truncated, timingLimited, frozen bool
}
type captured struct {
	Chunks                        []Chunk
	Observed, Captured            int64
	EOF, Truncated, TimingLimited bool
}

func newSample(start time.Time, limit int) *sample { return &sample{start: start, limit: limit} }
func (s *sample) feed(p []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return
	}
	s.observed += int64(len(p))
	n := min(len(p), s.limit-int(s.captured))
	if n < len(p) {
		s.truncated = true
	}
	if n > 0 {
		b := append([]byte(nil), p[:n]...)
		if len(s.chunks) < maxChunks {
			s.chunks = append(s.chunks, Chunk{time.Since(s.start).Milliseconds(), b})
		} else {
			s.timingLimited = true
			i := len(s.chunks) - 1
			s.chunks[i].Data = append(s.chunks[i].Data, b...)
		}
		s.captured += int64(n)
	}
	if errors.Is(err, io.EOF) {
		s.eof = true
	}
}
func (s *sample) freeze() captured {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	return captured{s.chunks, s.observed, s.captured, s.eof, s.truncated, s.timingLimited}
}
func (c captured) bytes() []byte {
	b := make([]byte, 0, c.Captured)
	for _, x := range c.Chunks {
		b = append(b, x.Data...)
	}
	return b
}

// tap never writes to disk or parses JSON on the forwarding path. Its retained
// prefix and timestamp count are bounded independently of the stream length.
type tap struct {
	source  io.ReadCloser
	sample  *sample
	idle    time.Duration
	once    sync.Once
	onError func(error)
}

func (b *tap) closeSource() { b.once.Do(func() { _ = b.source.Close() }) }
func (b *tap) Read(p []byte) (int, error) {
	var timer *time.Timer
	var fired chan struct{}
	if b.idle > 0 {
		fired = make(chan struct{})
		timer = time.AfterFunc(b.idle, func() { b.closeSource(); close(fired) })
	}
	n, err := b.source.Read(p)
	if timer != nil && !timer.Stop() {
		<-fired
		err = errIdle
	}
	b.sample.feed(p[:n], err)
	if err != nil && err != io.EOF && b.onError != nil {
		b.onError(err)
	}
	return n, err
}
func (b *tap) Close() error { b.closeSource(); return nil }
