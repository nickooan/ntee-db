// Package nteedb is a pure-Go, dependency-free embedded key-value store.
//
// It is log-structured: an append-only JSONL "main table" is
// the source of truth, and an in-memory index maps each key to the byte offset
// of its latest record in that file. The data log doubles as the write-ahead
// log, so there is no separate WAL — the index is always rebuildable from the
// log and can never drift out of sync with the data.
//
// It supports exact lookups and prefix scans (no substring/fuzzy search). Only
// the index (keys + offsets) is resident in memory; record bodies and large
// values stay on disk and are read on demand.
package nteedb

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// DefaultBlobThreshold is the value size at or above which a value is stored in
// the blob side file instead of inline in the main log.
//
// This is a disk-layout/compaction knob, NOT a memory knob: values are never
// held in memory regardless (only the index is; values are read on demand). The
// default is deliberately generous so that typical request/response bodies stay
// inline, where Compact reclaims their dead versions — blobs.dat is append-only
// and not yet compacted, so moderate, frequently-overwritten values are better
// kept inline. Reserve blobs for genuinely large payloads where avoiding a
// recopy on every compaction matters.
const DefaultBlobThreshold = 64 << 10 // 64 KiB

const (
	mainFile = "main.jsonl"
	hintFile = "main.jsonl.hint"
)

// Options configures a DB.
type Options struct {
	// Dir is the directory holding the store's files. Required.
	Dir string

	// BlobThreshold is the value size (in bytes) at or above which a value is
	// written to the blob side file rather than inline in main.jsonl. A value
	// of 0 selects DefaultBlobThreshold; a negative value disables blobs (all
	// values stored inline).
	BlobThreshold int

	// SyncEveryWrite fsyncs the main log on every write (power-loss durable,
	// but each write pays the hardware fsync, ~ms). When false, appends still
	// go straight to the OS via write(2) — there is no in-process buffer — so
	// a PROCESS crash (panic, kill -9, exit without Close) loses nothing; only
	// an OS crash / power loss can drop the most recent writes.
	SyncEveryWrite bool

	// HintEveryN rewrites the index hint file after this many writes (in
	// addition to on Close and after compaction). A value of 0 disables
	// periodic hint rewrites; the hint is still written on Close.
	HintEveryN int

	// Indexes declares the secondary indexes to maintain. They are rebuilt at
	// Open from the index values persisted in each record, so the same set
	// should be declared on every Open of a store.
	Indexes []IndexDef
}

// ErrClosed is returned when operating on a closed DB.
var ErrClosed = errors.New("nteedb: database is closed")

// DB is an open store. It is safe for concurrent use.
type DB struct {
	opts     Options
	mainPath string
	hintPath string
	blobPath string
	metaPath string

	mu     sync.RWMutex
	lock   *os.File   // exclusive flock handle enforcing a single writer process
	main   *mainLog   // append writer for main.jsonl (the main table)
	rf     *os.File   // read handle for main.jsonl (ReadAt is concurrency-safe)
	blobs  *blobStore // large-value side file
	pk     *pkIndex   // in-memory primary-key index
	writes int        // writes since the last hint rewrite
	closed bool

	// Async periodic hint machinery. The periodic hint rewrite (every
	// HintEveryN writes) runs in a background goroutine off the write path;
	// Close/Compact/range-delete hints stay synchronous checkpoints. Lock
	// order: foreground db.mu → hintMu; the background writer takes hintMu
	// only (never db.mu), so Close can wait on hintWG while holding db.mu.
	hintMu   sync.Mutex     // serializes all hint-file writes (async + sync)
	hintGen  atomic.Uint64  // bumped by every sync hint write; stale-snapshot guard
	hintBusy atomic.Bool    // single-flight: at most one async hint writer
	hintWG   sync.WaitGroup // lets Close wait out an in-flight writer

	// Secondary indexes (declared via Options.Indexes).
	indexDefs   []IndexDef
	secIndexes  map[string]*secIndex // name -> index
	prospective map[string]bool      // indexes not yet back-filled over pre-existing records
	dropped     map[string]ValueKind // soft-dropped indexes still lingering in records
}

// Open opens (creating if necessary) the store in opts.Dir.
func Open(opts Options) (*DB, error) {
	if opts.Dir == "" {
		return nil, errors.New("nteedb: Options.Dir is required")
	}
	if opts.BlobThreshold == 0 {
		opts.BlobThreshold = DefaultBlobThreshold
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}

	// Enforce a single writer process before touching any store file. The lock
	// is released on every failed-Open path below, on Close, and automatically
	// by the kernel if the process dies.
	lock, err := acquireLock(opts.Dir)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			_ = lock.Close()
		}
	}()

	db := &DB{
		opts:        opts,
		lock:        lock,
		mainPath:    filepath.Join(opts.Dir, mainFile),
		hintPath:    filepath.Join(opts.Dir, hintFile),
		blobPath:    filepath.Join(opts.Dir, blobFile),
		metaPath:    filepath.Join(opts.Dir, metaFile),
		pk:          newPkIndex(),
		indexDefs:   opts.Indexes,
		secIndexes:  make(map[string]*secIndex, len(opts.Indexes)),
		prospective: make(map[string]bool),
		dropped:     make(map[string]ValueKind),
	}
	for _, def := range opts.Indexes {
		if def.Name == "" {
			return nil, errors.New("nteedb: secondary index name is required")
		}
		if _, dup := db.secIndexes[def.Name]; dup {
			return nil, fmt.Errorf("nteedb: duplicate index name %q", def.Name)
		}
		db.secIndexes[def.Name] = newSecIndex(def)
	}

	// Rebuild the index: load the hint (if any) and replay only the log tail
	// past its watermark; otherwise full-scan. Either way, self-heal a torn tail.
	if err := db.load(); err != nil {
		return nil, err
	}

	// Adopt the declared schema (never rejected) and persist it. An index that is
	// new or kind-changed relative to the prior meta — on a store that already
	// has records — is "prospective": it covers only future writes until Reindex.
	if err := db.adoptSchema(); err != nil {
		return nil, err
	}

	lg, err := openMainLog(db.mainPath, opts.SyncEveryWrite)
	if err != nil {
		return nil, err
	}
	rf, err := os.Open(db.mainPath)
	if err != nil {
		_ = lg.close()
		return nil, err
	}
	blobs, err := openBlobs(db.blobPath)
	if err != nil {
		_ = lg.close()
		_ = rf.Close()
		return nil, err
	}
	db.main = lg
	db.rf = rf
	db.blobs = blobs
	opened = true
	return db, nil
}

// load rebuilds the in-memory index. It uses the hint file as a fast path —
// loading its (sorted) entries and replaying only the log tail past the hint's
// watermark — and falls back to a full scan if the hint is missing, corrupt, or
// claims to cover more than the log actually contains.
func (db *DB) load() error {
	from := int64(0)
	if entries, covers, ok := loadIndexHint(db.hintPath); ok {
		if info, err := os.Stat(db.mainPath); err == nil && covers <= info.Size() {
			// Rebuild both the primary index and the secondary indexes from the
			// hint snapshot (entries are already sorted by key, so pk.load takes
			// the btree's fast bulk path).
			db.pk = newPkIndex()
			for _, he := range entries {
				db.pk.load(pkEntry{key: he.Key, off: he.Off, n: he.N, ix: he.IX})
				if len(he.IX) > 0 {
					db.insertSecLocked(he.Key, he.IX)
				}
			}
			from = covers
		}
		// Otherwise the hint is stale/ahead of the log: ignore it and full-scan.
	}
	return db.replayTail(from)
}

// replayTail scans the main log from byte offset `from`, applying each record to
// the index, then truncates any torn final line left by a crash mid-append.
//
// A record whose blob ref points past the end of blobs.dat is treated the same
// as a torn line: writes fsync the blob before appending the referencing main
// record, so a dangling ref can only mean a power loss persisted main-log pages
// that were never acknowledged durable — the record (and everything after it)
// is part of the lost tail.
func (db *DB) replayTail(from int64) error {
	blobSize := int64(0)
	if info, err := os.Stat(db.blobPath); err == nil {
		blobSize = info.Size()
	}
	end, err := scanMainLog(db.mainPath, from, func(r record, off int64, n int32) error {
		if r.Blob != nil && r.Blob.Off+int64(r.Blob.Size) > blobSize {
			return errStopScan // dangling blob ref → start of the torn tail
		}
		if r.isTombstone() {
			// Retract first: the retraction reads ix off the primary entry.
			db.retractSecLocked(r.Key)
			db.pk.remove(r.Key)
		} else {
			db.pk.upsert(pkEntry{key: r.Key, off: off, n: n})
			db.refreshSecLocked(r.Key, r.IX)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(db.mainPath); statErr == nil && end < info.Size() {
		if err := os.Truncate(db.mainPath, end); err != nil {
			return err
		}
	}
	return nil
}

// adoptSchema records the declared index set in meta.json and marks which
// indexes are prospective (new/kind-changed on a store that already has records,
// so not yet covering those older records). Schema changes are never rejected.
func (db *DB) adoptSchema() error {
	prior, _ := loadMeta(db.metaPath)
	priorByName := make(map[string]metaIndex, len(prior.Indexes))
	for _, mi := range prior.Indexes {
		priorByName[mi.Name] = mi
	}
	hadData := db.pk.len() > 0

	declared := make(map[string]bool, len(db.indexDefs))
	out := make([]metaIndex, 0, len(db.indexDefs)+len(prior.Indexes))
	for _, def := range db.indexDefs {
		declared[def.Name] = true
		complete := false
		if mi, ok := priorByName[def.Name]; ok && mi.Kind == def.Kind.String() && !mi.Dropped {
			complete = mi.Complete // carry prior completeness forward
		} else {
			// New index, changed kind, or re-added after a drop: complete only
			// if there is no pre-existing data that would need back-filling.
			complete = !hadData
		}
		if !complete {
			db.prospective[def.Name] = true
		}
		out = append(out, metaIndex{Name: def.Name, Kind: def.Kind.String(), Complete: complete})
	}

	// Soft-drop: any prior index no longer declared is kept as a tombstone so
	// its lingering data stays observable and is preserved by Compact until a
	// Reindex purges it.
	for _, mi := range prior.Indexes {
		if declared[mi.Name] {
			continue
		}
		if k, ok := parseKind(mi.Kind); ok {
			db.dropped[mi.Name] = k
		}
		mi.Dropped = true
		out = append(out, mi)
	}
	return writeMeta(db.metaPath, out)
}

// writeHintLocked is the synchronous hint checkpoint (Close, Compact, range
// delete): it flushes the log so the watermark reflects durable data, then
// atomically rewrites the hint. Bumping hintGen first invalidates any
// in-flight async snapshot so a stale hint can never land after this fresh
// one. Callers must hold db.mu.
func (db *DB) writeHintLocked() error {
	db.hintGen.Add(1)
	db.hintMu.Lock()
	defer db.hintMu.Unlock()
	// Flush blobs first: a main record may reference a blob, so the blob must be
	// durable before the watermark declares that record covered.
	if db.blobs != nil {
		if err := db.blobs.flush(); err != nil {
			return err
		}
	}
	if err := db.main.flush(); err != nil {
		return err
	}
	if err := writeIndexHint(db.hintPath, db.pk, db.main.size); err != nil {
		return err
	}
	db.writes = 0
	return nil
}

// hintSnapshot is a consistent view of everything the background hint writer
// needs, taken under db.mu. pk is an O(1) copy-on-write clone of the primary
// tree — safe to iterate lock-free while the live tree keeps mutating — and
// the ix maps it references are never mutated in place (see refreshSecLocked).
type hintSnapshot struct {
	pk     *pkIndex
	covers int64
	gen    uint64
	main   *mainLog
	blobs  *blobStore
	path   string
}

// maybeWriteHintLocked spawns a background hint rewrite once HintEveryN writes
// have accumulated, so the periodic hint never stalls the caller's Put with
// fsyncs and a full index serialization. Single-flight: while one rewrite is
// in flight, further triggers are skipped and the write counter keeps
// accumulating until the next eligible write. The hint is a disposable
// fast-boot optimization, so everything here is best-effort. Callers must
// hold db.mu.
func (db *DB) maybeWriteHintLocked() {
	if db.opts.HintEveryN <= 0 || db.writes < db.opts.HintEveryN {
		return
	}
	if !db.hintBusy.CompareAndSwap(false, true) {
		return // one rewrite already in flight; coalesce
	}
	snap := hintSnapshot{
		pk:     db.pk.copy(), // O(1) COW clone — no per-entry copying under the lock
		covers: db.main.size,
		gen:    db.hintGen.Load(),
		main:   db.main,
		blobs:  db.blobs,
		path:   db.hintPath,
	}
	db.writes = 0
	db.hintWG.Add(1)
	go db.writeHintAsync(snap)
}

// writeHintAsync flushes the data files and writes the snapshotted hint from a
// background goroutine. It never takes db.mu: the flushes are bare fsyncs on
// fds whose Go-side state is only mutated under db.mu, and the snapshot is
// self-contained. Any error (e.g. Compact closed the old fd under us) aborts —
// a missed hint only costs a slower next boot.
func (db *DB) writeHintAsync(snap hintSnapshot) {
	defer db.hintWG.Done()
	defer db.hintBusy.Store(false)

	// Durability ordering as in the sync path: blobs, then log, so covers only
	// ever claims bytes whose referenced blobs are durable too.
	if snap.blobs != nil {
		if err := snap.blobs.flush(); err != nil {
			return
		}
	}
	if err := snap.main.flush(); err != nil {
		return
	}

	db.hintMu.Lock()
	defer db.hintMu.Unlock()
	// A synchronous checkpoint (Close/Compact/range delete) may have written a
	// fresher hint while we were flushing; never clobber it with this stale
	// snapshot.
	if db.hintGen.Load() != snap.gen {
		return
	}
	_ = writeIndexHint(snap.path, snap.pk, snap.covers)
}

// Put stores value under key. Any secondary indexes with an Extract function
// derive their value from the record automatically.
func (db *DB) Put(key string, value []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	return db.writeLocked(key, value, nil)
}

// PutIndexed stores value under key with explicit secondary index values (e.g.
// {"traceId": "abc", "status": 200}). Explicit values take precedence over any
// index Extract function. An unknown index name or a value of the wrong kind is
// an error and nothing is written.
func (db *DB) PutIndexed(key string, value []byte, idx IndexValues) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	return db.writeLocked(key, value, idx)
}

// Get returns the value stored under key. ok is false if the key is absent.
//
// Note: this holds db.mu.RLock across the pread, and Compact holds the exclusive
// Lock for its whole duration — so a read that arrives during a Compact stalls
// until it finishes. Fine when Compact is occasional (its whole point is to run
// rarely); a compaction-heavy workload would want online compaction (do the
// expensive buildRewrite off the exclusive lock via a COW index clone, take the
// lock only to replay the tail and swap). See BenchmarkGetContention.
func (db *DB) Get(key string) (value []byte, ok bool, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false, ErrClosed
	}
	e, ok := db.pk.get(key)
	if !ok {
		return nil, false, nil
	}
	rec, err := db.readRecord(e)
	if err != nil {
		return nil, false, err
	}
	if rec.Blob != nil {
		v, err := db.blobs.readAt(*rec.Blob)
		if err != nil {
			return nil, false, err
		}
		return v, true, nil
	}
	return rec.Value, true, nil
}

// GetMany returns the values for keys, aligned to the input order: values[i] is
// the value for keys[i] and found[i] reports whether that key existed (a missing
// key yields found[i]=false, values[i]=nil). It reads every record under a
// single read-lock — the batched counterpart to Get for callers (e.g. a
// records-by-index search) that resolve many keys at once.
//
// Note: the reads are intentionally sequential. A goroutine fan-out measured
// 2–3× slower — a cached pread is cheaper than the coordination overhead, and
// the batch cost sits in the FFI/JSON layers, not these reads.
func (db *DB) GetMany(keys []string) (values [][]byte, found []bool, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, nil, ErrClosed
	}
	values = make([][]byte, len(keys))
	found = make([]bool, len(keys))
	for i, key := range keys {
		e, ok := db.pk.get(key)
		if !ok {
			continue
		}
		rec, err := db.readRecord(e)
		if err != nil {
			return nil, nil, err
		}
		if rec.Blob != nil {
			v, err := db.blobs.readAt(*rec.Blob)
			if err != nil {
				return nil, nil, err
			}
			values[i], found[i] = v, true
			continue
		}
		values[i], found[i] = rec.Value, true
	}
	return values, found, nil
}

// useBlobFor reports whether a value of the given size should be stored in the
// blob side file rather than inline. A non-positive BlobThreshold disables blobs.
func (db *DB) useBlobFor(size int) bool {
	return db.opts.BlobThreshold > 0 && size >= db.opts.BlobThreshold
}

// Stats is a point-in-time snapshot of store size.
type Stats struct {
	Records   int   `json:"records"`   // live records (primary keys)
	MainBytes int64 `json:"mainBytes"` // main.jsonl size — includes dead records until Compact
	BlobBytes int64 `json:"blobBytes"` // blobs.dat size — includes orphaned blobs
}

// Stats returns the store's live record count and on-disk file sizes. Cheap:
// every value is already tracked in memory (no I/O, no scans).
func (db *DB) Stats() Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return Stats{}
	}
	s := Stats{Records: db.pk.len()}
	if db.main != nil {
		s.MainBytes = db.main.size
	}
	if db.blobs != nil {
		s.BlobBytes = db.blobs.size
	}
	return s
}

// Has reports whether key is present without reading its value.
func (db *DB) Has(key string) bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return false
	}
	_, ok := db.pk.get(key)
	return ok
}

// Delete removes key. Deleting an absent key is a no-op.
func (db *DB) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if _, ok := db.pk.get(key); !ok {
		return nil
	}
	if _, _, err := db.main.append(record{Key: key, Deleted: true}); err != nil {
		return err
	}
	// Retract first: the retraction reads ix off the primary entry.
	db.retractSecLocked(key)
	db.pk.remove(key)
	db.writes++
	db.maybeWriteHintLocked()
	return nil
}

// PrefixScan returns, in sorted order, every key beginning with prefix. An empty
// prefix returns all keys.
func (db *DB) PrefixScan(prefix string) ([]string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	es := db.pk.prefix(prefix)
	keys := make([]string, len(es))
	for i, e := range es {
		keys[i] = e.key
	}
	return keys, nil
}

// readRecord reads and decodes the record located by e from the main log.
func (db *DB) readRecord(e pkEntry) (record, error) {
	buf := make([]byte, e.n)
	if _, err := db.rf.ReadAt(buf, e.off); err != nil {
		return record{}, err
	}
	return unmarshalRecord(bytes.TrimSuffix(buf, []byte{'\n'}))
}

// Close flushes pending state and releases resources. The DB must not be used
// afterward.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true

	// Wait out any in-flight background hint writer before closing the file
	// handles it flushes. Safe while holding db.mu: the writer never takes it.
	db.hintWG.Wait()

	var err error
	if db.main != nil {
		// Write a final hint so the next boot is fast (this also flushes the log).
		if e := db.writeHintLocked(); e != nil {
			err = e
		}
		if e := db.main.close(); e != nil && err == nil {
			err = e
		}
	}
	if db.rf != nil {
		if e := db.rf.Close(); e != nil && err == nil {
			err = e
		}
	}
	if db.blobs != nil {
		if e := db.blobs.close(); e != nil && err == nil {
			err = e
		}
	}
	// Release the single-writer lock last, once the store's files are closed.
	if db.lock != nil {
		if e := db.lock.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
