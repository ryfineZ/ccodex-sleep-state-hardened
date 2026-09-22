// Package routehealth tracks transport health independently of node inventory.
// A lease fences late responses and permits exactly one half-open trial.
package routehealth

import (
	"sync"
	"time"
)

type Outcome uint8

const (
	Neutral Outcome = iota // Cancellation or downstream failure; no attribution.
	Success                // Complete transport, not proof of model/state quality.
	Failure                // Attributable connection/read failure.
)

type Config struct {
	Threshold     int
	Base, Maximum time.Duration
	Now           func() time.Time
	Jitter        func(time.Duration) time.Duration
}
type Status struct {
	State     string    `json:"state"`
	Failures  int       `json:"consecutive_failures"`
	OpenUntil time.Time `json:"open_until,omitempty"`
	Successes uint64    `json:"successes"`
	Errors    uint64    `json:"transport_failures"`
}
type record struct {
	Status
	epoch, sequence, lastFailure uint64
	openings                     int
}
type Manager struct {
	mu     sync.Mutex
	config Config
	routes map[string]*record
}
type Lease struct {
	manager         *Manager
	id              string
	epoch, sequence uint64
	trial           bool
	once            sync.Once
}

func New(c Config) *Manager {
	if c.Threshold < 1 {
		c.Threshold = 2
	}
	if c.Base <= 0 {
		c.Base = 30 * time.Second
	}
	if c.Maximum < c.Base {
		c.Maximum = 5 * time.Minute
		if c.Maximum < c.Base {
			c.Maximum = c.Base
		}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Manager{config: c, routes: make(map[string]*record)}
}
func (m *Manager) get(id string) *record {
	r := m.routes[id]
	if r == nil {
		r = &record{Status: Status{State: "healthy"}}
		m.routes[id] = r
	}
	return r
}
func (m *Manager) Available(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.get(id)
	return r.State != "half_open" && (r.State != "open" || !m.config.Now().Before(r.OpenUntil))
}
func (m *Manager) Begin(id string) (*Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.get(id)
	if r.State == "half_open" || (r.State == "open" && m.config.Now().Before(r.OpenUntil)) {
		return nil, false
	}
	trial := r.State == "open"
	if trial {
		r.State = "half_open"
		r.epoch++
	}
	r.sequence++
	return &Lease{manager: m, id: id, epoch: r.epoch, sequence: r.sequence, trial: trial}, true
}
func (l *Lease) Finish(outcome Outcome) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		m := l.manager
		m.mu.Lock()
		defer m.mu.Unlock()
		r := m.get(l.id)
		if l.epoch != r.epoch {
			return
		}
		switch outcome {
		case Success:
			r.Successes++
			// A response started before a newer failure cannot erase that failure.
			if l.sequence < r.lastFailure {
				return
			}
			r.State = "healthy"
			r.Failures = 0
			r.openings = 0
			r.OpenUntil = time.Time{}
		case Failure:
			r.Errors++
			r.Failures++
			r.lastFailure = max(r.lastFailure, l.sequence)
			if l.trial || r.Failures >= m.config.Threshold {
				m.open(r, true)
			} else {
				r.State = "suspect"
			}
		default:
			if l.trial {
				m.open(r, false)
			}
		}
	})
}
func (m *Manager) open(r *record, increase bool) {
	if increase {
		r.openings++
	}
	delay := m.config.Base
	for n := 1; n < r.openings && delay < m.config.Maximum; n++ {
		delay = min(delay*2, m.config.Maximum)
	}
	if m.config.Jitter != nil {
		delay = m.config.Jitter(delay)
	}
	delay = max(time.Millisecond, min(delay, m.config.Maximum))
	r.State = "open"
	r.OpenUntil = m.config.Now().Add(delay)
	r.epoch++
}
func (m *Manager) Status(id string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(id).Status
}
