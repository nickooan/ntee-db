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
	if db.blobs != nil {
		_ = db.blobs.close()
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
// Reads stay live throughout the rebuild: Compact holds only the writer gate
// (writes pend until it finishes) and takes the exclusive lock just for the
// brief final swap. See TestReadsProceedDuringCompactRewrite.
//
// Only the main log is rewritten; blob references are preserved and blobs.dat is
// left untouched, keeping the swap a single atomic rename (crash-safe). The
// containing directory is deliberately not fsynced after the rename: if power
// is lost before the rename metadata is durable, the old main.jsonl simply
// remains and the leftover .compact file is ignored on the next open.
func (db *DB) Compact() error {
	db.wmu.Lock()
	defer db.wmu.Unlock()
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

// rewriteGated rewrites the main log keeping only live records, applying
// transform to each, then atomically swaps the file in and rebuilds in-memory
// state. Callers must hold db.wmu (the writer gate) — that alone makes the
// long buildRewrite phase safe: writers are excluded, so db.pk / db.rf /
// db.secIndexes are only read concurrently (by readers under mu.RLock, which
// keep working). db.mu is taken exclusively only for the swap at the end.
// (blobs.dat is unchanged, so db.blobs stays open as-is.)
func (db *DB) rewriteGated(transform func(record) (record, error)) error {
	// closed cannot flip while we hold wmu (Close takes it too), so one check
	// up front suffices.
	db.mu.RLock()
	closed := db.closed
	db.mu.RUnlock()
	if closed {
		return ErrClosed
	}

	newMain := db.mainPath + ".compact"
	_ = os.Remove(newMain)

	// Long phase — readers stay live.
	newIdx, err := db.buildRewrite(newMain, transform)
	if err != nil {
		_ = os.Remove(newMain)
		return err
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

	return db.writeHintLocked()
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
