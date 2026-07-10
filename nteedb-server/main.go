// Command nteedb-server exposes a ntee-db store over TCP with a
// memcached-style text protocol and single-line JSON responses.
//
//	nteedb-server -schema schema.json [-addr 127.0.0.1:6740] [-dir /override]
//
// Auth (optional): -auth / NTEEDB_AUTH for a single shared password (grants
// admin), or -auth-file for user:password[:role] lines. With no auth the
// server refuses to bind non-loopback addresses unless -insecure is set
// (protected mode, borrowed from redis).
package main

import (
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

type cliOptions struct {
	addr     string
	schema   string
	dir      string
	password string
	authFile string
	insecure bool
	idle     time.Duration
}

func main() {
	var o cliOptions
	flag.StringVar(&o.addr, "addr", "127.0.0.1:6740", "host:port to listen on")
	flag.StringVar(&o.schema, "schema", "", "path to schema.json (required)")
	flag.StringVar(&o.dir, "dir", "", "store directory (overrides schema's \"dir\")")
	flag.StringVar(&o.password, "auth", os.Getenv("NTEEDB_AUTH"), "shared auth password (or NTEEDB_AUTH env); grants admin")
	flag.StringVar(&o.authFile, "auth-file", "", "path to user:password[:role] file (role: admin|user)")
	flag.BoolVar(&o.insecure, "insecure", false, "allow binding a non-loopback address without auth")
	flag.DurationVar(&o.idle, "idle", 5*time.Minute, "per-connection idle timeout (0 disables)")
	flag.Parse()

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

	auth, err := buildAuth(o.password, o.authFile)
	if err != nil {
		return err
	}
	if err := checkProtectedMode(o.addr, auth, o.insecure); err != nil {
		return err
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

	srv := NewServer(Config{Addr: o.addr, IdleTimeout: o.idle, AutoCompact: schema.AutoCompact}, db, auth, schema)
	if err := srv.Listen(); err != nil {
		return err
	}
	autoCompact := "off"
	if schema.AutoCompact {
		autoCompact = "on"
	}
	log.Printf("listening on %s (store %s, auth %s, %d indexes, auto-compact %s)",
		srv.Addr(), schema.Dir, auth.mode, len(schema.Indexes), autoCompact)

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

// checkProtectedMode refuses a non-loopback bind when no auth is configured,
// unless -insecure explicitly accepts that (trusted private network).
func checkProtectedMode(addr string, auth *authStore, insecure bool) error {
	if auth.required() || insecure {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad -addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("protected mode: refusing to bind %q without auth — set -auth/NTEEDB_AUTH or -auth-file, or pass -insecure for a trusted network", addr)
}
