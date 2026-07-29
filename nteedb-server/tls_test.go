package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// testTLSConfig generates a self-signed certificate for loopback and returns
// the server-side tls.Config plus a client config that trusts it.
func testTLSConfig(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nteedb-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	server := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}
	client := &tls.Config{RootCAs: pool, ServerName: "localhost"}
	return server, client
}

// startTLSServer starts a server with both a plain and a TLS listener on
// ephemeral ports, returning it plus the client-side TLS config.
func startTLSServer(t *testing.T, auth *authStore) (*Server, *tls.Config) {
	t.Helper()
	serverConf, clientConf := testTLSConfig(t)
	srv := startServer(t, testSchema(t), auth, Config{
		TLSAddr:   "127.0.0.1:0",
		TLSConfig: serverConf,
	})
	return srv, clientConf
}

// dialTLS mirrors dial() over the TLS listener.
func dialTLS(t *testing.T, srv *Server, conf *tls.Config) *testClient {
	t.Helper()
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", srv.TLSAddr(), conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &testClient{t: t, c: c, r: bufio.NewReader(c)}
}

func TestTLSCommands(t *testing.T) {
	srv, clientConf := startTLSServer(t, authNone())
	tc := dialTLS(t, srv, clientConf)

	tc.mustOK(`put k {"a":1}`)
	if r := tc.mustOK("get k").(map[string]any); r["a"] != float64(1) {
		t.Fatalf("get over TLS = %v", r)
	}
	if r := tc.mustOK("incr c 5"); r != float64(5) {
		t.Fatalf("incr over TLS = %v", r)
	}
	if r := tc.mustOK("topup c 5 8"); r != float64(2) {
		t.Fatalf("topup over TLS = %v", r)
	}
}

func TestTLSAndPlainShareTheStore(t *testing.T) {
	srv, clientConf := startTLSServer(t, authNone())
	plain := dial(t, srv)
	secure := dialTLS(t, srv, clientConf)

	plain.mustOK(`put shared {"via":"plain"}`)
	if r := secure.mustOK("get shared").(map[string]any); r["via"] != "plain" {
		t.Fatalf("TLS conn does not see plain write: %v", r)
	}
	secure.mustOK(`put shared2 {"via":"tls"}`)
	if r := plain.mustOK("get shared2").(map[string]any); r["via"] != "tls" {
		t.Fatalf("plain conn does not see TLS write: %v", r)
	}
	if srv.Addr() == "" || srv.TLSAddr() == "" {
		t.Fatalf("expected both listeners bound: plain=%q tls=%q", srv.Addr(), srv.TLSAddr())
	}
}

func TestTLSAuth(t *testing.T) {
	srv, clientConf := startTLSServer(t, authPassword("s3cret"))
	tc := dialTLS(t, srv, clientConf)

	tc.mustFail("get k", "auth required")
	tc.mustOK("auth s3cret")
	tc.mustOK(`put k {"a":1}`)
}

func TestTLSRejectsUntrustedClientVerification(t *testing.T) {
	srv, _ := startTLSServer(t, authNone())
	// A client with an empty root pool must fail verification of the
	// self-signed certificate.
	empty := &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "localhost"}
	_, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", srv.TLSAddr(), empty)
	if err == nil {
		t.Fatal("dial with empty root pool unexpectedly succeeded")
	}
}

func TestCloseShutsBothListeners(t *testing.T) {
	serverConf, _ := testTLSConfig(t)
	srv := startServer(t, testSchema(t), authNone(), Config{
		TLSAddr:   "127.0.0.1:0",
		TLSConfig: serverConf,
	})
	plainAddr, tlsAddr := srv.Addr(), srv.TLSAddr()
	srv.Close()
	for _, addr := range []string{plainAddr, tlsAddr} {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			t.Fatalf("listener %s still accepting after Close", addr)
		}
	}
}
