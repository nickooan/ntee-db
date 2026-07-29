// Command nteedb-server exposes a ntee-db store over TCP with a
// memcached-style text protocol and single-line JSON responses.
//
//	nteedb-server -schema schema.json [-addr 127.0.0.1:6666] [-dir /override]
//	              [-tls-cert cert.pem -tls-key key.pem [-tls-addr 127.0.0.1:6667]]
//
// TLS (optional): -tls-cert/-tls-key start a TLS listener on -tls-addr
// alongside the plain one; -addr "" disables the plain listener for a
// TLS-only deployment.
//
// Auth (optional): -auth / NTEEDB_AUTH for a single shared password (grants
// admin), or -auth-file for user:password[:role] lines. With no auth the
// server refuses to bind non-loopback addresses unless -insecure is set
// (protected mode, borrowed from redis) — TLS encrypts but does not
// authorize, so the rule applies to both listeners.
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

const defaultTLSAddr = "127.0.0.1:6667"

// Version is the release version, printed by -version. Release builds inject
// the git tag via -ldflags "-X main.Version=…"; "dev" marks a plain
// `go build` / `go install`.
var Version = "dev"

type cliOptions struct {
	addr     string
	tlsAddr  string
	tlsCert  string
	tlsKey   string
	schema   string
	dir      string
	password string
	authFile string
	insecure bool
	idle     time.Duration
}

func main() {
	var o cliOptions
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.StringVar(&o.addr, "addr", "127.0.0.1:6666", "host:port for the plain listener (\"\" disables it — TLS-only)")
	flag.StringVar(&o.tlsAddr, "tls-addr", defaultTLSAddr, "host:port for the TLS listener (requires -tls-cert/-tls-key)")
	flag.StringVar(&o.tlsCert, "tls-cert", "", "PEM certificate (or chain) enabling the TLS listener")
	flag.StringVar(&o.tlsKey, "tls-key", "", "PEM private key for -tls-cert")
	flag.StringVar(&o.schema, "schema", "", "path to schema.json (required)")
	flag.StringVar(&o.dir, "dir", "", "store directory (overrides schema's \"dir\")")
	flag.StringVar(&o.password, "auth", os.Getenv("NTEEDB_AUTH"), "shared auth password (or NTEEDB_AUTH env); grants admin")
	flag.StringVar(&o.authFile, "auth-file", "", "path to user:password[:role] file (role: admin|user)")
	flag.BoolVar(&o.insecure, "insecure", false, "allow binding a non-loopback address without auth")
	flag.DurationVar(&o.idle, "idle", 5*time.Minute, "per-connection idle timeout (0 disables)")
	flag.Parse()

	if *showVersion {
		fmt.Println("nteedb-server " + Version)
		return
	}

	log.SetFlags(log.LstdFlags)
	log.SetPrefix("nteedb-server: ")
	if err := run(o); err != nil {
		log.Fatal(err)
	}
}

func run(o cliOptions) error {
	if o.schema == "" {
		return errors.New("-schema is required (see nteedb-server/README.md)")
	}
	schema, err := LoadSchema(o.schema)
	if err != nil {
		return err
	}
	if o.dir != "" {
		schema.Dir = o.dir
	}
	if schema.Dir == "" {
		return errors.New(`no store directory: set "dir" in the schema or pass -dir`)
	}

	tlsConf, err := buildTLS(o)
	if err != nil {
		return err
	}
	if o.addr == "" && tlsConf == nil {
		return errors.New(`-addr "" disables the plain listener, which requires TLS: pass -tls-cert/-tls-key`)
	}

	auth, err := buildAuth(o.password, o.authFile)
	if err != nil {
		return err
	}
	if o.addr != "" {
		if err := checkProtectedMode("-addr", o.addr, auth, o.insecure); err != nil {
			return err
		}
	}
	if tlsConf != nil {
		if err := checkProtectedMode("-tls-addr", o.tlsAddr, auth, o.insecure); err != nil {
			return err
		}
	}

	opts, err := schema.Options()
	if err != nil {
		return err
	}
	db, err := nteedb.Open(opts)
	if errors.Is(err, nteedb.ErrLocked) {
		return fmt.Errorf("store %s is locked by another process (the store allows a single writer; stop the other process first): %w", schema.Dir, err)
	}
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close() // writes the index hint → next boot is fast

	cfg := Config{Addr: o.addr, TLSAddr: o.tlsAddr, TLSConfig: tlsConf, IdleTimeout: o.idle}
	schema.AutoCompact.apply(&cfg) // user-set thresholds win; the rest default in NewServer
	srv := NewServer(cfg, db, auth, schema)
	if err := srv.Listen(); err != nil {
		return err
	}
	autoCompact := "off"
	switch {
	case cfg.AutoCompact && cfg.BlobsRelieve:
		autoCompact = "on"
	case cfg.AutoCompact:
		autoCompact = "on (blobs off)"
	}
	// Keep this line's shape stable — clients' test harnesses discover the
	// bound address by matching "listening on <addr>". The TLS line is worded
	// without that prefix (and printed second) so it can never shadow it.
	if addr := srv.Addr(); addr != "" {
		log.Printf("listening on %s (store %s, auth %s, %d indexes, auto-compact %s)",
			addr, schema.Dir, auth.mode, len(schema.Indexes), autoCompact)
	} else {
		log.Printf("plain listener disabled (store %s, auth %s, %d indexes, auto-compact %s)",
			schema.Dir, auth.mode, len(schema.Indexes), autoCompact)
	}
	if tlsAddr := srv.TLSAddr(); tlsAddr != "" {
		log.Printf("tls on %s (cert %s)", tlsAddr, o.tlsCert)
	}

	// Graceful shutdown: stop accepting, close connections, then (deferred)
	// db.Close.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()

	select {
	case s := <-sig:
		log.Printf("received %s, shutting down", s)
		srv.Close()
		return nil
	case err := <-done:
		srv.Close()
		return err
	}
}

func buildAuth(password, authFile string) (*authStore, error) {
	switch {
	case password != "" && authFile != "":
		return nil, errors.New("-auth and -auth-file are mutually exclusive")
	case authFile != "":
		return authFileStore(authFile)
	case password != "":
		return authPassword(password), nil
	default:
		return authNone(), nil
	}
}

// buildTLS turns the -tls-* flags into a *tls.Config, or nil when TLS is
// off. Cert and key must come together; a non-default -tls-addr without them
// is a misconfiguration, not a silently ignored flag.
func buildTLS(o cliOptions) (*tls.Config, error) {
	if o.tlsCert == "" && o.tlsKey == "" {
		if o.tlsAddr != defaultTLSAddr {
			return nil, errors.New("-tls-addr requires -tls-cert and -tls-key")
		}
		return nil, nil
	}
	if o.tlsCert == "" || o.tlsKey == "" {
		return nil, errors.New("-tls-cert and -tls-key must be set together")
	}
	cert, err := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// checkProtectedMode refuses a non-loopback bind when no auth is configured,
// unless -insecure explicitly accepts that (trusted private network). It
// applies to the plain and TLS listeners alike: TLS encrypts the transport
// but authorizes nobody.
func checkProtectedMode(flagName, addr string, auth *authStore, insecure bool) error {
	if auth.required() || insecure {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad %s %q: %w", flagName, addr, err)
	}
	if isLoopback(host) {
		return nil
	}
	return fmt.Errorf("protected mode: refusing to bind %q without auth — set -auth/NTEEDB_AUTH or -auth-file, or pass -insecure for a trusted network", addr)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
