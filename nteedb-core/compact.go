package nteedb

import (
	"bufio"
	"fmt"
	"os"
)

// openMainLogFn is a seam so tests can inject a reopen failure into the
// compaction swap (the fail-stop path below is otherwise unreachable).
var openMainLogFn = openMainLog

// rewriteRecordHook, when non-nil, runs once per record inside buildRewrite —
// a test seam to observe (and pause) the gated rebuild phase.
var rewriteRecordHook func()

// failStopLocked permanently disables the store after an unrecoverable error
// mid-compaction-swap: the old main handles are already closed and no usable
// replacements exist. Marking the store closed makes every later call return
// ErrClosed (instead of "file already closed" confusion), and the remaining
// resources are released since Close() would now be a no-op. Callers hold db.mu.
func (db *DB) failStopLocked(cause error) error {
	db.closed = true
	db.main, db.rf = nil, nil
	db.hintWG.Wait() // let any in-flight background hint writer finish first
	for _, bs := range db.blobs {
		_ = bs.close()
	}
	if db.lock != nil {
		_ = db.lock.Close()
	}
	return fmt.Errorf("nteedb: store disabled after failed compaction swap: %w", cause)
}

// Compact rewrites the main log to contain only live records (one per key, with
// superseded versions and tombstones dropped), reclaiming space. It is also
// schema-aware: each record's ix is filtered to the currently declared indexes,
// so fields of dropped indexes are swept away. Cost is O(live bytes): every
// live record line — including inline values — is read and rewritten (only
// blob CONTENTS are spared; their refs are copied verbatim).
//
// Reads stay live throughout the rebuild: Compact raises the compaction gate
// (writes pend until it finishes — see lockWrite) and takes the exclusive
// lock just for the brief final swap. See TestReadsProceedDuringCompactRewrite.
//
// Only the main log is rewritten; blob references are preserved and blobs.dat is
// left untouched, keeping the swap a single atomic rename (crash-safe). The
// containing directory is deliberately not fsynced after the rename: if power
// is lost before the rename metadata is durable, the old main.jsonl simply
// remains and the leftover .compact file is ignored on the next open.
func (db *DB) Compact() error {
	return db.rewriteGated(db.compactTransform)
}

// compactTransform keeps a record as-is except for filtering its ix down to the
// *known* indexes — active or soft-dropped (no value reads). Soft-dropped index
// data is deliberately preserved here; only Reindex purges it.
func (db *DB) compactTransform(rec record) (record, error) {
	rec.IX = db.filterIXKnown(rec.IX)
	return rec, nil
}

// filterIX returns the subset of ix whose names are currently *active* indexes
// AND whose values match the index's declared kind (used by Reindex, which
// purges soft-dropped indexes). The kind check matters when an explicit-value
// index's kind changed: without it the old wrong-kind value would be rewritten
// into the record forever — un-indexable (makeEntry rejects it at boot) yet
// never cleaned, since Reindex cannot re-derive explicit values.
func (db *DB) filterIX(ix map[string]any) map[string]any {
	if len(ix) == 0 {
		return nil
	}
	out := make(map[string]any, len(ix))
	for name, v := range ix {
		si, ok := db.secIndexes[name]
		if !ok {
			continue
		}
		if _, err := si.makeEntry(v, ""); err != nil {
			continue // wrong kind for the current declaration: drop it
		}
		out[name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// filterIXKnown returns the subset of ix whose names are known — active OR
// soft-dropped — stripping only truly-unknown names (used by Compact).
func (db *DB) filterIXKnown(ix map[string]any) map[string]any {
	if len(ix) == 0 {
		return nil
	}
	out := make(map[string]any, len(ix))
	for name, v := range ix {
		_, active := db.secIndexes[name]
		_, dropped := db.dropped[name]
		if active || dropped {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// raiseGate raises the compaction gate: writers park in lockWrite until
// lowerGate runs; readers are unaffected. It waits out any rebuild already in
// flight, so at most one runs at a time. Returns ErrClosed on a closed store
// (closed cannot flip while the gate is up — Close waits in lockWrite too).
func (db *DB) raiseGate() error {
	db.lockWrite()
	if db.closed {
		db.mu.Unlock()
		return ErrClosed
	}
	db.compacting = true
	db.mu.Unlock()
	return nil
}

// lowerGate lowers the compaction gate and wakes every parked writer. It must
// run on EVERY exit path of a raised gate (defer it right after raiseGate).
func (db *DB) lowerGate() {
	db.mu.Lock()
	db.compacting = false
	db.writable.Broadcast()
	db.mu.Unlock()
}

// blobRewrite is the new blob generation being written alongside a Relieve
// rewrite; nil for plain Compact/Reindex (their blob refs are copied verbatim).
type blobRewrite struct {
	gen   int
	store *blobStore
}

// rewriteGated rewrites the main log keeping only live records, applying
// transform to each, then atomically swaps the file in and rebuilds in-memory
// state, all behind the compaction gate.
func (db *DB) rewriteGated(transform func(record) (record, error)) error {
	if err := db.raiseGate(); err != nil {
		return err
	}
	defer db.lowerGate()
	return db.rewriteBody(transform, nil)
}

// rewriteBody is the rebuild + swap shared by Compact/Reindex/Relieve.
// Callers hold the raised compaction gate: every mutation waits in lockWrite,
// which alone makes the long buildRewrite phase safe — db.pk / db.rf /
// db.secIndexes / db.blobs are only read concurrently (by readers under
// mu.RLock, which keep working). db.mu is held exclusively only for the swap
// at the end. br, when non-nil, is the new blob generation Relieve is
// writing; it is made durable before the main rename commits refs into it,
// and the swap retires every other generation (all refs now point at br).
func (db *DB) rewriteBody(transform func(record) (record, error), br *blobRewrite) error {
	newMain := db.mainPath + ".compact"
	_ = os.Remove(newMain)

	// Long phase — readers stay live, writers wait on the gate.
	newIdx, err := db.buildRewrite(newMain, transform)
	if err != nil {
		_ = os.Remove(newMain)
		return err
	}
	if br != nil {
		// Blob-before-record durability order, as on the write path: the new
		// generation must be durable before any committed record references it.
		if err := br.store.flush(); err != nil {
			_ = os.Remove(newMain)
			return err
		}
	}

	// Brief exclusive phase.
	db.mu.Lock()
	defer db.mu.Unlock()

	// Swap: close old main handles, atomically replace the file, reopen. Past
	// this point the old handles are gone — any failure below must fail-stop
	// (see failStopLocked): limping on would leave db.main/db.rf pointing at
	// closed files while db.closed stays false, wedging every later call with
	// confusing "file already closed" errors.
	_ = db.main.close()
	_ = db.rf.Close()
	if err := os.Rename(newMain, db.mainPath); err != nil {
		return db.failStopLocked(err)
	}

	lg, err := openMainLogFn(db.mainPath, db.opts.SyncEveryWrite)
	if err != nil {
		return db.failStopLocked(err)
	}
	rf, err := os.Open(db.mainPath)
	if err != nil {
		_ = lg.close()
		return db.failStopLocked(err)
	}
	db.main = lg
	db.rf = rf
	db.pk = newIdx
	db.rebuildSecLocked()
	db.writes = 0
	if br != nil {
		// Every live ref now points at the new generation: retire the others.
		// (A crash before these removals just leaves stray unreferenced files,
		// consolidated by the next Relieve.)
		for g, bs := range db.blobs {
			_ = bs.close()
			_ = os.Remove(blobGenPath(db.opts.Dir, g))
			delete(db.blobs, g)
		}
		db.blobs[br.gen] = br.store
		db.curGen = br.gen
	}

	return db.writeHintLocked()
}

// BlobsRelieve rewrites every blob into a fresh generation file, dropping
// orphaned blobs and consolidating any stray generations, then retires the
// old files. It is unconditional MECHANISM: the core applies no thresholds —
// callers (a server's policy loop, application code) decide WHEN to run it,
// typically from BlobUsage numbers. Because blob refs live in main-log lines
// and the main-log rename is the atomic commit point, it necessarily compacts
// the main log in the same pass. Same concurrency contract as Compact: reads
// stay live throughout; writes pend on the compaction gate. Cost is O(live
// bytes) INCLUDING blob contents (unlike Compact, which never reads them).
func (db *DB) BlobsRelieve() error {
	if err := db.raiseGate(); err != nil {
		return err
	}
	defer db.lowerGate()

	br := &blobRewrite{gen: db.curGen + 1}
	var err error
	if br.store, err = openBlobs(blobGenPath(db.opts.Dir, br.gen)); err != nil {
		return err
	}
	transform := func(rec record) (record, error) {
		rec, err := db.compactTransform(rec)
		if err != nil || rec.Blob == nil {
			return rec, err
		}
		val, err := db.blobReadAt(rec.Blob)
		if err != nil {
			return rec, err
		}
		ref, err := br.store.append(val)
		if err != nil {
			return rec, err
		}
		ref.Gen = br.gen
		rec.Blob = &ref
		return rec, nil
	}
	if err := db.rewriteBody(transform, br); err != nil {
		// Pre-commit failures leave the new generation unreferenced — remove
		// it. After a post-commit fail-stop (db.closed) the committed main log
		// DOES reference it: keep the file so the next Open recovers.
		db.mu.Lock()
		dead := db.closed
		db.mu.Unlock()
		if !dead {
			_ = br.store.close()
			_ = os.Remove(blobGenPath(db.opts.Dir, br.gen))
		}
		return err
	}
	return nil
}

// BlobUsage is a point-in-time measurement of blob-file occupancy — the input
// for deciding when to run BlobsRelieve.
type BlobUsage struct {
	TotalBytes    int64 `json:"totalBytes"`    // size of all generation files
	LiveBytes     int64 `json:"liveBytes"`     // bytes referenced by live records
	OrphanedBytes int64 `json:"orphanedBytes"` // TotalBytes - LiveBytes
	Generations   int   `json:"generations"`   // >1 means a crashed relieve left a stray file
}

// BlobUsage computes the store's blob occupancy by reading every live
// record's main-log LINE (blob contents are never read) under one read lock —
// the same pattern as GetMany. O(records) small preads: cheap enough for a
// periodic policy check, too expensive for a per-request stat.
func (db *DB) BlobUsage() (BlobUsage, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return BlobUsage{}, ErrClosed
	}
	u := BlobUsage{Generations: len(db.blobs)}
	for _, bs := range db.blobs {
		u.TotalBytes += bs.size
	}
	var scanErr error
	db.pk.scan(func(e pkEntry) bool {
		rec, err := db.readRecord(e)
		if err != nil {
			scanErr = err
			return false
		}
		if rec.Blob != nil {
			u.LiveBytes += int64(rec.Blob.Size)
		}
		return true
	})
	if scanErr != nil {
		return BlobUsage{}, scanErr
	}
	u.OrphanedBytes = u.TotalBytes - u.LiveBytes
	return u, nil
}

// buildRewrite writes a new main log at path containing only the current live
// records (in sorted key order), applying transform to each. It returns the new
// primary index; each entry carries its final ix values, from which the
// secondary indexes are rebuilt.
func (db *DB) buildRewrite(path string, transform func(record) (record, error)) (*pkIndex, error) {
	mf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(mf)

	newIdx := newPkIndex()
	var off int64
	var scanErr error
	db.pk.scan(func(e pkEntry) bool { // ascending key order → newIdx.load bulk path
		if rewriteRecordHook != nil {
			rewriteRecordHook()
		}
		rec, err := db.readRecord(e)
		if err != nil {
			scanErr = err
			return false
		}
		if rec, scanErr = transform(rec); scanErr != nil {
			return false
		}
		line, err := marshalRecord(rec)
		if err != nil {
			scanErr = err
			return false
		}
		line = append(line, '\n')
		if _, err := w.Write(line); err != nil {
			scanErr = err
			return false
		}
		n := int32(len(line))
		newIdx.load(pkEntry{key: e.key, off: off, n: n, ix: rec.IX})
		off += int64(n)
		return true
	})
	if scanErr != nil {
		_ = mf.Close()
		return nil, scanErr
	}

	if err := w.Flush(); err != nil {
		_ = mf.Close()
		return nil, err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()
		return nil, err
	}
	return newIdx, mf.Close()
}
