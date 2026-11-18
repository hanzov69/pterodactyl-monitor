package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	bucketSecs     = 300                 // history resolution
	retention      = 30 * 24 * time.Hour // history kept
	maxEvents      = 100
	saveEvery      = 5 * time.Minute
	noticeMaxAge   = 24 * time.Hour
	maxConcurrent  = 8
	userAgent      = "pterodactyl-monitor/1.0"
	statusUp       = "up"
	statusFailing  = "failing"  // failing, but below the alert threshold
	statusStarting = "starting" // server is starting or stopping
	statusDown     = "down"
	statusMaint    = "maintenance"
	statusUnknown  = "unknown"
)

// Probe is a service-level check run against a server that the panel reports as running.
type Probe struct {
	Type string `json:"type"`           // http | tcp | a2s | minecraft | raknet | palworld
	URL  string `json:"url,omitempty"`  // http
	Host string `json:"host,omitempty"` // everything else
	Port int    `json:"port,omitempty"`

	// Password is the RCON password for a palworld check, read from the
	// panel each cycle. It is deliberately never written to disk or to the
	// dashboard API.
	Password string `json:"-"`
}

func (p *Probe) String() string {
	scheme := ""
	switch {
	case p == nil:
		return ""
	case p.Type == "http":
		return p.URL
	case p.Type == "palworld":
		scheme = "rcon"
	default:
		scheme = p.Type
	}
	return scheme + "://" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

type Target struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // panel | api | node | server
	URL         string `json:"url"`
	NodeID      int    `json:"nodeId,omitempty"`
	Maintenance bool   `json:"maintenance,omitempty"`

	// Servers only.
	Identifier string   `json:"identifier,omitempty"` // panel short id
	Node       string   `json:"node,omitempty"`
	Idle       string   `json:"idle,omitempty"` // suspended, installing, ...: shown, never checked
	Probe      *Probe   `json:"probe,omitempty"`
	Tags       []string `json:"tags,omitempty"` // settings worth showing, e.g. "crossplay on"
	Note       string   `json:"note,omitempty"` // why a server has no service check, when that needs saying
}

// Label is the name used in emails and the event log.
func (t Target) Label() string {
	if t.Kind == "server" {
		return "Server: " + t.Name
	}
	return t.Name
}

type State struct {
	Status       string    `json:"status"`
	Since        time.Time `json:"since"`
	FailSince    time.Time `json:"failSince"`
	PendingSince time.Time `json:"pendingSince"`
	Fails        int       `json:"fails"`
	LastCheck    time.Time `json:"lastCheck"`
	LatencyMs    int64     `json:"latencyMs"`
	Detail       string    `json:"detail"`
	Power        string    `json:"power,omitempty"` // servers: running | starting | stopping | offline
	Stale        bool      `json:"stale,omitempty"` // last check could not determine anything
	SeenUp       bool      `json:"seenUp,omitempty"`
	Announced    bool      `json:"announced,omitempty"` // current outage is in the event log
	Notified     bool      `json:"notified,omitempty"`  // current outage was emailed

	// A server's service check only counts against it once that exact check
	// has passed at least once; see checkServer.
	ProbeKey       string `json:"probeKey,omitempty"`
	ProbeConfirmed bool   `json:"probeConfirmed,omitempty"`

	// Expiry of the TLS certificate the target presented on its last check
	// (panel and node checks over https), and the expiry we last warned about.
	CertExpiry time.Time `json:"certExpiry,omitzero"`
	CertWarned time.Time `json:"certWarned,omitzero"`
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
	Servers []Target                     `json:"servers"`
	States  map[string]*State            `json:"states"`
	History map[string]map[int64]*Bucket `json:"history"`
	Events  []Event                      `json:"events"`
	Outbox  []Notice                     `json:"outbox"`
	Muted   map[string]bool              `json:"muted"` // targets with email alerts switched off
}

type outcome int

const (
	resSkip    outcome = iota // nothing could be determined; leave status alone
	resOK                     // healthy
	resFail                   // unhealthy
	resPending                // server starting/stopping: not up, but not yet a failure
)

type result struct {
	kind        outcome
	ms          int64
	detail      string
	power       string
	probePassed bool      // the server's service check ran and passed
	certExpiry  time.Time // expiry of the TLS certificate presented, if any
}

func okResult(ms int64, detail string) result   { return result{kind: resOK, ms: ms, detail: detail} }
func failResult(ms int64, detail string) result { return result{kind: resFail, ms: ms, detail: detail} }

type Monitor struct {
	cfg      Config
	secure   *http.Client
	insecure *http.Client

	mu        sync.Mutex
	d         Data
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
		started:  time.Now(),
	}
	m.load()
	return m
}

func (m *Monitor) dataFile() string { return filepath.Join(m.cfg.DataDir, "state.json") }

func (m *Monitor) load() {
	defer func() {
		if m.d.States == nil {
			m.d.States = map[string]*State{}
		}
		if m.d.History == nil {
			m.d.History = map[string]map[int64]*Bucket{}
		}
		if m.d.Muted == nil {
			m.d.Muted = map[string]bool{}
		}
	}()
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
	return time.Since(last) < 3*m.cfg.Interval+6*m.cfg.Timeout
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
	if m.cfg.ClientKey != "" {
		ts = append(ts, Target{ID: "panel-client-api", Name: "Panel API (servers)", Kind: "api", URL: m.cfg.APIURL})
	}
	return ts
}

func (m *Monitor) cycle(ctx context.Context) {
	results := map[string]result{}
	var rmu sync.Mutex
	set := func(id string, r result) {
		rmu.Lock()
		results[id] = r
		rmu.Unlock()
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)
	run := func(id string, fn func() result) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			set(id, fn())
		}()
	}

	static := m.staticTargets()
	for _, t := range static {
		switch t.ID {
		case "panel-public":
			run(t.ID, func() result { return m.checkPanel(ctx, t.URL, false) })
		case "panel-lan":
			run(t.ID, func() result { return m.checkPanel(ctx, t.URL, m.cfg.LANInsecure) })
		case "panel-api":
			// Synchronous: a successful listing refreshes the node set checked below.
			nodes, r := m.listNodes(ctx)
			set(t.ID, r)
			if r.kind == resOK {
				m.mu.Lock()
				m.d.Nodes = nodes
				m.mu.Unlock()
			}
		case "panel-client-api":
			servers, r := m.listServers(ctx)
			set(t.ID, r)
			if r.kind == resOK {
				m.mu.Lock()
				m.d.Servers = servers
				m.mu.Unlock()
			}
		}
	}

	// If a listing failed, the last known nodes/servers keep being checked.
	m.mu.Lock()
	if m.cfg.APIKey == "" {
		m.d.Nodes = nil
	}
	if m.cfg.ClientKey == "" {
		m.d.Servers = nil
	}
	dynamic := append(append([]Target(nil), m.d.Nodes...), m.d.Servers...)
	m.mu.Unlock()
	for _, t := range dynamic {
		switch {
		case t.Maintenance || t.Idle != "":
		case t.Kind == "node":
			run(t.ID, func() result { return m.checkNode(ctx, t) })
		case t.Kind == "server":
			run(t.ID, func() result { return m.checkServer(ctx, t) })
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return // shutting down; don't record aborted checks as failures
	}

	now := time.Now()
	m.mu.Lock()
	active := map[string]bool{}
	for _, t := range append(static, dynamic...) {
		active[t.ID] = true
		switch {
		case t.Maintenance:
			m.setIdle(t, statusMaint, "maintenance mode enabled in panel", now)
		case t.Idle != "":
			m.setIdle(t, t.Idle, "server is "+strings.ReplaceAll(t.Idle, "_", " "), now)
		default:
			m.apply(t, results[t.ID], now)
		}
	}
	for id := range m.d.States { // nodes/servers deleted from the panel
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

// setIdle parks a target that is deliberately out of service (node in
// maintenance, suspended server): shown on the dashboard, never alerted on.
func (m *Monitor) setIdle(t Target, status, detail string, now time.Time) {
	st := m.state(t.ID, now)
	if st.Status != status {
		*st = State{Status: status, Since: now, Detail: detail}
	}
}

// apply records one check result and handles status transitions. Caller holds m.mu.
func (m *Monitor) apply(t Target, r result, now time.Time) {
	st := m.state(t.ID, now)
	st.LastCheck, st.Detail, st.Power = now, r.detail, r.power
	st.Stale = r.kind == resSkip
	if key := t.Probe.String(); st.ProbeKey != key { // check added, removed or pointed elsewhere
		st.ProbeKey, st.ProbeConfirmed = key, false
	}
	if r.probePassed {
		st.ProbeConfirmed = true
	}
	if r.kind == resSkip {
		return
	}
	st.LatencyMs = r.ms
	m.trackCert(t, st, r, now)

	// A server may sit in "starting" for a while; past the grace period that is a failure.
	if r.kind == resPending {
		if st.PendingSince.IsZero() {
			st.PendingSince = now
		}
		if d := now.Sub(st.PendingSince); d > m.cfg.StartGrace {
			r.kind = resFail
			r.detail = fmt.Sprintf("stuck %s for %s", r.power, strings.TrimSuffix(d.Round(time.Minute).String(), "0s"))
			st.Detail = r.detail
		}
	} else {
		st.PendingSince = time.Time{}
	}

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
	if r.kind == resOK {
		b.Up++
		b.MsSum += r.ms
	}

	prev := st.Status
	var next string
	switch r.kind {
	case resOK:
		st.Fails = 0
		next = statusUp
	case resPending:
		next = statusStarting
		if prev == statusDown {
			next = statusDown // not recovered until it is actually up
		}
	default:
		st.Fails++
		if st.Fails == 1 {
			st.FailSince = now
		}
		next = statusFailing
		if prev == statusDown || st.Fails >= m.cfg.FailThreshold {
			next = statusDown
		}
	}
	if next == statusUp {
		defer func() { st.SeenUp = true }()
	}
	if next == prev {
		return
	}
	st.Status = next

	// First line of an email's details: the URL, or for servers the service check (if any).
	where := func(width int) string {
		switch {
		case t.URL == "":
			return ""
		case t.Kind == "server":
			return fmt.Sprintf("%-*s %s\n", width, "Check:", t.URL)
		}
		return fmt.Sprintf("%-*s %s\n", width, "URL:", t.URL)
	}
	switch {
	case next == statusDown:
		st.Since = st.FailSince
		// A server that has never been seen running (e.g. one that is simply
		// switched off when first discovered) is shown as down but not announced.
		if t.Kind == "server" && !st.SeenUp {
			return
		}
		st.Announced = true
		m.event(Event{Time: now, TargetID: t.ID, Name: t.Label(), Type: "down", Detail: r.detail})
		if !m.d.Muted[t.ID] {
			st.Notified = true
			m.notify(now, "DOWN: "+t.Label(), fmt.Sprintf(
				"%s is DOWN\n\n%sSince:  %s\nReason: %s\nFailed checks in a row: %d\n",
				t.Label(), where(7), fmtTime(st.Since), r.detail, st.Fails))
		}
	case next == statusUp && prev == statusDown:
		dur := now.Sub(st.Since).Round(time.Second)
		if t.Kind != "server" { // only servers can have a quiet or muted outage
			st.Announced, st.Notified = true, true
		}
		if st.Announced {
			m.event(Event{Time: now, TargetID: t.ID, Name: t.Label(), Type: "up", Detail: r.detail, DurationSec: int64(dur.Seconds())})
		}
		if st.Notified {
			m.notify(now, "RECOVERED: "+t.Label(), fmt.Sprintf(
				"%s has RECOVERED\n\n%sDown from: %s\nUp again:  %s\nDowntime:  %s\n",
				t.Label(), where(10), fmtTime(st.Since), fmtTime(now), dur))
		}
		st.Announced, st.Notified = false, false
		st.Since = now
	case next == statusUp && prev != statusFailing, next == statusStarting:
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

// SetAlerts switches email alerts for one server on or off.
func (m *Monitor) SetAlerts(id string, enabled bool) bool {
	m.mu.Lock()
	known := false
	for _, t := range m.d.Servers {
		known = known || t.ID == id
	}
	if known {
		if enabled {
			delete(m.d.Muted, id)
		} else {
			m.d.Muted[id] = true
		}
	}
	m.mu.Unlock()
	if known {
		log.Printf("alerts for %s set to %v", id, enabled)
		m.Save()
	}
	return known
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

// ---- panel and node checks ----

func (m *Monitor) get(ctx context.Context, skipVerify bool, rawURL string, hdr map[string]string) (int, []byte, int64, error) {
	code, body, ms, _, err := m.getx(ctx, skipVerify, rawURL, hdr)
	return code, body, ms, err
}

// getx is get plus the expiry date of the TLS certificate the server
// presented (zero for plain http). The date is also recovered when the
// request fails because that certificate did not verify, e.g. it has expired.
func (m *Monitor) getx(ctx context.Context, skipVerify bool, rawURL string, hdr map[string]string) (int, []byte, int64, time.Time, error) {
	var expiry time.Time
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, 0, expiry, err
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
		var cve *tls.CertificateVerificationError
		if errors.As(err, &cve) && len(cve.UnverifiedCertificates) > 0 {
			expiry = cve.UnverifiedCertificates[0].NotAfter
		}
		return 0, nil, time.Since(start).Milliseconds(), expiry, err
	}
	defer resp.Body.Close()
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		expiry = resp.TLS.PeerCertificates[0].NotAfter
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, time.Since(start).Milliseconds(), expiry, nil
}

// trackCert remembers when a target's certificate expires and raises one
// notice per certificate once that is closer than CERT_WARN_DAYS. It reports
// what the service is serving right now, which is what its clients see: a
// daemon that was not restarted after a renewal keeps presenting the old one.
// Caller holds m.mu.
func (m *Monitor) trackCert(t Target, st *State, r result, now time.Time) {
	if r.certExpiry.IsZero() {
		if r.kind == resOK { // answered without TLS
			st.CertExpiry = time.Time{}
		}
		return
	}
	st.CertExpiry = r.certExpiry
	left := r.certExpiry.Sub(now)
	if m.cfg.CertWarn <= 0 || left > m.cfg.CertWarn || st.CertWarned.Equal(r.certExpiry) {
		return
	}
	st.CertWarned = r.certExpiry
	on := r.certExpiry.Local().Format("2006-01-02")
	what := "TLS certificate expired on " + on
	if left > 0 {
		what = fmt.Sprintf("TLS certificate expires in %s, on %s", days(left), on)
	}
	m.event(Event{Time: now, TargetID: t.ID, Name: t.Label(), Type: "notice", Detail: what})
	m.notify(now, "CERTIFICATE: "+t.Label(), fmt.Sprintf(
		"%s: %s\n\nURL: %s\n\nThis is the certificate the service is presenting right now. If it was renewed\n"+
			"on disk but the service has not been restarted since, restarting the service is\nusually all that is needed.\n",
		t.Label(), what, t.URL))
}

func days(d time.Duration) string {
	n := int(d.Hours() / 24)
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// failDetail describes a failed request, naming an expired certificate plainly.
func failDetail(err error, certExpiry time.Time) string {
	var inv x509.CertificateInvalidError
	if errors.As(err, &inv) && inv.Reason == x509.Expired && !certExpiry.IsZero() && certExpiry.Before(time.Now()) {
		return "TLS certificate expired on " + certExpiry.Local().Format("2006-01-02")
	}
	return errDetail(err)
}

func errDetail(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timed out"
		}
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	return err.Error()
}

func (m *Monitor) checkPanel(ctx context.Context, u string, skipVerify bool) result {
	code, _, ms, cert, err := m.getx(ctx, skipVerify, u, nil)
	r := okResult(ms, fmt.Sprintf("HTTP %d", code))
	switch {
	case err != nil:
		r = failResult(ms, failDetail(err, cert))
	case code >= 400:
		r = failResult(ms, fmt.Sprintf("HTTP %d", code))
	}
	r.certExpiry = cert
	return r
}

func bearer(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key, "Accept": "application/json"}
}

func (m *Monitor) listNodes(ctx context.Context) ([]Target, result) {
	var nodes []Target
	var totalMs int64
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/application/nodes?per_page=100&page=%d", m.cfg.APIURL, page)
		code, body, ms, err := m.get(ctx, m.cfg.LANInsecure, u, bearer(m.cfg.APIKey))
		totalMs += ms
		if err != nil {
			return nil, failResult(totalMs, errDetail(err))
		}
		if code != http.StatusOK {
			hint := ""
			if code == 401 || code == 403 {
				hint = " (check PANEL_API_KEY: needs an Application API key with Nodes: Read)"
			}
			return nil, failResult(totalMs, fmt.Sprintf("HTTP %d%s", code, hint))
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
			Meta pageMeta `json:"meta"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, failResult(totalMs, "unexpected API response: "+err.Error())
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
	return nodes, okResult(totalMs, fmt.Sprintf("%d node(s) listed", len(nodes)))
}

type pageMeta struct {
	Pagination struct {
		TotalPages int `json:"total_pages"`
	} `json:"pagination"`
}

// checkNode asks the node's Wings daemon for anything at all. Wings answers
// 401 to requests without its daemon token, which is enough to know it is alive.
func (m *Monitor) checkNode(ctx context.Context, n Target) result {
	code, _, ms, cert, err := m.getx(ctx, m.cfg.NodeInsecure, n.URL+"/api/system", map[string]string{"Accept": "application/json"})
	r := okResult(ms, "Wings responding")
	switch {
	case err != nil:
		r = failResult(ms, failDetail(err, cert))
	case code != 200 && code != 401 && code != 403:
		r = failResult(ms, fmt.Sprintf("HTTP %d from Wings", code))
	}
	r.certExpiry = cert
	return r
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
	Alerts bool                `json:"alerts"`
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
	ServersOn   bool         `json:"serversEnabled"`
	CertWarn    int          `json:"certWarnDays"`
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
		Range: rng, CellSec: r.cellSec, EmailOn: m.cfg.SMTP.Enabled(), ServersOn: m.cfg.ClientKey != "",
		CertWarn: int(m.cfg.CertWarn.Hours() / 24),
		Targets:  []TargetView{}, Events: []Event{},
	}
	for i := len(m.d.Events) - 1; i >= 0; i-- {
		s.Events = append(s.Events, m.d.Events[i])
	}

	nCells := int64(r.span.Seconds()) / r.cellSec
	end := (now.Unix()/r.cellSec + 1) * r.cellSec
	start := end - nCells*r.cellSec

	for _, t := range append(append(m.staticTargets(), m.d.Nodes...), m.d.Servers...) {
		st := m.d.States[t.ID]
		if st == nil {
			st = &State{Status: statusUnknown, Since: now}
		}
		v := TargetView{Target: t, State: *st, Alerts: !m.d.Muted[t.ID], Uptime: map[string]*float64{}, Cells: make([]Cell, nCells)}
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
