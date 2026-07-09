package nteedb

import (
	"strings"

	"github.com/tidwall/btree"
)

// pkEntry locates a key's latest record within the main log and carries the
// key's current secondary-index values (for retraction on overwrite/delete and
// for hint snapshots — there is no separate key→ix map).
type pkEntry struct {
	key string
	off int64
	n   int32
	ix  map[string]any // current secondary-index values; nil when none
}

// pkIndex is the in-memory primary-key index: a counted copy-on-write B-tree
// ordered by key. Insert/delete/lookup are O(log n) regardless of key order,
// ordered iteration serves prefix scans, and copy() is an O(1) COW clone the
// background hint writer iterates without holding db.mu (shared nodes are
// copied on write by whichever side mutates first). Each entry carries its
// current secondary-index values, so there is no separate key→ix map to copy
// on every hint snapshot — see hintSnapshot in db.go. BenchmarkPutIndexedHotValue
// and BenchmarkPutHintEveryN guard the two costs this structure eliminated.
type pkIndex struct {
	tree *btree.BTreeG[pkEntry]
}

func pkLess(a, b pkEntry) bool { return a.key < b.key }

func newPkIndex() *pkIndex {
	return &pkIndex{tree: btree.NewBTreeG(pkLess)}
}

func (ix *pkIndex) len() int { return ix.tree.Len() }

// get returns the entry for an exact key.
func (ix *pkIndex) get(key string) (pkEntry, bool) {
	return ix.tree.Get(pkEntry{key: key})
}

// upsert inserts e, or updates an existing key's record location in place,
// preserving the entry's current ix (which only refreshSecLocked rewrites).
func (ix *pkIndex) upsert(e pkEntry) {
	if prev, ok := ix.tree.Get(e); ok {
		e.ix = prev.ix
	}
	ix.tree.Set(e)
}

// setIX replaces the stored secondary-index values for key (no-op if absent).
func (ix *pkIndex) setIX(key string, ixv map[string]any) {
	if e, ok := ix.tree.Get(pkEntry{key: key}); ok {
		e.ix = ixv
		ix.tree.Set(e)
	}
}

// remove deletes a key, reporting whether it was present.
func (ix *pkIndex) remove(key string) bool {
	_, ok := ix.tree.Delete(pkEntry{key: key})
	return ok
}

// load bulk-appends an entry during boot. Entries must arrive in ascending key
// order (the hint file is sorted), which takes the btree's fast Load path.
func (ix *pkIndex) load(e pkEntry) {
	ix.tree.Load(e)
}

// scan visits every entry in ascending key order while fn returns true.
func (ix *pkIndex) scan(fn func(e pkEntry) bool) {
	ix.tree.Scan(fn)
}

// ascendFrom visits entries with key >= from in ascending order while fn
// returns true.
func (ix *pkIndex) ascendFrom(from string, fn func(e pkEntry) bool) {
	ix.tree.Ascend(pkEntry{key: from}, fn)
}

// prefix returns all entries whose key starts with p, in sorted order. An empty
// prefix matches every entry.
func (ix *pkIndex) prefix(p string) []pkEntry {
	var out []pkEntry
	ix.tree.Ascend(pkEntry{key: p}, func(e pkEntry) bool {
		if !strings.HasPrefix(e.key, p) {
			return false
		}
		out = append(out, e)
		return true
	})
	return out
}

// copy returns an O(1) copy-on-write clone that is safe to iterate from another
// goroutine while the original keeps mutating.
func (ix *pkIndex) copy() *pkIndex {
	return &pkIndex{tree: ix.tree.Copy()}
}
