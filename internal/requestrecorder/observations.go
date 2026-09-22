package requestrecorder

import (
	"sort"
	"time"
)

// ObservationGroup is descriptive. It never changes routing or labels a proxy bad.
type ObservationGroup struct {
	Forwarded  string    `json:"forwarded_model"`
	Scope      string    `json:"credential_scope_fingerprint"`
	Route      string    `json:"configured_route_label"`
	Upstream   string    `json:"upstream_origin"`
	Model      string    `json:"requested_model"`
	Samples    int       `json:"samples"`
	Consistent int       `json:"consistent"`
	Different  int       `json:"different"`
	Conflict   int       `json:"conflict"`
	Unknown    int       `json:"unknown"`
	Limited    int       `json:"limited"`
	LastSeen   time.Time `json:"last_seen"`
}

func (s *Store) Observations() []ObservationGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	type key struct{ scope, route, upstream, model, forwarded string }
	groups := map[key]*ObservationGroup{}
	for _, r := range s.summaries {
		k := key{r.Scope, r.Route, r.Upstream, r.Requested, r.Forwarded}
		g := groups[k]
		if g == nil {
			g = &ObservationGroup{Scope: r.Scope, Route: r.Route, Upstream: r.Upstream, Model: r.Requested, Forwarded: r.Forwarded}
			groups[k] = g
		}
		g.Samples++
		if r.Limited {
			g.Limited++
		}
		if r.Started.After(g.LastSeen) {
			g.LastSeen = r.Started
		}
		switch r.Verdict {
		case "consistent":
			g.Consistent++
		case "mismatch":
			g.Different++
		case "conflict":
			g.Conflict++
		default:
			g.Unknown++
		}
	}
	out := make([]ObservationGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}
