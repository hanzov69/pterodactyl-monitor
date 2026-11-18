package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testCA issues short-lived certificates for 127.0.0.1 so the checks can be
// run against servers presenting healthy, soon-to-expire and expired ones.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-90 * 24 * time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert, key, pool}
}

// serve starts an HTTPS server whose certificate expires at notAfter and
// which answers 401, as Wings does to a request without its token.
func (ca *testCA) serve(t *testing.T, notAfter time.Time) *httptest.Server {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	notBefore := time.Now().Add(-time.Hour)
	if notAfter.Before(notBefore) { // an already expired certificate
		notBefore = notAfter.Add(-48 * time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: notBefore, NotAfter: notAfter,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func newTestMonitor(t *testing.T, ca *testCA) *Monitor {
	t.Helper()
	m := NewMonitor(Config{
		Timeout: 5 * time.Second, Interval: time.Minute, FailThreshold: 3, DataDir: t.TempDir(),
		CertWarn: 14 * 24 * time.Hour,
		SMTP:     SMTPConfig{Host: "smtp.invalid", From: "a@example.test", To: []string{"b@example.test"}},
	})
	m.secure.Transport.(*http.Transport).TLSClientConfig.RootCAs = ca.pool
	return m
}

func notices(m *Monitor) (events []string) {
	for _, e := range m.d.Events {
		if e.Type == "notice" {
			events = append(events, e.Detail)
		}
	}
	return events
}

func near(a, b time.Time) bool { return a.Sub(b).Abs() < 2*time.Second }

func TestCertificateExpiryIsTracked(t *testing.T) {
	ca := newTestCA(t)
	m := newTestMonitor(t, ca)
	node := Target{ID: "node-1", Name: "Node: test", Kind: "node"}
	now := time.Now()

	// Healthy certificate, far from expiry: recorded, no notice.
	far := now.Add(80 * 24 * time.Hour)
	node.URL = ca.serve(t, far).URL
	r := m.checkNode(context.Background(), node)
	if r.kind != resOK || !near(r.certExpiry, far) {
		t.Fatalf("healthy cert: kind=%v expiry=%v detail=%q", r.kind, r.certExpiry, r.detail)
	}
	m.apply(node, r, now)
	if st := m.d.States[node.ID]; !near(st.CertExpiry, far) || len(notices(m)) != 0 || len(m.d.Outbox) != 0 {
		t.Fatalf("healthy cert should be recorded without a notice: %+v %v", st, notices(m))
	}

	// Inside the warning window: still up, exactly one notice however often it is seen.
	soon := now.Add(10 * 24 * time.Hour)
	node.URL = ca.serve(t, soon).URL
	r = m.checkNode(context.Background(), node)
	if r.kind != resOK || !near(r.certExpiry, soon) {
		t.Fatalf("expiring cert: kind=%v expiry=%v detail=%q", r.kind, r.certExpiry, r.detail)
	}
	m.apply(node, r, now)
	m.apply(node, r, now.Add(3*time.Minute))
	if n := notices(m); len(n) != 1 || !strings.Contains(n[0], "expires in 9 days") || len(m.d.Outbox) != 1 {
		t.Fatalf("want one 'expires in 9 days' notice and one email, got %v, outbox %d", n, len(m.d.Outbox))
	}
	if m.d.States[node.ID].Status != statusUp {
		t.Fatalf("an expiring certificate must not mark the node down: %s", m.d.States[node.ID].Status)
	}

	// Renewed: quiet again, and the new certificate gets its own notice later.
	r.certExpiry = now.Add(90 * 24 * time.Hour)
	m.apply(node, r, now.Add(6*time.Minute))
	if len(notices(m)) != 1 {
		t.Fatalf("a renewed certificate should not raise a notice: %v", notices(m))
	}
	m.apply(node, r, now.Add(80*24*time.Hour))
	if len(notices(m)) != 2 {
		t.Fatalf("the renewed certificate should be warned about when its turn comes: %v", notices(m))
	}
}

func TestExpiredCertificate(t *testing.T) {
	ca := newTestCA(t)
	m := newTestMonitor(t, ca)
	node := Target{ID: "node-1", Name: "Node: test", Kind: "node"}
	gone := time.Now().Add(-24 * time.Hour)
	node.URL = ca.serve(t, gone).URL

	// Verified connection: the check fails and says why in plain words.
	r := m.checkNode(context.Background(), node)
	if r.kind != resFail || !strings.HasPrefix(r.detail, "TLS certificate expired on ") || !near(r.certExpiry, gone) {
		t.Fatalf("expired cert: kind=%v expiry=%v detail=%q", r.kind, r.certExpiry, r.detail)
	}
	m.apply(node, r, time.Now())
	if n := notices(m); len(n) != 1 || !strings.HasPrefix(n[0], "TLS certificate expired on ") {
		t.Fatalf("want an 'expired' notice, got %v", n)
	}

	// With verification switched off the connection works, but the expiry is still seen.
	m2 := newTestMonitor(t, ca)
	m2.cfg.NodeInsecure = true
	r = m2.checkNode(context.Background(), node)
	if r.kind != resOK || !near(r.certExpiry, gone) {
		t.Fatalf("expired cert, unverified: kind=%v expiry=%v detail=%q", r.kind, r.certExpiry, r.detail)
	}
	m2.apply(node, r, time.Now())
	if n := notices(m2); len(n) != 1 || !strings.HasPrefix(n[0], "TLS certificate expired on ") {
		t.Fatalf("want an 'expired' notice with verification off, got %v", n)
	}
}

func TestPlainHTTPHasNoCertificate(t *testing.T) {
	m := newTestMonitor(t, newTestCA(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	panel := Target{ID: "panel-lan", Name: "Panel (LAN)", Kind: "panel", URL: srv.URL}
	m.state(panel.ID, time.Now()).CertExpiry = time.Now().Add(time.Hour) // left over from an https URL
	r := m.checkPanel(context.Background(), srv.URL, false)
	m.apply(panel, r, time.Now())
	if st := m.d.States[panel.ID]; r.kind != resOK || !st.CertExpiry.IsZero() || len(notices(m)) != 0 {
		t.Fatalf("plain http should clear certificate info: kind=%v state=%+v", r.kind, st)
	}
}

func TestCertWarningCanBeTurnedOff(t *testing.T) {
	ca := newTestCA(t)
	m := newTestMonitor(t, ca)
	m.cfg.CertWarn = 0
	node := Target{ID: "node-1", Name: "Node: test", Kind: "node", URL: ca.serve(t, time.Now().Add(48*time.Hour)).URL}
	m.apply(node, m.checkNode(context.Background(), node), time.Now())
	if len(notices(m)) != 0 || m.d.States[node.ID].CertExpiry.IsZero() {
		t.Fatalf("CERT_WARN_DAYS=0 should record the expiry but never warn: %v", notices(m))
	}
}
