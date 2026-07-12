package nteedb

import (
	"bytes"
	"errors"
	"math"
)

// ErrNotCounter is returned by Incr when the key holds a value that was not
// written as a counter (plain Put values, blobs, documents — any non-counter
// type). Counters are int64-only and created only by Incr itself; an existing
// value is never reinterpreted as a number.
var ErrNotCounter = errors.New("nteedb: key holds a non-counter value")

// ErrCounterOverflow is returned when an increment would take the counter
// past the int64 range. The stored value is left unchanged.
var ErrCounterOverflow = errors.New("nteedb: counter overflows int64")

// counterWidth is the exact on-disk size of a counter value: an explicit sign
// character followed by 19 zero-padded digits (int64's max and min magnitudes
// are both 19 digits). Every int64 has exactly one such form, which is what
// makes same-offset in-place replacement safe: an increment never shifts a
// byte, and even a torn write can only mix old/new sign/digit characters —
// still a structurally valid JSON string, so crash recovery is never tricked
// into truncating records that follow it. Floats are excluded by design:
// they have no bounded canonical text form and drift under repeated adds.
const counterWidth = 20

// formatCounter renders v in the fixed-width counter form.
func formatCounter(v int64) []byte {
	b := make([]byte, counterWidth)
	sign := byte('+')
	mag := uint64(v)
	if v < 0 {
		sign = '-'
		mag = uint64(-(v + 1)) + 1 // negate without overflowing on MinInt64
	}
	b[0] = sign
	for i := counterWidth - 1; i >= 1; i-- {
		b[i] = byte('0' + mag%10)
		mag /= 10
	}
	return b
}

// parseCounter parses the fixed-width counter form. ok is false for anything
// that is not exactly a sign character plus 19 digits within int64 range.
func parseCounter(b []byte) (int64, bool) {
	if len(b) != counterWidth || (b[0] != '+' && b[0] != '-') {
		return 0, false
	}
	var mag uint64
	for _, c := range b[1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		mag = mag*10 + uint64(c-'0')
	}
	// 19 digits max out at 9999999999999999999 < 2^64, so mag cannot have
	// wrapped; only the int64 bound needs checking.
	if b[0] == '+' {
		if mag > math.MaxInt64 {
			return 0, false
		}
		return int64(mag), true
	}
	if mag > uint64(math.MaxInt64)+1 {
		return 0, false
	}
	return -int64(mag-1) - 1, true // negate without overflowing on MinInt64
}

// Incr atomically adds delta (negative to decrement) to the int64 counter at
// key and returns the new value. A missing key initializes to 0 before delta
// is applied. Counters are a distinct value type: Incr on a key holding any
// other value returns ErrNotCounter, and a Put on a counter key demotes it to
// a plain value. An increment past the int64 range returns ErrCounterOverflow
// and leaves the value unchanged.
//
// Counters never participate in secondary indexes — like every immediate
// value they are primary-key-only (find them by key or prefix scan). Because
// nothing derives state from a counter's value, and the value is stored in a
// fixed-width form (see counterWidth), an increment overwrites the digits in
// place: no log growth, no index churn. Only initialization (and the
// defensive format-mismatch path) appends a record.
func (db *DB) Incr(key string, delta int64) (int64, error) {
	db.lockWrite()
	defer db.mu.Unlock()
	if db.closed {
		return 0, ErrClosed
	}
	return db.incrLocked(key, delta)
}

func (db *DB) incrLocked(key string, delta int64) (int64, error) {
	e, ok := db.pk.get(key)
	if !ok {
		// Missing (or previously deleted — tombstones drop the pk entry):
		// initialize to 0 and apply delta via the normal append path, which
		// also creates the pk entry.
		return delta, db.incrAppendLocked(key, delta)
	}

	rec, err := db.readRecord(e)
	if err != nil {
		return 0, err
	}
	if rec.Blob != nil || !rec.Counter {
		return 0, ErrNotCounter
	}
	cur, ok := parseCounter(rec.Value)
	if !ok {
		return 0, ErrNotCounter // flag set but bytes malformed: refuse to touch
	}
	if (delta > 0 && cur > math.MaxInt64-delta) || (delta < 0 && cur < math.MinInt64-delta) {
		return 0, ErrCounterOverflow
	}
	next := cur + delta

	// In-place fast path. Counters carry no index entries (they are
	// primary-key-only), so nothing can go stale under a byte patch. The
	// e.ix check is a defensive invariant guard: a counter record with index
	// entries should not exist, but if one ever does, the append fallback
	// rewrites it with nil ix, retracting the stale entries.
	if len(e.ix) == 0 {
		line, err := marshalRecord(rec)
		if err == nil && int32(len(line))+1 == e.n {
			// Locate the value inside the marshaled line. The pattern cannot
			// occur inside the escaped key field: its quotes would be \" there.
			pat := append([]byte(`"s":"`), rec.Value...)
			pat = append(pat, '"')
			if idx := bytes.Index(line, pat); idx >= 0 {
				if _, err := db.patcher.WriteAt(formatCounter(next), e.off+int64(idx)+5); err != nil {
					return 0, err
				}
				if db.opts.SyncEveryWrite {
					if err := db.patcher.Sync(); err != nil {
						return 0, err
					}
				}
				return next, nil
			}
		}
		// Any mismatch (marshal drift, length, pattern): fall through to the
		// always-correct append path.
	}
	return next, db.incrAppendLocked(key, next)
}

// incrAppendLocked appends the counter's new value as a fresh record. Counters
// are pure key:value pairs — no secondary index derivation, no eviction
// bookkeeping — so this is a bare append with the counter flag set (nil ix
// also retracts any stale index entries if a legacy record carried them).
// Callers must hold db.mu.
func (db *DB) incrAppendLocked(key string, v int64) error {
	if err := db.appendRecordLocked(key, formatCounter(v), nil, db.opts.SyncEveryWrite, true); err != nil {
		return err
	}
	db.writes++
	db.maybeWriteHintLocked()
	return nil
}
