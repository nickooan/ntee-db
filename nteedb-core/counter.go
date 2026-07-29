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

// ErrNegativeAmount is returned by Topup and Take when given a negative
// amount. Their amounts are magnitudes with a fixed direction; a negative
// amount would silently invert the guard's meaning.
var ErrNegativeAmount = errors.New("nteedb: amount must be non-negative")

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
	next, _, err := db.counterUpdateLocked(key, func(cur int64) (int64, bool, error) {
		if (delta > 0 && cur > math.MaxInt64-delta) || (delta < 0 && cur < math.MinInt64-delta) {
			return 0, false, ErrCounterOverflow
		}
		return cur + delta, true, nil
	})
	return next, err
}

// Topup atomically adds up to amount (which must be >= 0) to the int64
// counter at key, clamped so the value never exceeds max, and returns how
// much of amount did NOT fit: 0 means the full amount applied, a positive
// overflow means the counter was filled to max (or already at/above it) and
// that many units were left over. A missing key is treated as 0: a topup
// that adds anything creates it, one that adds nothing (overflow == amount,
// or a zero amount) writes nothing. A counter already above max is left
// unchanged — Topup only ever adds.
//
// On any error the returned count is -1, never 0 — the number alone
// distinguishes error (-1), fully applied (0), and clamped (> 0).
func (db *DB) Topup(key string, amount, max int64) (int64, error) {
	if amount < 0 {
		return -1, ErrNegativeAmount
	}
	db.lockWrite()
	defer db.mu.Unlock()
	if db.closed {
		return -1, ErrClosed
	}
	var overflow int64
	_, _, err := db.counterUpdateLocked(key, func(cur int64) (int64, bool, error) {
		var applied int64
		switch {
		case cur >= max:
			applied = 0
		case cur > math.MaxInt64-amount || cur+amount > max:
			// Partial fill to max. max-cur cannot overflow in either arm:
			// the first implies cur >= 1 (amount <= MaxInt64), and in both
			// the room max-cur is positive and below amount <= MaxInt64.
			applied = max - cur
		default:
			applied = amount
		}
		overflow = amount - applied
		if applied == 0 {
			return 0, false, nil
		}
		return cur + applied, true, nil
	})
	if err != nil {
		return -1, err
	}
	return overflow, nil
}

// Take atomically subtracts amount (which must be >= 0) from the int64
// counter at key only if the result stays >= left, and reports whether it
// applied. A missing key is treated as 0, so a take of more than 0-left
// reports false without creating the key. A difference that would leave the
// int64 range is necessarily below left, so it reports false rather than an
// error.
func (db *DB) Take(key string, amount, left int64) (bool, error) {
	if amount < 0 {
		return false, ErrNegativeAmount
	}
	db.lockWrite()
	defer db.mu.Unlock()
	if db.closed {
		return false, ErrClosed
	}
	_, applied, err := db.counterUpdateLocked(key, func(cur int64) (int64, bool, error) {
		if cur < math.MinInt64+amount { // cur-amount underflows: certainly < left
			return 0, false, nil
		}
		next := cur - amount
		if next < left {
			return 0, false, nil
		}
		return next, true, nil
	})
	return applied, err
}

// counterUpdateLocked reads the counter at key (missing or deleted reads as
// 0), asks step for the next value, and — when step applies — writes it
// through the same in-place-patch / append machinery Incr uses. When step
// declines (apply false), nothing is written: no append, no pk entry for a
// missing key, no write/hint bookkeeping. Callers must hold db.mu.
func (db *DB) counterUpdateLocked(key string, step func(cur int64) (next int64, apply bool, err error)) (int64, bool, error) {
	e, ok := db.pk.get(key)
	if !ok {
		// Missing (or previously deleted — tombstones drop the pk entry):
		// treat as 0; an applied step creates the entry via the append path.
		next, apply, err := step(0)
		if err != nil || !apply {
			return next, false, err
		}
		return next, true, db.incrAppendLocked(key, next)
	}

	rec, err := db.readRecord(e)
	if err != nil {
		return 0, false, err
	}
	if rec.Blob != nil || !rec.Counter {
		return 0, false, ErrNotCounter
	}
	cur, ok := parseCounter(rec.Value)
	if !ok {
		return 0, false, ErrNotCounter // flag set but bytes malformed: refuse to touch
	}
	next, apply, err := step(cur)
	if err != nil || !apply {
		return next, false, err
	}

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
					return 0, false, err
				}
				if db.opts.SyncEveryWrite {
					if err := db.patcher.Sync(); err != nil {
						return 0, false, err
					}
				}
				return next, true, nil
			}
		}
		// Any mismatch (marshal drift, length, pattern): fall through to the
		// always-correct append path.
	}
	return next, true, db.incrAppendLocked(key, next)
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
