package nteedb

// Range deletes remove a contiguous span of primary keys in one pass. Keys are
// compared lexically (Go string order) over the whole key, so callers control
// what a range means via their key design (e.g. a zero-padded, time-ordered
// suffix makes "less than cutoff" mean "older than cutoff").
//
// Like Delete, each removed key gets a tombstone appended to the log, so the
// deletion is durable and crash-safe; the in-memory primary and secondary
// indexes are trimmed, and one hint rewrite snapshots the trimmed state. The
// on-disk log is not reclaimed here — that is Compact's job.

// RemoveByPkLess deletes every key strictly less than cutoff (the cutoff key
// itself is kept). It returns the number of keys removed.
func (db *DB) RemoveByPkLess(cutoff string) (int, error) {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return 0, ErrClosed
	}
	var doomed []pkEntry
	db.pk.scan(func(e pkEntry) bool {
		if e.key >= cutoff {
			return false
		}
		doomed = append(doomed, e)
		return true
	})
	return db.removePkEntriesLocked(doomed)
}

// RemoveByPkGreater deletes every key strictly greater than cutoff (the cutoff
// key itself is kept). It returns the number of keys removed.
func (db *DB) RemoveByPkGreater(cutoff string) (int, error) {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return 0, ErrClosed
	}
	var doomed []pkEntry
	db.pk.ascendFrom(cutoff, func(e pkEntry) bool {
		if e.key > cutoff {
			doomed = append(doomed, e)
		}
		return true
	})
	return db.removePkEntriesLocked(doomed)
}

// removePkEntriesLocked removes the given primary entries (snapshotted by the
// caller — they must not alias live tree state), retracting their secondary
// entries in one sweep per index and appending a tombstone per key. Callers
// must hold db.mu.
func (db *DB) removePkEntriesLocked(doomed []pkEntry) (int, error) {
	if len(doomed) == 0 {
		return 0, nil
	}

	// Append all tombstones first, before any in-memory change. A mid-loop
	// append failure then leaves the in-memory indexes untouched, and the log
	// (the source of truth) still converges on the next replay — matching
	// Delete's append-then-mutate ordering.
	for _, e := range doomed {
		if _, _, err := db.main.append(record{Key: e.key, Deleted: true}); err != nil {
			return 0, err
		}
	}

	// Retract secondary entries in one sweep per index, then drop the primary
	// entries (each carries its ix, so retraction needs no separate map).
	doomedSet := make(map[string]struct{}, len(doomed))
	for _, e := range doomed {
		doomedSet[e.key] = struct{}{}
	}
	for _, si := range db.secIndexes {
		si.removePKs(doomedSet)
	}
	for _, e := range doomed {
		db.pk.remove(e.key)
	}

	// Force a hint rewrite so the trimmed state is the fast-boot snapshot and
	// covers advances past the tombstones we just appended.
	db.writes += len(doomed)
	if err := db.writeHintLocked(); err != nil {
		return 0, err
	}
	return len(doomed), nil
}
