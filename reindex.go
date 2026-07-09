package nteedb

// Reindex rewrites every live record, re-deriving secondary index values via
// each declared index's Extract function — back-filling indexes that were added
// (or whose kind changed) after records already existed. It also reclaims dead
// space and drops ix fields of undeclared indexes. The store is briefly
// read-only (write lock held); it is O(table) and reads every record's value
// (including blobs), so it is a deliberate, occasional operation.
//
// Only Extract-based indexes can be back-filled. Explicit-value indexes (no
// Extract) cannot — their historical values were never recorded anywhere — and
// remain prospective afterward.
func (db *DB) Reindex() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if err := db.rewriteLocked(db.reindexTransform); err != nil {
		return err
	}
	return db.markReindexedLocked()
}

// reindexTransform recomputes a record's ix: it keeps the declared values
// already present (including explicit-value indexes) and re-runs every
// Extract-based index over the record's value.
func (db *DB) reindexTransform(rec record) (record, error) {
	newIX := db.filterIX(rec.IX)

	var value []byte
	if rec.Blob != nil {
		v, err := db.blobs.readAt(*rec.Blob)
		if err != nil {
			return rec, err
		}
		value = v
	} else {
		value = rec.Value
	}

	for _, def := range db.indexDefs {
		if def.Extract == nil {
			continue
		}
		v, ok := def.Extract(rec.Key, value)
		if !ok {
			continue
		}
		if _, err := db.secIndexes[def.Name].makeEntry(v, rec.Key); err != nil {
			return rec, err
		}
		if newIX == nil {
			newIX = make(map[string]any)
		}
		newIX[def.Name] = v
	}
	rec.IX = newIX
	return rec, nil
}

// markReindexedLocked updates meta and prospective state after a Reindex:
// Extract-based indexes are now complete; explicit-value indexes keep their
// prior prospective status (Reindex cannot back-fill them). Callers hold db.mu.
func (db *DB) markReindexedLocked() error {
	hasExtract := make(map[string]bool, len(db.indexDefs))
	for _, def := range db.indexDefs {
		if def.Extract != nil {
			hasExtract[def.Name] = true
		}
	}
	out := make([]metaIndex, 0, len(db.indexDefs))
	for _, def := range db.indexDefs {
		var complete bool
		if hasExtract[def.Name] {
			complete = true
			delete(db.prospective, def.Name)
		} else {
			complete = !db.prospective[def.Name]
		}
		out = append(out, metaIndex{Name: def.Name, Kind: def.Kind.String(), Complete: complete})
	}
	// Reindex purges soft-dropped indexes: their data was stripped from the
	// rewritten records (active-only filter) and they are dropped from meta here.
	db.dropped = make(map[string]ValueKind)
	return writeMeta(db.metaPath, out)
}
