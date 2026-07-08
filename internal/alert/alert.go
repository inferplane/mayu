// Package alert is the budget-alert webhook emitter (D5b, ADR-017): a leaf
// package that evaluates a team's monthly-budget utilization ratio against
// configured thresholds and fires a fire-and-forget webhook POST when a new
// threshold is crossed. It knows nothing about governance, budget storage, or
// config — Notifier.Observe is called by whatever computed the ratio (the
// governance package's Settle), and New's caller resolves the webhook URL and
// thresholds from config.
package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"
)

// recentCap bounds the in-memory ring of recent fires (GET /admin/alerts/recent,
// spec §6.7 "show alert status + recent fires") — unbounded growth would leak
// memory over the life of a long-running instance.
const recentCap = 50

// Fire is one alert delivery attempt. Error is a classification string, never
// the raw error text — the webhook URL (which may embed a Slack/SNS token)
// must never end up in this struct (it is served over the admin API).
type Fire struct {
	TS          string  `json:"ts"`
	Team        string  `json:"team"`
	Threshold   float64 `json:"threshold"`
	Ratio       float64 `json:"ratio"`
	SpentMicros int64   `json:"spent_usd_micros"`
	LimitMicros int64   `json:"limit_usd_micros"`
	Delivered   bool    `json:"delivered"`
	Error       string  `json:"error,omitempty"`
}

// Notifier evaluates budget-utilization ratios against a fixed threshold list
// and posts a JSON webhook on each newly-crossed threshold. State (fired,
// recent) is in-memory and per-instance: on a multi-replica deployment each
// instance evaluates independently, so a threshold crossing may fire once per
// replica (documented in ADR-017, same per-instance caveat as ADR-013's
// limiter/budget counters).
type Notifier struct {
	url        string
	thresholds []float64 // sorted ascending
	client     *http.Client
	wg         sync.WaitGroup // tracks in-flight deliver() goroutines, for graceful shutdown

	mu     sync.Mutex
	fired  map[string]float64 // team -> highest threshold fired in the current window
	recent []Fire             // newest first, capped at recentCap
	now    func() time.Time
}

// New builds a Notifier. thresholds need not be sorted; empty/non-positive
// entries are dropped. timeout<=0 defaults to 5s.
func New(webhookURL string, thresholds []float64, timeout time.Duration) *Notifier {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ts := make([]float64, 0, len(thresholds))
	for _, t := range thresholds {
		if t > 0 {
			ts = append(ts, t)
		}
	}
	sort.Float64s(ts)
	return &Notifier{
		url:        webhookURL,
		thresholds: ts,
		client:     &http.Client{Timeout: timeout},
		fired:      map[string]float64{},
		now:        time.Now,
	}
}

// Observe evaluates one team's post-debit spend against limit and fires the
// highest newly-crossed threshold, if any. Called synchronously from the
// governance Settle path — the ratio math is cheap; delivery happens on a
// separate goroutine so a slow/unreachable webhook never adds request latency.
func (n *Notifier) Observe(team string, spentMicros, limitMicros int64) {
	if n == nil || limitMicros <= 0 || len(n.thresholds) == 0 {
		return
	}
	ratio := float64(spentMicros) / float64(limitMicros)

	n.mu.Lock()
	prev := n.fired[team]
	// A ratio below the last-fired threshold means the budget window rolled
	// over (or the limit was raised) since we last fired — re-arm.
	// ponytail: ratio-drop heuristic instead of exposing windowEnd from
	// BudgetStore; widen the interface if a real edge case needs it.
	if ratio < prev {
		prev = 0
	}
	var crossed float64
	for _, t := range n.thresholds {
		if ratio >= t && t > prev {
			crossed = t
		}
	}
	if crossed == 0 {
		if prev == 0 {
			delete(n.fired, team) // bound map size: nothing armed, no need to remember this team
		} else {
			n.fired[team] = prev
		}
		n.mu.Unlock()
		return
	}
	n.fired[team] = crossed
	n.mu.Unlock()

	fire := Fire{
		TS:          n.now().UTC().Format(time.RFC3339Nano),
		Team:        team,
		Threshold:   crossed,
		Ratio:       ratio,
		SpentMicros: spentMicros,
		LimitMicros: limitMicros,
	}
	n.wg.Add(1)
	go n.deliver(fire)
}

// Close waits for in-flight webhook deliveries to finish (bounded by each
// delivery's own http.Client timeout), so a graceful shutdown does not
// silently abandon an alert POST spawned in the last window. Safe to call
// once; the Notifier must not be used afterward.
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.wg.Wait()
}

func (n *Notifier) deliver(fire Fire) {
	defer n.wg.Done()
	body, _ := json.Marshal(map[string]any{
		"event":            "budget_alert",
		"team":             fire.Team,
		"threshold":        fire.Threshold,
		"ratio":            fire.Ratio,
		"spent_usd_micros": fire.SpentMicros,
		"limit_usd_micros": fire.LimitMicros,
		"ts":               fire.TS,
	})
	req, err := http.NewRequest(http.MethodPost, n.url, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := n.client.Do(req)
		switch {
		case doErr != nil:
			fire.Error = classifyError(doErr)
		case resp.StatusCode >= 300:
			// Canonical, Go-controlled reason phrase — never the server's raw
			// resp.Status line (classification-string posture, ADR-017 §6).
			fire.Error = "webhook returned non-2xx: " + http.StatusText(resp.StatusCode)
		default:
			fire.Delivered = true
		}
		if resp != nil {
			io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
			resp.Body.Close()
		}
	} else {
		fire.Error = "webhook request build failed"
	}
	n.record(fire)
	if fire.Error != "" {
		fmt.Fprintf(os.Stderr, "inferplane: budget alert webhook for team %q: %s\n", fire.Team, fire.Error)
	}
}

// classifyError returns a sanitized delivery-failure classification — never
// the raw error text, which for a *url.Error embeds the destination URL (and
// the URL may carry an embedded webhook token).
func classifyError(err error) string {
	if ue, ok := err.(*url.Error); ok {
		if ue.Timeout() {
			return "webhook delivery timed out"
		}
	}
	return "webhook delivery failed"
}

func (n *Notifier) record(fire Fire) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.recent = append([]Fire{fire}, n.recent...)
	if len(n.recent) > recentCap {
		n.recent = n.recent[:recentCap]
	}
}

// Recent returns a copy of the recent-fires ring, newest first.
func (n *Notifier) Recent() []Fire {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Fire, len(n.recent))
	copy(out, n.recent)
	return out
}
