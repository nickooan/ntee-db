package nteedb

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/btree"
)

// ValueKind is the type of values held by a secondary index.
type ValueKind int

const (
	KindString ValueKind = iota
	KindNumber
)

func (k ValueKind) String() string {
	if k == KindNumber {
		return "number"
	}
	return "string"
}

// IndexValues maps index names to their values for a single record, e.g.
// {"traceId": "abc", "status": 200}.
type IndexValues = map[string]any

// IndexDef declares a secondary index. Extract is optional: when set, the index
// value for a record is derived automatically from its key and value (the "scan
// itself" mode); when nil, the value must be supplied via PutIndexed. The
// derived/supplied value is persisted in the record so the index can be rebuilt
// at boot without re-reading values.
//
// Indexes cover JSON-object values only: Extract is never executed over an
// immediate value (string, number, boolean, array, binary), and PutIndexed
// rejects explicit values for one. Immediate values are primary-key-only.
//
// MaxPerValue, when > 0, caps how many records may share one value in this
// index. A write that pushes a value's group over the cap evicts the oldest
// record(s) — lowest primary key within the group — as a full, durable delete
// (tombstone; the record leaves the primary index and every secondary index).
// "Oldest" therefore relies on the caller designing time-ordered keys (see the
// README's key-design section). A write whose NEW key would itself be the
// eviction victim (it sorts at/below a full group's eviction boundary) is
// rejected with an error rather than silently vanishing. 0 or negative =
// unlimited. Enforced on the write path only: not during boot replay or
// Reindex back-fill, but a group left over the cap (e.g. after lowering it) is
// drained by the next write to that value.
type IndexDef struct {
	Name        string
	Kind        ValueKind
	Extract     func(key string, value []byte) (any, bool)
	MaxPerValue int
}

// secEntry is one (value, primary-key) pair in a secondary index. For a string
// index only s is used; for a number index only f is used.
type secEntry struct {
	s  string
	f  float64
	pk string
}

// secIndex is one named secondary index: a counted B-tree of entries ordered
// by (value, primary key). Insert/remove are O(log n) wherever the value sorts
// (the sorted-slice predecessor memmoved the tail — O(N²) to build a hot
// low-cardinality index; BenchmarkPutIndexedHotValue guards the regression).
// The query walks keep their positional shape via rank/GetAt over the counted
// tree: a position lookup is O(log n), a bound search O(log² n).
type secIndex struct {
	name string
	kind ValueKind
	max  int // cap on records per distinct value; <= 0 = unlimited
	tree *btree.BTreeG[secEntry]
}

func newSecIndex(def IndexDef) *secIndex {
	si := &secIndex{name: def.Name, kind: def.Kind, max: def.MaxPerValue}
	si.tree = btree.NewBTreeG(si.less) // less reads si.kind, set above
	return si
}

// count returns the number of entries.
func (si *secIndex) count() int { return si.tree.Len() }

// at returns the i-th entry in (value, pk) order; i must be in [0, count).
func (si *secIndex) at(i int) secEntry {
	e, _ := si.tree.GetAt(i)
	return e
}

// makeEntry converts a value (string, or any numeric/json.Number) into a typed
// entry for this index, validating it against the index's kind.
func (si *secIndex) makeEntry(val any, pk string) (secEntry, error) {
	e := secEntry{pk: pk}
	switch si.kind {
	case KindNumber:
		f, ok := toFloat(val)
		if !ok {
			return e, fmt.Errorf("nteedb: index %q expects a number, got %T", si.name, val)
		}
		e.f = f
	default: // KindString
		s, ok := val.(string)
		if !ok {
			return e, fmt.Errorf("nteedb: index %q expects a string, got %T", si.name, val)
		}
		e.s = s
	}
	return e, nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// less orders entries by value, then by primary key.
func (si *secIndex) less(a, b secEntry) bool {
	if si.kind == KindNumber {
		if a.f != b.f {
			return a.f < b.f
		}
	} else if a.s != b.s {
		return a.s < b.s
	}
	return a.pk < b.pk
}

func (si *secIndex) valueEqual(a, b secEntry) bool {
	if si.kind == KindNumber {
		return a.f == b.f
	}
	return a.s == b.s
}

// valueGreater reports whether a's value is strictly greater than b's (ignoring
// the primary key).
func (si *secIndex) valueGreater(a, b secEntry) bool {
	if si.kind == KindNumber {
		return a.f > b.f
	}
	return a.s > b.s
}

// lowerBound returns the rank of the first entry >= e (or count if none) — a
// binary search over positions, O(log² n) on the counted tree.
func (si *secIndex) lowerBound(e secEntry) int {
	return sort.Search(si.count(), func(i int) bool {
		return !si.less(si.at(i), e)
	})
}

// upperBoundValue returns the rank of the first entry whose value is strictly
// greater than the probe's value — i.e. one past the last entry with that value.
func (si *secIndex) upperBoundValue(e secEntry) int {
	return sort.Search(si.count(), func(i int) bool {
		return si.valueGreater(si.at(i), e)
	})
}

func (si *secIndex) insert(e secEntry) {
	si.tree.Set(e) // an exact duplicate (same value + pk) replaces itself
}

func (si *secIndex) remove(e secEntry) {
	si.tree.Delete(e)
}

// removePKs drops every entry whose primary key is in pks — the bulk
// counterpart to remove, used by a range delete: one ordered sweep to collect
// (O(n)), then O(m log n) deletes.
func (si *secIndex) removePKs(pks map[string]struct{}) {
	var doomed []secEntry
	si.tree.Scan(func(e secEntry) bool {
		if _, gone := pks[e.pk]; gone {
			doomed = append(doomed, e)
		}
		return true
	})
	for _, e := range doomed {
		si.tree.Delete(e)
	}
}

// exact returns the primary keys whose index value equals val (multi-value).
// limit controls how many and in which direction:
//   - limit == 0: all matches, ascending by primary key.
//   - limit > 0:  the first `limit` matches, ascending.
//   - limit < 0:  the last `|limit|` matches, descending (newest-first when the
//     primary key encodes order).
func (si *secIndex) exact(val any, limit int) ([]string, error) {
	probe, err := si.makeEntry(val, "")
	if err != nil {
		return nil, err
	}
	it := si.tree.Iter()
	defer it.Release()
	if !it.Seek(probe) { // first entry >= (val, "") — the group's first, if any
		return nil, nil
	}

	if limit >= 0 { // ascending; stop at limit (0 = all)
		var out []string
		for {
			e := it.Item()
			if !si.valueEqual(e, probe) {
				break
			}
			out = append(out, e.pk)
			if limit > 0 && len(out) == limit {
				break
			}
			if !it.Next() {
				break
			}
		}
		return out, nil
	}

	// limit < 0: the last |limit| of the group, descending. Collect the group
	// forward, then emit its tail reversed.
	var grp []string
	for {
		e := it.Item()
		if !si.valueEqual(e, probe) {
			break
		}
		grp = append(grp, e.pk)
		if !it.Next() {
			break
		}
	}
	n := -limit
	if n > len(grp) {
		n = len(grp)
	}
	out := make([]string, 0, n)
	for i := len(grp) - 1; i >= len(grp)-n; i-- {
		out = append(out, grp[i])
	}
	return out, nil
}

// exists reports whether any entry has index value == val, without collecting
// the matching primary keys — the cheap (O(log n)) counterpart of exact.
func (si *secIndex) exists(val any) (bool, error) {
	probe, err := si.makeEntry(val, "")
	if err != nil {
		return false, err
	}
	it := si.tree.Iter()
	defer it.Release()
	return it.Seek(probe) && si.valueEqual(it.Item(), probe), nil
}

// prefix returns the primary keys whose (string) index value starts with p.
//
// limit is applied per distinct index value (grouped), not to the flattened
// match list — a prefix can span several values, and the limit selects within
// each one:
//   - limit == 0: all matches, flat, in (value, primary key) order.
//   - limit > 0:  the first `limit` primary keys of each value, ascending.
//   - limit < 0:  the last `|limit|` of each value, descending (newest-first
//     when the primary key encodes order), matching exact's direction.
//
// A forward iterator from the first entry >= p walks the matching run one group
// at a time. When limit != 0 it emits each value's first/last N and then Seeks
// past the group's interior to the next distinct value (v+"\x00" is v's
// immediate successor in value space), so a grouped query stays sublinear in
// group size — O(groups · (|limit| + log n)) — instead of touching every entry.
func (si *secIndex) prefix(p string, limit int) ([]string, error) {
	if si.kind != KindString {
		return nil, fmt.Errorf("nteedb: prefix query requires a string index, %q is %s", si.name, si.kind)
	}
	it := si.tree.Iter()
	defer it.Release()

	var out []string
	for ok := it.Seek(secEntry{s: p}); ok; {
		v := it.Item().s
		if !strings.HasPrefix(v, p) {
			break
		}
		next := secEntry{s: v + "\x00"} // first entry whose value is > v
		switch {
		case limit == 0: // all of the group, flat, ascending
			for ok && it.Item().s == v {
				out = append(out, it.Item().pk)
				ok = it.Next()
			}
		case limit > 0: // first `limit` of the group, ascending
			for cnt := 0; ok && it.Item().s == v && cnt < limit; cnt++ {
				out = append(out, it.Item().pk)
				ok = it.Next()
			}
			if ok && it.Item().s == v { // skip the group's tail
				ok = it.Seek(next)
			}
		default: // limit < 0: last |limit| of the group, descending
			// Jump past the group, then step back into it.
			var back bool
			if it.Seek(next) {
				back = it.Prev()
			} else {
				back = it.Last() // no next value: the group runs to the end
			}
			for cnt := 0; back && it.Item().s == v && cnt < -limit; cnt++ {
				out = append(out, it.Item().pk)
				back = it.Prev()
			}
			ok = it.Seek(next) // reposition to the next group for the outer loop
		}
	}
	return out, nil
}

// rangeQuery returns the primary keys whose index value is within [lo, hi].
func (si *secIndex) rangeQuery(lo, hi any) ([]string, error) {
	loE, err := si.makeEntry(lo, "")
	if err != nil {
		return nil, err
	}
	hiE, err := si.makeEntry(hi, "")
	if err != nil {
		return nil, err
	}
	it := si.tree.Iter()
	defer it.Release()
	var out []string
	for ok := it.Seek(loE); ok; ok = it.Next() {
		e := it.Item()
		if si.kind == KindNumber { // stop once the value exceeds hi
			if e.f > hiE.f {
				break
			}
		} else if e.s > hiE.s {
			break
		}
		out = append(out, e.pk)
	}
	return out, nil
}

// --- DB integration: write path, retraction, and query API ---

// writeLocked appends a record for key and updates the primary and secondary
// indexes. It handles blob offloading for large values. Callers must hold db.mu.
func (db *DB) writeLocked(key string, value []byte, explicit IndexValues) error {
	ix, err := db.buildIndexValues(key, value, explicit)
	if err != nil {
		return err
	}
	if err := db.checkSelfEvictionLocked(key, ix); err != nil {
		return err
	}
	if err := db.appendRecordLocked(key, value, ix, db.opts.SyncEveryWrite, false); err != nil {
		return err
	}
	if err := db.enforceMaxPerValueLocked(ix); err != nil {
		return err
	}
	db.writes++
	db.maybeWriteHintLocked()
	return nil
}

// appendRecordLocked is the per-record write core shared by Put, PutBatch and
// Incr: blob offload, main-log append, and primary/secondary index updates.
// durable controls the per-write fsyncs — batch writers pass false and issue a
// single flush at the end of the batch. counter marks the record as an int64
// counter (see Incr); counter records are never blob-offloaded, whatever the
// configured threshold — in-place increments need the value inline in the main
// log. Callers must hold db.mu and have validated ix via buildIndexValues.
func (db *DB) appendRecordLocked(key string, value []byte, ix map[string]any, durable, counter bool) error {
	// Large values go to the blob side file; the main record just references
	// them. The blob is fsynced before the referencing main record is appended —
	// on EVERY path, not just durable mode: the two live in different files, so
	// without this barrier a power loss could persist the main record while the
	// blob bytes are still in the page cache, leaving a reference past the end
	// of blobs.dat. With it, a crash can only ever orphan a blob. Blobs are rare
	// (values >= BlobThreshold), so the extra fsync on the fast path is cheap.
	rec := record{Key: key, Value: value, IX: ix, Counter: counter}
	if !counter && db.useBlobFor(len(value)) {
		bs := db.curBlobs()
		ref, err := bs.append(value)
		if err != nil {
			return err
		}
		if err := bs.flush(); err != nil {
			return err
		}
		ref.Gen = db.curGen
		rec = record{Key: key, Blob: &ref, IX: ix}
	}

	off, n, err := db.main.appendSync(rec, durable)
	if err != nil {
		return err
	}
	db.pk.upsert(pkEntry{key: key, off: off, n: n})
	db.refreshSecLocked(key, ix)
	return nil
}

// enforceMaxPerValueLocked applies each capped index's MaxPerValue to the value
// groups this write just touched: while a group holds more than max records,
// the lowest-pk (oldest, when keys encode time) records are evicted as full,
// durable deletes — tombstone first, then primary-index removal and retraction
// from every secondary index, exactly like Delete. Callers must hold db.mu.
//
// An overwrite of an existing key never grows a group (its old entry is
// retracted before the new one is inserted), so it cannot trigger eviction.
func (db *DB) enforceMaxPerValueLocked(ix map[string]any) error {
	for name, val := range ix {
		si := db.secIndexes[name]
		if si == nil || si.max <= 0 {
			continue
		}
		probe, err := si.makeEntry(val, "")
		if err != nil {
			continue // value already validated on write; be lenient here
		}
		lo := si.lowerBound(probe)
		hi := si.upperBoundValue(probe)
		excess := (hi - lo) - si.max
		if excess <= 0 {
			continue
		}
		// Snapshot the victim pks (the group's lowest) before retraction below
		// mutates the tree we are reading.
		victims := make([]string, 0, excess)
		for i := lo; i < lo+excess; i++ {
			victims = append(victims, si.at(i).pk)
		}
		for _, pk := range victims {
			if _, _, err := db.main.append(record{Key: pk, Deleted: true}); err != nil {
				return err
			}
			// Retract BEFORE removing the primary entry — the retraction reads
			// the victim's ix values off that entry.
			db.retractSecLocked(pk)
			db.pk.remove(pk)
			db.writes++
		}
	}
	return nil
}

// checkSelfEvictionLocked rejects a write whose key would be immediately
// evicted by a capped index: eviction keeps a group's highest primary keys, so
// a NEW key that sorts at (or below) the group's eviction boundary would be
// tombstoned by its own write — Put would report success while Get finds
// nothing. Rejecting up front (before anything is appended) turns that silent
// loss into an explicit error. Overwrites of a key already in the group never
// grow it, so they are always allowed. Callers must hold db.mu.
func (db *DB) checkSelfEvictionLocked(key string, ix map[string]any) error {
	for name, val := range ix {
		si := db.secIndexes[name]
		if si == nil || si.max <= 0 {
			continue
		}
		evict, err := si.wouldSelfEvict(val, key)
		if err != nil {
			continue // value already validated by buildIndexValues; be lenient
		}
		if evict {
			return fmt.Errorf(
				"nteedb: key %q would be immediately evicted by index %q (maxPerValue %d): keys must be ordered so new keys sort after existing ones",
				key, name, si.max)
		}
	}
	return nil
}

// wouldSelfEvict reports whether inserting (val, key) into this capped index
// would make the new entry itself an eviction victim: the group is at (or over)
// its cap, key is not already a member, and key sorts within the group's lowest
// `excess` entries after insertion.
func (si *secIndex) wouldSelfEvict(val any, key string) (bool, error) {
	if si.max <= 0 {
		return false, nil
	}
	probe, err := si.makeEntry(val, "")
	if err != nil {
		return false, err
	}
	lo := si.lowerBound(probe)
	hi := si.upperBoundValue(probe)
	excess := (hi - lo) + 1 - si.max // group size after inserting the new entry
	if excess <= 0 {
		return false, nil
	}
	// An overwrite of an existing member does not grow the group.
	self, err := si.makeEntry(val, key)
	if err != nil {
		return false, err
	}
	if i := si.lowerBound(self); i < hi && si.at(i).pk == key {
		return false, nil
	}
	// The new entry is among the victims iff it sorts at or below the
	// excess-th smallest existing member of the group.
	return key <= si.at(lo+excess-1).pk, nil
}

// isObjectValue reports whether value is shaped like a JSON object — leading
// JSON whitespace then '{'. A cheap shape sniff, not full validation: only
// object records may carry secondary index entries (see buildIndexValues).
func isObjectValue(value []byte) bool {
	for _, c := range value {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c == '{'
	}
	return false
}

// buildIndexValues merges explicit values with values derived from index
// Extract functions, validating each against its index's kind. It returns nil
// when there are no secondary index values for this record.
//
// Only JSON-object values may carry index entries: immediate values (strings,
// numbers, booleans, arrays, binary) are plain key:value pairs addressed by
// primary key alone — explicit values for them are an error, and Extract
// functions are never executed over them.
func (db *DB) buildIndexValues(key string, value []byte, explicit IndexValues) (map[string]any, error) {
	if len(db.secIndexes) == 0 && len(explicit) == 0 {
		return nil, nil
	}
	if !isObjectValue(value) {
		if len(explicit) > 0 {
			return nil, fmt.Errorf("nteedb: secondary index values require a JSON object value; key %q holds an immediate value (primary-key access only)", key)
		}
		return nil, nil // immediate value: never run Extract
	}
	out := make(map[string]any)
	for name, val := range explicit {
		si := db.secIndexes[name]
		if si == nil {
			return nil, fmt.Errorf("nteedb: unknown index %q", name)
		}
		if _, err := si.makeEntry(val, key); err != nil {
			return nil, err
		}
		out[name] = val
	}
	for _, def := range db.indexDefs {
		if def.Extract == nil {
			continue
		}
		if _, done := out[def.Name]; done {
			continue // explicit value wins
		}
		if v, ok := def.Extract(key, value); ok {
			if _, err := db.secIndexes[def.Name].makeEntry(v, key); err != nil {
				return nil, err
			}
			out[def.Name] = v
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// insertSecLocked applies a key's index values to the secondary indexes. It is
// best-effort (skips unknown indexes / invalid values) so it is safe during
// boot replay where the declared index set may differ. Callers must hold db.mu.
func (db *DB) insertSecLocked(key string, ix map[string]any) {
	for name, val := range ix {
		si := db.secIndexes[name]
		if si == nil {
			continue
		}
		if e, err := si.makeEntry(val, key); err == nil {
			si.insert(e)
		}
	}
}

// refreshSecLocked retracts a key's previous secondary entries, applies its new
// ones, and records ix on the key's primary entry (the single home of "this
// key's current index values"; there is no separate map). The stored ix map is
// owned from here on and never mutated in place — every writer allocates a
// fresh map — which is what lets the background hint writer iterate a COW
// clone of the primary tree without copying the maps. Callers must hold db.mu.
func (db *DB) refreshSecLocked(key string, ix map[string]any) {
	db.retractSecLocked(key)
	if len(ix) == 0 {
		return
	}
	db.insertSecLocked(key, ix)
	db.pk.setIX(key, ix)
}

// retractSecLocked removes a key's current secondary entries (used on overwrite
// and delete). The values are read from the key's primary entry, so this MUST
// run before pk.remove when a key is being deleted. Callers must hold db.mu.
func (db *DB) retractSecLocked(key string) {
	e, ok := db.pk.get(key)
	if !ok || len(e.ix) == 0 {
		return
	}
	for name, val := range e.ix {
		si := db.secIndexes[name]
		if si == nil {
			continue
		}
		if se, err := si.makeEntry(val, key); err == nil {
			si.remove(se)
		}
	}
	db.pk.setIX(key, nil)
}

// rebuildSecLocked rebuilds all secondary indexes from the ix values carried on
// the primary entries (used after a rewrite/compaction/reindex, when db.pk has
// been replaced wholesale). Callers must hold db.mu.
func (db *DB) rebuildSecLocked() {
	for _, si := range db.secIndexes {
		si.tree.Clear()
	}
	db.pk.scan(func(e pkEntry) bool {
		db.insertSecLocked(e.key, e.ix)
		return true
	})
}

// ByIndex returns the primary keys whose value in the named index equals val
// (multi-value). An optional limit controls count and direction: 0 (or omitted)
// = all ascending; N>0 = first N ascending; N<0 = last |N| descending.
func (db *DB) ByIndex(name string, val any, limit ...int) ([]string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	si := db.secIndexes[name]
	if si == nil {
		return nil, fmt.Errorf("nteedb: unknown index %q", name)
	}
	n := 0
	if len(limit) > 0 {
		n = limit[0]
	}
	return si.exact(val, n)
}

// ByIndexHas reports whether any record has value val in the named index,
// without materializing the matching keys — the secondary-index counterpart of
// Has. An unknown index name (or a value of the wrong kind) is an error.
func (db *DB) ByIndexHas(name string, val any) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return false, ErrClosed
	}
	si := db.secIndexes[name]
	if si == nil {
		return false, fmt.Errorf("nteedb: unknown index %q", name)
	}
	return si.exists(val)
}

// ByIndexPrefix returns the primary keys whose value in the named (string) index
// starts with prefix. An optional limit is applied per distinct index value
// (grouped): 0 (or omitted) = all matches flat; N>0 = first N of each value
// ascending; N<0 = last |N| of each value descending.
func (db *DB) ByIndexPrefix(name, prefix string, limit ...int) ([]string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	si := db.secIndexes[name]
	if si == nil {
		return nil, fmt.Errorf("nteedb: unknown index %q", name)
	}
	n := 0
	if len(limit) > 0 {
		n = limit[0]
	}
	return si.prefix(prefix, n)
}

// ProspectiveIndexes returns, sorted, the names of declared indexes that have
// not been back-filled over records that existed before the index was added (or
// its kind changed). These indexes cover only records written since; call
// Reindex to populate them over historical data (Extract-based indexes only).
func (db *DB) ProspectiveIndexes() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]string, 0, len(db.prospective))
	for name := range db.prospective {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DroppedIndexes returns, sorted, the names of indexes that were dropped from
// the declared set but whose values still linger in records (a soft-drop). They
// remain until a Reindex purges them from records and meta.
func (db *DB) DroppedIndexes() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]string, 0, len(db.dropped))
	for name := range db.dropped {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ByIndexRange returns the primary keys whose value in the named index is within
// the inclusive range [lo, hi].
func (db *DB) ByIndexRange(name string, lo, hi any) ([]string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	si := db.secIndexes[name]
	if si == nil {
		return nil, fmt.Errorf("nteedb: unknown index %q", name)
	}
	return si.rangeQuery(lo, hi)
}
