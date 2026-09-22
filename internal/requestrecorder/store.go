package requestrecorder

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/modelaudit"
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

const recordFileLimit = 96 << 20

type Record struct {
	IncomingRequest *Body              `json:"incoming_request_body,omitempty"`
	ModelOverride   ModelOverrideAudit `json:"model_override"`
	Context         ProtocolContext    `json:"protocol_context"`
	Quota           []QuotaWindow      `json:"quota_observations,omitempty"`
	QuotaLimited    bool               `json:"quota_observations_limited"`
	Schema          int                `json:"schema_version"`
	ID              string             `json:"id"`
	Started         time.Time          `json:"started_at"`
	DurationMS      int64              `json:"duration_ms"`
	HeadersMS       int64              `json:"response_headers_ms"`
	Mode            string             `json:"mode"`
	Method          string             `json:"method"`
	URI             string             `json:"uri"`
	Upstream        string             `json:"upstream_origin"`
	OutgoingURI     string             `json:"outgoing_uri"`
	IncomingHeaders http.Header        `json:"incoming_headers"`
	RequestHeaders  http.Header        `json:"outgoing_headers"`
	ResponseHeaders http.Header        `json:"upstream_response_headers"`
	Status          int                `json:"upstream_status"`
	ClientStatus    int                `json:"client_status"`
	Outcome         string             `json:"outcome"`
	Request         Body               `json:"request_body"`
	Response        Body               `json:"response_body"`
	Model           modelaudit.Result  `json:"model_detection"`
}
type pending struct {
	incoming          *captured
	record            Record
	request, response captured
}

func (p pending) analyse() Record {
	r := p.record
	data, note := decodeBody(p.request.bytes(), r.RequestHeaders.Get("Content-Encoding"))
	forwarded := modelaudit.RequestModel(data)
	requested := forwarded
	if p.incoming != nil {
		original, _ := decodeBody(p.incoming.bytes(), r.IncomingHeaders.Get("Content-Encoding"))
		requested = modelaudit.RequestModel(original)
		body := analyseBody(*p.incoming, r.IncomingHeaders, r.Mode, nil, false)
		r.IncomingRequest = &body
	}
	if r.ModelOverride.Applied {
		// These names were parsed while preparing the exact outgoing body, even
		// when the retained body prefix is too short for later JSON analysis.
		requested = r.ModelOverride.OriginalModel
		forwarded = r.ModelOverride.ForwardedModel
	}
	d := modelaudit.New(requested, forwarded)
	if note != "" || p.request.Truncated || !p.request.EOF {
		d.Limited()
	}
	d.Headers(r.ResponseHeaders, "response_header", 0, r.HeadersMS)
	r.Request = analyseBody(p.request, r.RequestHeaders, r.Mode, nil, false)
	q := &quotaCollector{}
	q.Headers(r.ResponseHeaders, "response_header", 0, r.HeadersMS)
	r.Response = analyseBody(p.response, r.ResponseHeaders, r.Mode, d, true, q.Event)
	r.Quota, r.QuotaLimited = q.Windows, q.Limited
	r.Model = d.Result()
	if r.Outcome == "http_complete" {
		switch {
		case r.Status >= 400:
			r.Outcome = "upstream_http_error"
		case r.Response.StreamOutcome == "failed":
			r.Outcome = "upstream_stream_error"
		case r.Response.StreamOutcome == "incomplete":
			r.Outcome = "incomplete_stream"
		case r.Response.StreamOutcome == "observation_limited":
			r.Outcome = "observation_limited"
		}
	}
	full := r.Mode == "full"
	r.IncomingHeaders = redactHeaders(r.IncomingHeaders, full)
	r.RequestHeaders = redactHeaders(r.RequestHeaders, full)
	r.ResponseHeaders = redactHeaders(r.ResponseHeaders, full)
	return r
}

type Summary struct {
	Scope      string        `json:"credential_scope_fingerprint"`
	Route      string        `json:"configured_route_label"`
	Upstream   string        `json:"upstream_origin"`
	Forwarded  string        `json:"forwarded_model,omitempty"`
	HeadersMS  int64         `json:"response_headers_ms"`
	Quota      []QuotaWindow `json:"quota_observations,omitempty"`
	ID         string        `json:"id"`
	Started    time.Time     `json:"started_at"`
	Method     string        `json:"method"`
	URI        string        `json:"uri"`
	Status     int           `json:"status"`
	Outcome    string        `json:"outcome"`
	DurationMS int64         `json:"duration_ms"`
	Requested  string        `json:"requested_model,omitempty"`
	Declared   []string      `json:"declared_models"`
	Verdict    string        `json:"model_verdict"`
	Limited    bool          `json:"evidence_limited"`
}

func summarize(r Record) Summary {
	return Summary{ID: r.ID, Started: r.Started, Method: r.Method, URI: r.URI, Status: r.ClientStatus, Outcome: r.Outcome, DurationMS: r.DurationMS, Requested: r.Model.Requested, Declared: r.Model.Declared, Verdict: r.Model.Verdict, Limited: r.Model.Limited, Scope: r.Context.Scope, Route: r.Context.Route, Upstream: r.Upstream, Forwarded: r.Model.Forwarded, HeadersMS: r.HeadersMS, Quota: r.Quota}
}

type Store struct {
	root                                                *os.Root
	config                                              Config
	queue                                               chan pending
	done                                                chan struct{}
	lifecycle                                           sync.RWMutex
	closed                                              bool
	mu                                                  sync.Mutex
	summaries                                           map[string]Summary
	bytes                                               int64
	saved, dropped, queueDrops, quotaDrops, writeErrors atomic.Uint64
}

func NewStore(c Config) (*Store, error) {
	root, err := openRecordingRoot(c.Directory)
	if err != nil {
		return nil, err
	}
	s := &Store{root: root, config: c, queue: make(chan pending, c.QueueSize), done: make(chan struct{}), summaries: map[string]Summary{}}
	entries, err := recordingEntries(root)
	if err != nil {
		root.Close()
		return nil, err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			s.bytes += info.Size()
		}
		if entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !idPattern.MatchString(id) {
			continue
		}
		r, err := s.Read(id)
		if err == nil {
			s.summaries[id] = summarize(r)
		}
		if len(s.summaries) > 10000 {
			root.Close()
			return nil, errors.New("recording directory index exceeds limit")
		}
	}
	go s.run()
	return s, nil
}
func (s *Store) Submit(p pending) bool {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		s.dropped.Add(1)
		return false
	}
	select {
	case s.queue <- p:
		return true
	default:
		s.queueDrops.Add(1)
		s.dropped.Add(1)
		return false
	}
}
func (s *Store) run() {
	defer close(s.done)
	for p := range s.queue {
		s.save(p.analyse())
	}
}
func (s *Store) save(r Record) {
	data, err := json.Marshal(r)
	if err != nil {
		s.writeErrors.Add(1)
		s.dropped.Add(1)
		return
	}
	data = append(data, '\n')
	if len(data) > recordFileLimit {
		s.quotaDrops.Add(1)
		s.dropped.Add(1)
		return
	}
	s.mu.Lock()
	quota := s.bytes+int64(len(data)) > int64(s.config.DiskMiB)<<20 || len(s.summaries) >= s.config.MaxRecords
	s.mu.Unlock()
	if quota {
		s.quotaDrops.Add(1)
		s.dropped.Add(1)
		return
	}
	if !idPattern.MatchString(r.ID) {
		s.writeErrors.Add(1)
		s.dropped.Add(1)
		return
	}
	tmp, name := r.ID+".part", r.ID+".json"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		// Check the open handle before writing any sensitive bytes. New Windows
		// files inherit the verified directory DACL; a changed ACL fails closed.
		err = checkPrivateHandle(f, false)
		if err == nil {
			_, err = f.Write(data)
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		// Link publishes a complete immutable file atomically and NEVER overwrites
		// an existing record or follows a user-provided destination symlink.
		if err == nil {
			err = s.root.Link(tmp, name)
		}
		_ = s.root.Remove(tmp)
	}
	if err != nil {
		s.writeErrors.Add(1)
		s.dropped.Add(1)
		return
	}
	s.mu.Lock()
	s.bytes += int64(len(data))
	s.summaries[r.ID] = summarize(r)
	s.mu.Unlock()
	s.saved.Add(1)
}
func (s *Store) Read(id string) (Record, error) {
	var r Record
	if !idPattern.MatchString(id) {
		return r, errors.New("invalid recording id")
	}
	name := id + ".json"
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > recordFileLimit {
		return r, errors.New("record unavailable")
	}
	f, err := s.root.Open(name)
	if err != nil {
		return r, errors.New("record unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || checkPrivateHandle(f, false) != nil {
		return r, errors.New("record unavailable or no longer private")
	}
	d := json.NewDecoder(io.LimitReader(f, recordFileLimit+1))
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF || r.ID != id || r.Schema != 1 {
		return r, errors.New("invalid record")
	}
	return r, nil
}
func (s *Store) List(limit int) []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(s.summaries))
	for _, x := range s.summaries {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	if limit < 1 || limit > 200 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
func (s *Store) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"saved_this_run": s.saved.Load(), "records": len(s.summaries), "disk_bytes": s.bytes, "disk_quota_bytes": int64(s.config.DiskMiB) << 20, "queue_depth": len(s.queue), "dropped": s.dropped.Load(), "queue_drops": s.queueDrops.Load(), "quota_drops": s.quotaDrops.Load(), "write_errors": s.writeErrors.Load()}
}
func (s *Store) Close() {
	s.lifecycle.Lock()
	if s.closed {
		s.lifecycle.Unlock()
		<-s.done
		return
	}
	s.closed = true
	close(s.queue)
	s.lifecycle.Unlock()
	<-s.done
	_ = s.root.Close()
}
