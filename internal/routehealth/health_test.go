package routehealth

import (
	"sync"
	"testing"
	"time"
)

func TestCircuitFencingRecoveryAndCancellation(t *testing.T) {
	now := time.Unix(1000, 0)
	m := New(Config{Threshold: 2, Base: time.Second, Maximum: 8 * time.Second, Now: func() time.Time { return now }})
	old, _ := m.Begin("r")
	a, _ := m.Begin("r")
	a.Finish(Failure)
	old.Finish(Success)
	if m.Status("r").Failures != 1 {
		t.Fatal("late success erased newer failure")
	}
	b, _ := m.Begin("r")
	b.Finish(Failure)
	if m.Available("r") {
		t.Fatal("open circuit admitted request")
	}
	now = now.Add(time.Second)
	trial, ok := m.Begin("r")
	if !ok {
		t.Fatal("no trial")
	}
	if _, ok = m.Begin("r"); ok {
		t.Fatal("multiple half-open trials")
	}
	trial.Finish(Neutral)
	if m.Available("r") {
		t.Fatal("cancelled trial reopened circuit")
	}
	now = now.Add(time.Second)
	trial, _ = m.Begin("r")
	trial.Finish(Failure)
	if m.Status("r").OpenUntil.Sub(now) != 2*time.Second {
		t.Fatal("backoff not exponential")
	}
	now = now.Add(2 * time.Second)
	trial, _ = m.Begin("r")
	trial.Finish(Success)
	if m.Status("r").State != "healthy" {
		t.Fatal("did not recover")
	}
	trial.Finish(Failure)
	if m.Status("r").State != "healthy" {
		t.Fatal("lease finished twice")
	}
}
func TestSingleConcurrentTrial(t *testing.T) {
	now := time.Unix(1, 0)
	m := New(Config{Threshold: 1, Base: time.Second, Now: func() time.Time { return now }})
	l, _ := m.Begin("r")
	l.Finish(Failure)
	now = now.Add(time.Second)
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			if _, ok := m.Begin("r"); ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if winners != 1 {
		t.Fatal(winners)
	}
}
