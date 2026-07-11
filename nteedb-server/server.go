package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

const serverVersion = "0.1.0"

type Config struct {
	Addr        string
	IdleTimeout time.Duration // 0 disables the per-command read deadline
	MaxLine     int           // command line limit (inline puts included)
	MaxValue    int           // length-prefixed data block limit
	Quiet       bool          // silence per-connection logs (tests)

	// Auto-compaction policy (user-configurable via schema.json's
	// "autoCompact" — bool or options object; see AutoCompactConfig). The core
	// exposes only mechanisms (Compact, BlobsRelieve) and measurements
	// (Stats, LiveBytes, BlobUsage); every threshold lives here. Reads stay
	// live during any reclamation; writes pend.
	AutoCompact     bool
	CompactInterval time.Duration // check cadence (default 30s)

	// Main log: compact when its dead-space share reaches MainRatio and the
	// log is at least MainMinBytes (compacting tiny logs is pointless churn).
	MainRatio    float64 // default 0.5
	MainMinBytes int64   // default 1 MiB

	// Blobs: run BlobsRelieve when the orphaned share reaches BlobRatio and
	// the blob files hold at least BlobMinRelieveDataSize. The floor is much
	// higher than the main log's because a blob rewrite copies live blob
	// contents — real I/O.
	BlobsRelieve           bool    // enable the automatic blob trigger
	BlobRatio              float64 // default 0.5
	BlobMinRelieveDataSize int64   // default 64 MiB
}

type Server struct {
	cfg    Config
	db     *nteedb.DB
	auth   *authStore
	schema *Schema
	kinds  map[string]nteedb.ValueKind

	ln     net.Listener
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed atomic.Bool
	stop   chan struct{} // closed by Close; ends the auto-compact loop

	// counters surfaced by the stats command
	totalConns   atomic.Int64
	commands     atomic.Int64
	autoCompacts atomic.Int64
	blobCompacts atomic.Int64 // relieve runs that rewrote the blob file
}

func NewServer(cfg Config, db *nteedb.DB, auth *authStore, schema *Schema) *Server {
	if cfg.MaxLine <= 0 {
		cfg.MaxLine = 1 << 20 // 1 MiB
	}
	if cfg.MaxValue <= 0 {
		cfg.MaxValue = 32 << 20 // 32 MiB
	}
	if cfg.MainRatio <= 0 {
		cfg.MainRatio = 0.5
	}
	if cfg.MainMinBytes <= 0 {
		cfg.MainMinBytes = 1 << 20 // 1 MiB
	}
	if cfg.CompactInterval <= 0 {
		cfg.CompactInterval = 30 * time.Second
	}
	if cfg.BlobRatio <= 0 {
		cfg.BlobRatio = 0.5
	}
	if cfg.BlobMinRelieveDataSize <= 0 {
		cfg.BlobMinRelieveDataSize = 64 << 20 // 64 MiB
	}
	return &Server{
		cfg:    cfg,
		db:     db,
		auth:   auth,
		schema: schema,
		kinds:  schema.Kinds(),
		conns:  make(map[net.Conn]struct{}),
		stop:   make(chan struct{}),
	}
}

// Listen binds the address. Split from Serve so callers (and tests, with
// port 0) can learn the bound address before serving.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve accepts connections until Close. Each connection gets its own
// goroutine; reads run in parallel via the core's RWMutex, writes serialize.
func (s *Server) Serve() error {
	if s.cfg.AutoCompact {
		s.wg.Add(1)
		go s.autoCompactLoop()
	}
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return nil // Close() shut the listener down
			}
			return err
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.totalConns.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(c)
		}()
	}
}

// Close stops accepting, closes live connections, and waits for handlers to
// finish. The DB is closed by the caller (main) after Close returns.
func (s *Server) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.stop)
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// autoCompactLoop periodically checks the log's dead-space ratio and compacts
// when it crosses the threshold. Compaction keeps reads live (the core holds
// only its compaction gate during the rebuild); writes pend until it finishes.
func (s *Server) autoCompactLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.CompactInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.maybeCompact()
		}
	}
}

// maybeCompact runs one pass of the server's reclamation policy. Blobs are
// checked first: BlobsRelieve compacts the main log as part of its commit, so
// when it runs the main-log check is moot.
func (s *Server) maybeCompact() {
	st := s.db.Stats()

	if usage, need := s.blobsNeedRelieve(st); need {
		s.runBlobsRelieve(st, usage)
		return
	}
	if s.mainNeedsCompact(st) {
		s.runMainCompact(st)
	}
}

// blobsNeedRelieve decides the blob trigger: enabled, past the size floor
// (O(1) — the O(records) BlobUsage scan only runs beyond it), and either the
// orphaned share reached BlobRatio or a crashed relieve left multiple
// generation files to consolidate.
func (s *Server) blobsNeedRelieve(st nteedb.Stats) (nteedb.BlobUsage, bool) {
	if !s.cfg.BlobsRelieve || st.BlobBytes < s.cfg.BlobMinRelieveDataSize {
		return nteedb.BlobUsage{}, false
	}
	usage, err := s.db.BlobUsage()
	if err != nil {
		s.logf("auto-compact: blob usage check failed: %v", err)
		return nteedb.BlobUsage{}, false
	}
	orphaned := float64(usage.OrphanedBytes) >= s.cfg.BlobRatio*float64(usage.TotalBytes)
	return usage, orphaned || usage.Generations > 1
}

func (s *Server) runBlobsRelieve(st nteedb.Stats, usage nteedb.BlobUsage) {
	s.logf("auto-compact: %d of %d blob bytes orphaned (%d generations), relieving",
		usage.OrphanedBytes, usage.TotalBytes, usage.Generations)
	start := time.Now()
	if err := s.db.BlobsRelieve(); err != nil {
		s.logf("auto-compact: relieve failed: %v", err)
		return
	}
	s.autoCompacts.Add(1)
	s.blobCompacts.Add(1)
	after := s.db.Stats()
	s.logf("auto-compact: main %d → %d bytes, blobs %d → %d bytes in %s",
		st.MainBytes, after.MainBytes, st.BlobBytes, after.BlobBytes,
		time.Since(start).Round(time.Millisecond))
}

// mainNeedsCompact decides the main-log trigger: past the size floor with a
// dead-space share of at least MainRatio.
func (s *Server) mainNeedsCompact(st nteedb.Stats) bool {
	if st.MainBytes < s.cfg.MainMinBytes {
		return false
	}
	dead := st.MainBytes - s.db.LiveBytes()
	return float64(dead) >= s.cfg.MainRatio*float64(st.MainBytes)
}

func (s *Server) runMainCompact(st nteedb.Stats) {
	s.logf("auto-compact: %d of %d main-log bytes dead, compacting",
		st.MainBytes-s.db.LiveBytes(), st.MainBytes)
	start := time.Now()
	if err := s.db.Compact(); err != nil {
		s.logf("auto-compact failed: %v", err)
		return
	}
	s.autoCompacts.Add(1)
	s.logf("auto-compact: main %d → %d bytes in %s",
		st.MainBytes, s.db.Stats().MainBytes, time.Since(start).Round(time.Millisecond))
}

// logf logs unless the server is quiet (tests) or shutting down (errors from
// a torn-down store are noise, not news).
func (s *Server) logf(format string, args ...any) {
	if s.cfg.Quiet || s.closed.Load() {
		return
	}
	log.Printf(format, args...)
}

// connState is one client connection's session: auth status and granted role.
type connState struct {
	authed bool
	role   role
}

func (s *Server) handleConn(c net.Conn) {
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()

	r := bufio.NewReaderSize(c, s.cfg.MaxLine)
	w := bufio.NewWriter(c)
	rw := respWriter{w: w}

	st := connState{}
	if !s.auth.required() {
		st.authed, st.role = true, roleAdmin
	}

	for {
		if s.cfg.IdleTimeout > 0 {
			c.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		}
		line, err := readLine(r)
		if err != nil {
			switch {
			case errors.Is(err, errLineTooLong):
				rw.fail("%v (max %d bytes; use the length-prefixed put form for large values)", err, s.cfg.MaxLine)
				w.Flush()
			case errors.Is(err, io.EOF):
				// clean disconnect
			default:
				if ne, ok := err.(net.Error); ok && ne.Timeout() && !s.cfg.Quiet {
					log.Printf("conn %s: idle timeout", c.RemoteAddr())
				}
			}
			return // any read error ends the connection
		}
		if len(line) == 0 {
			continue
		}

		s.commands.Add(1)
		quit, err := s.dispatch(rw, r, line, &st)
		if err != nil {
			// Protocol-level failure mid-frame (bad data block, value too big
			// with unread bytes, network error): report if possible and drop
			// the connection — the stream position is no longer trustworthy.
			rw.fail("%v", err)
			w.Flush()
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
		if quit {
			return
		}
	}
}

// requireAuth guards a command: unauthenticated connections may only run
// auth/hello/quit (redis's pre-auth allowlist — ping deliberately excluded).
func (st *connState) requireAuth() error {
	if !st.authed {
		return fmt.Errorf("auth required")
	}
	return nil
}

func (st *connState) requireAdmin(cmd string) error {
	if err := st.requireAuth(); err != nil {
		return err
	}
	if st.role != roleAdmin {
		return fmt.Errorf("permission denied: %s requires admin", cmd)
	}
	return nil
}
