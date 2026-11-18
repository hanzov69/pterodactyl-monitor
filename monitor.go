package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	bucketSecs    = 300                 // history resolution
	retention     = 30 * 24 * time.Hour // history kept
	maxEvents     = 100
	saveEvery     = 5 * time.Minute
	noticeMaxAge  = 24 * time.Hour
	userAgent     = "pterodactyl-monitor/1.0"
	statusUp      = "up"
	statusFailing = "failing" // failing, but below the alert threshold
	statusDown    = "down"
	statusMaint   = "maintenance"
	statusUnknown = "unknown"
)

type Target struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // panel | api | node
	URL         string `json:"url"`
	NodeID      int    `json:"nodeId,omitempty"`
	Maintenance bool   `json:"maintenance,omitempty"`
}

type State struct {
	Status    string    `json:"status"`
	Since     time.Time `json:"since"`
	FailSince time.Time `json:"failSince"`
	Fails     int       `json:"fails"`
	LastCheck time.Time `json:"lastCheck"`
	LatencyMs int64     `json:"latencyMs"`
	Detail    string    `json:"detail"`
}

type Bucket struct {
	Up    int   `json:"u"`
	Total int   `json:"n"`
	MsSum int64 `json:"ms"`
}

type Event struct {
	Time        time.Time `json:"time"`
	TargetID    string    `json:"targetId"`
	Name        string    `json:"name"`
	Type        string    `json:"type"` // down | up
	Detail      string    `json:"detail"`
	DurationSec int64     `json:"durationSec,omitempty"`
}

type Notice struct {
	Time    time.Time `json:"time"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
}

type Data struct {
	Nodes   []Target                     `json:"nodes"`
	States  map[string]*State            `json:"states"`
	History map[string]map[int64]*Bucket `json:"history"`
	Events  []Event                      `json:"events"`
	Outbox  []Notice                     `json:"outbox"`
}

type result struct {
	ok     bool
	ms     int64
	detail string
}

type Monitor struct {
	cfg      Config
	secure   *http.Client
	insecure *http.Client

	mu        sync.Mutex
	d         Data
	tokens    map[int]string // wings daemon tokens, memory only
	lastCycle time.Time
	started   time.Time
}

func NewMonitor(cfg Config) *Monitor {
	mk := func(skipVerify bool) *http.Client {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: skipVerify}
		tr.DisableKeepAlives = true // every check exercises a fresh connection
		return &http.Client{Transport: tr, Timeout: cfg.Timeout}
	}
	m := &Monitor{
		cfg:      cfg,
		secure:   mk(false),
		insecure: mk(true),
		tokens:   map[int]string{},
		started:  time.Now(),
		d: Data{
			States:  map[string]*State{},
			History: map[string]map[int64]*Bucket{},
		},
	}
	m.load()
	return m
}

func (m *Monitor) dataFile() string { return filepath.Join(m.cfg.DataDir, "state.json") }

func (m *Monitor) load() {
	b, err := os.ReadFile(m.dataFile())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Fatalf("reading %s: %v", m.dataFile(), err)
	}
	var d Data
	if err := json.Unmarshal(b, &d); err != nil {
		bad := m.dataFile() + ".corrupt"
		os.Rename(m.dataFile(), bad)
		log.Printf("state file unreadable (%v); moved to %s and starting fresh", err, bad)
		return
	}
	if d.States == nil {
		d.States = map[string]*State{}
	}
	if d.History == nil {
		d.History = map[string]map[int64]*Bucket{}
	}
	m.d = d
}

func (m *Monitor) Save() {
	m.mu.Lock()
	b, err := json.Marshal(&m.d)
	m.mu.Unlock()
	if err == nil {
		tmp := m.dataFile() + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, m.dataFile())
		}
	}
	if err != nil {
		log.Printf("saving state: %v", err)
	}
}

func (m *Monitor) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := m.lastCycle
	if last.IsZero() {
		last = m.started
	}
	return time.Since(last) < 3*m.cfg.Interval+3*m.cfg.Timeout
}

func (m *Monitor) Run(ctx context.Context) {
	tick := time.NewTicker(m.cfg.Interval)
	defer tick.Stop()
	lastSave := time.Now()
	for {
		m.cycle(ctx)
		if time.Since(lastSave) >= saveEvery {
			m.prune()
			m.Save()
			lastSave = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// staticTargets are the panel endpoints that come from configuration.
func (m *Monitor) staticTargets() []Target {
	var ts []Target
	if m.cfg.PublicURL != "" {
		ts = append(ts, Target{ID: "panel-public", Name: "Panel (public)", Kind: "panel", URL: m.cfg.PublicURL})
	}
	if m.cfg.LANURL != "" {
		ts = append(ts, Target{ID: "panel-lan", Name: "Panel (LAN)", Kind: "panel", URL: m.cfg.LANURL})
	}
	if m.cfg.APIKey != "" {
		ts = append(ts, Target{ID: "panel-api", Name: "Panel API", Kind: "api", URL: m.cfg.APIURL})
	}
	return ts
}

func (m *Monitor) cycle(ctx context.Context) {
	results := map[string]result{}
	var rmu sync.Mutex
	var wg sync.WaitGroup
	run := func(t Target, fn func() result) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := fn()
			rmu.Lock()
			results[t.ID] = r
			rmu.Unlock()
		}()
	}

	static := m.staticTargets()
	for _, t := range static {
		switch t.ID {
		case "panel-public":
			run(t, func() result { return m.checkPanel(ctx, t.URL, false) })
		case "panel-lan":
			run(t, func() result { return m.checkPanel(ctx, t.URL, m.cfg.LANInsecure) })
		case "panel-api":
			// Synchronous: a successful listing refreshes the node set checked below.
			nodes, r := m.listNodes(ctx)
			results[t.ID] = r
			if r.ok {
				m.mu.Lock()
				m.d.Nodes = nodes
				m.mu.Unlock()
			}
		}
	}

	m.mu.Lock()
	nodes := append([]Target(nil), m.d.Nodes...)
	m.mu.Unlock()
	for _, n := range nodes {
		if !n.Maintenance {
			run(n, func() result { return m.checkNode(ctx, n) })
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return // shutting down; don't record aborted checks as failures
	}

	now := time.Now()
	m.mu.Lock()
	active := map[string]bool{}
	for _, t := range append(static, nodes...) {
		active[t.ID] = true
		if t.Maintenance {
			m.setMaintenance(t, now)
		} else {
			m.apply(t, results[t.ID], now)
		}
	}
	for id := range m.d.States { // nodes deleted from the panel
		if !active[id] {
			delete(m.d.States, id)
		}
	}
	m.lastCycle = now
	m.mu.Unlock()

	m.flushOutbox()
}

func (m *Monitor) state(id string, now time.Time) *State {
	st := m.d.States[id]
	if st == nil {
		st = &State{Status: statusUnknown, Since: now}
		m.d.States[id] = st
	}
	return st
}

func (m *Monitor) setMaintenance(t Target, now time.Time) {
	st := m.state(t.ID, now)
	if st.Status != statusMaint {
		*st = State{Status: statusMaint, Since: now, Detail: "maintenance mode enabled in panel"}
	}
}

// apply records one check result and handles status transitions. Caller holds m.mu.
func (m *Monitor) apply(t Target, r result, now time.Time) {
	st := m.state(t.ID, now)
	st.LastCheck, st.LatencyMs, st.Detail = now, r.ms, r.detail

	h := m.d.History[t.ID]
	if h == nil {
		h = map[int64]*Bucket{}
		m.d.History[t.ID] = h
	}
	key := now.Unix() / bucketSecs * bucketSecs
	b := h[key]
	if b == nil {
		b = &Bucket{}
		h[key] = b
	}
	b.Total++
	if r.ok {
		b.Up++
		b.MsSum += r.ms
	}

	prev, next := st.Status, statusUp
	if r.ok {
		st.Fails = 0
	} else {
		st.Fails++
		if st.Fails == 1 {
			st.FailSince = now
		}
		next = statusFailing
		if prev == statusDown || st.Fails >= m.cfg.FailThreshold {
			next = statusDown
		}
	}
	if next == prev {
		return
	}
	st.Status = next
	switch {
	case next == statusDown:
		st.Since = st.FailSince
		m.event(Event{Time: now, TargetID: t.ID, Name: t.Name, Type: "down", Detail: r.detail})
		m.notify(now, "DOWN: "+t.Name, fmt.Sprintf(
			"%s is DOWN\n\nURL:    %s\nSince:  %s\nReason: %s\nFailed checks in a row: %d\n",
			t.Name, t.URL, fmtTime(st.Since), r.detail, st.Fails))
	case next == statusUp && prev == statusDown:
		dur := now.Sub(st.Since).Round(time.Second)
		m.event(Event{Time: now, TargetID: t.ID, Name: t.Name, Type: "up", Detail: r.detail, DurationSec: int64(dur.Seconds())})
		m.notify(now, "RECOVERED: "+t.Name, fmt.Sprintf(
			"%s has RECOVERED\n\nURL:       %s\nDown from: %s\nUp again:  %s\nDowntime:  %s\n",
			t.Name, t.URL, fmtTime(st.Since), fmtTime(now), dur))
		st.Since = now
	case next == statusUp && prev != statusFailing:
		st.Since = now
	}
}

func fmtTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") }

func (m *Monitor) event(e Event) {
	log.Printf("%s: %s (%s)", strings.ToUpper(e.Type), e.Name, e.Detail)
	m.d.Events = append(m.d.Events, e)
	if n := len(m.d.Events); n > maxEvents {
		m.d.Events = m.d.Events[n-maxEvents:]
	}
}

func (m *Monitor) notify(now time.Time, subject, body string) {
	if m.cfg.SMTP.Enabled() {
		m.d.Outbox = append(m.d.Outbox, Notice{Time: now, Subject: subject, Body: body})
	}
}

// flushOutbox sends all pending notices as one email. On failure they stay
// queued and are retried next cycle, until they are too old to matter.
func (m *Monitor) flushOutbox() {
	m.mu.Lock()
	var pending []Notice
	for _, n := range m.d.Outbox {
		if time.Since(n.Time) < noticeMaxAge {
			pending = append(pending, n)
		}
	}
	m.d.Outbox = pending
	m.mu.Unlock()
	if len(pending) == 0 {
		return
	}

	subject := pending[0].Subject
	if len(pending) > 1 {
		subject = fmt.Sprintf("%d status changes", len(pending))
	}
	var body strings.Builder
	for i, n := range pending {
		if i > 0 {
			body.WriteString("\n----------------------------------------\n\n")
		}
		body.WriteString(n.Body)
	}
	if m.cfg.DashboardURL != "" {
		body.WriteString("\nDashboard: " + m.cfg.DashboardURL + "\n")
	}
	if err := m.cfg.SMTP.Send("[Pterodactyl] "+subject, body.String()); err != nil {
		log.Printf("sending email failed (will retry): %v", err)
		return
	}
	log.Printf("email sent: %s", subject)
	m.mu.Lock()
	m.d.Outbox = m.d.Outbox[len(pending):]
	m.mu.Unlock()
	m.Save()
}

func (m *Monitor) prune() {
	cutoff := time.Now().Add(-retention).Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, h := range m.d.History {
		for k := range h {
			if k < cutoff {
				delete(h, k)
			}
		}
		if len(h) == 0 {
			delete(m.d.History, id)
		}
	}
}

// ---- checks ----

func (m *Monitor) get(ctx context.Context, skipVerify bool, rawURL string, hdr map[string]string) (int, []byte, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	cl := m.secure
	if skipVerify {
		cl = m.insecure
	}
	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, time.Since(start).Milliseconds(), err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, time.Since(start).Milliseconds(), nil
}

func errDetail(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timed out"
		}
		err = ue.Err
	}
	return err.Error()
}

func (m *Monitor) checkPanel(ctx context.Context, u string, skipVerify bool) result {
	code, _, ms, err := m.get(ctx, skipVerify, u, nil)
	if err != nil {
		return result{false, ms, errDetail(err)}
	}
	return result{code < 400, ms, fmt.Sprintf("HTTP %d", code)}
}

func (m *Monitor) apiHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + m.cfg.APIKey, "Accept": "application/json"}
}

func (m *Monitor) listNodes(ctx context.Context) ([]Target, result) {
	var nodes []Target
	var totalMs int64
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/application/nodes?per_page=100&page=%d", m.cfg.APIURL, page)
		code, body, ms, err := m.get(ctx, m.cfg.LANInsecure, u, m.apiHeaders())
		totalMs += ms
		if err != nil {
			return nil, result{false, totalMs, errDetail(err)}
		}
		if code != http.StatusOK {
			hint := ""
			if code == 401 || code == 403 {
				hint = " (check PANEL_API_KEY: needs an Application API key with Nodes: Read)"
			}
			return nil, result{false, totalMs, fmt.Sprintf("HTTP %d%s", code, hint)}
		}
		var resp struct {
			Data []struct {
				Attributes struct {
					ID          int    `json:"id"`
					Name        string `json:"name"`
					FQDN        string `json:"fqdn"`
					Scheme      string `json:"scheme"`
					Listen      int    `json:"daemon_listen"`
					Maintenance bool   `json:"maintenance_mode"`
				} `json:"attributes"`
			} `json:"data"`
			Meta struct {
				Pagination struct {
					TotalPages int `json:"total_pages"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, result{false, totalMs, "unexpected API response: " + err.Error()}
		}
		for _, d := range resp.Data {
			a := d.Attributes
			nodes = append(nodes, Target{
				ID:          fmt.Sprintf("node-%d", a.ID),
				Name:        "Node: " + a.Name,
				Kind:        "node",
				URL:         fmt.Sprintf("%s://%s:%d", a.Scheme, a.FQDN, a.Listen),
				NodeID:      a.ID,
				Maintenance: a.Maintenance,
			})
		}
		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, result{true, totalMs, fmt.Sprintf("%d node(s) listed", len(nodes))}
}

// nodeToken returns the Wings daemon token for a node (cached), or "" if the
// API key isn't allowed to read it. Without a token we can still tell that
// Wings is alive, just not query its version.
func (m *Monitor) nodeToken(ctx context.Context, nodeID int) string {
	m.mu.Lock()
	tok, ok := m.tokens[nodeID]
	m.mu.Unlock()
	if ok {
		return tok
	}
	u := fmt.Sprintf("%s/api/application/nodes/%d/configuration", m.cfg.APIURL, nodeID)
	code, body, _, err := m.get(ctx, m.cfg.LANInsecure, u, m.apiHeaders())
	if err != nil || code != http.StatusOK {
		return ""
	}
	var c struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &c)
	m.mu.Lock()
	m.tokens[nodeID] = c.Token
	m.mu.Unlock()
	return c.Token
}

func (m *Monitor) checkNode(ctx context.Context, n Target) result {
	hdr := map[string]string{"Accept": "application/json"}
	tok := m.nodeToken(ctx, n.NodeID)
	if tok != "" {
		hdr["Authorization"] = "Bearer " + tok
	}
	code, body, ms, err := m.get(ctx, m.cfg.NodeInsecure, n.URL+"/api/system", hdr)
	if err != nil {
		return result{false, ms, errDetail(err)}
	}
	switch {
	case code == http.StatusOK:
		var sys struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &sys) == nil && sys.Version != "" {
			return result{true, ms, "Wings " + sys.Version}
		}
		return result{true, ms, "Wings responding"}
	case code == 401 || code == 403:
		if tok != "" { // token rotated; refetch next cycle
			m.mu.Lock()
			delete(m.tokens, n.NodeID)
			m.mu.Unlock()
		}
		return result{true, ms, "Wings responding"}
	default:
		return result{false, ms, fmt.Sprintf("HTTP %d from Wings", code)}
	}
}

// ---- dashboard snapshot ----

type Cell struct {
	T     int64  `json:"t"`
	Up    int    `json:"up"`
	Total int    `json:"total"`
	Ms    *int64 `json:"ms"`
}

type TargetView struct {
	Target
	State
	Uptime map[string]*float64 `json:"uptime"`
	Cells  []Cell              `json:"cells"`
}

type Snapshot struct {
	Generated   time.Time    `json:"generated"`
	LastCycle   time.Time    `json:"lastCycle"`
	IntervalSec int          `json:"intervalSec"`
	Range       string       `json:"range"`
	CellSec     int64        `json:"cellSec"`
	Targets     []TargetView `json:"targets"`
	Events      []Event      `json:"events"`
	EmailOn     bool         `json:"emailEnabled"`
}

var ranges = map[string]struct {
	span    time.Duration
	cellSec int64
}{
	"24h": {24 * time.Hour, 15 * 60},          // 96 cells
	"7d":  {7 * 24 * time.Hour, 2 * 60 * 60},  // 84 cells
	"30d": {30 * 24 * time.Hour, 8 * 60 * 60}, // 90 cells
}

func (m *Monitor) Snapshot(rng string) Snapshot {
	r, ok := ranges[rng]
	if !ok {
		rng, r = "24h", ranges["24h"]
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Snapshot{
		Generated: now, LastCycle: m.lastCycle, IntervalSec: int(m.cfg.Interval.Seconds()),
		Range: rng, CellSec: r.cellSec, EmailOn: m.cfg.SMTP.Enabled(),
		Targets: []TargetView{}, Events: []Event{},
	}
	for i := len(m.d.Events) - 1; i >= 0; i-- {
		s.Events = append(s.Events, m.d.Events[i])
	}

	nCells := int64(r.span.Seconds()) / r.cellSec
	end := (now.Unix()/r.cellSec + 1) * r.cellSec
	start := end - nCells*r.cellSec

	for _, t := range append(m.staticTargets(), m.d.Nodes...) {
		st := m.d.States[t.ID]
		if st == nil {
			st = &State{Status: statusUnknown, Since: now}
		}
		v := TargetView{Target: t, State: *st, Uptime: map[string]*float64{}, Cells: make([]Cell, nCells)}
		for i := range v.Cells {
			v.Cells[i].T = start + int64(i)*r.cellSec
		}
		sums := map[string]*[2]int{"24h": {}, "7d": {}, "30d": {}}
		msSum := make([]int64, nCells)
		for k, b := range m.d.History[t.ID] {
			for name, rr := range ranges {
				if k >= now.Add(-rr.span).Unix() {
					sums[name][0] += b.Up
					sums[name][1] += b.Total
				}
			}
			if k >= start && k < end {
				i := (k - start) / r.cellSec
				v.Cells[i].Up += b.Up
				v.Cells[i].Total += b.Total
				msSum[i] += b.MsSum
			}
		}
		for i := range v.Cells {
			if v.Cells[i].Up > 0 {
				avg := msSum[i] / int64(v.Cells[i].Up)
				v.Cells[i].Ms = &avg
			}
		}
		for name, sum := range sums {
			if sum[1] > 0 {
				pct := 100 * float64(sum[0]) / float64(sum[1])
				v.Uptime[name] = &pct
			} else {
				v.Uptime[name] = nil
			}
		}
		s.Targets = append(s.Targets, v)
	}
	return s
}
