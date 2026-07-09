// Command capi builds nteedb as a C-shared library (-buildmode=c-shared) so it
// can be loaded in-process from other languages (e.g. Node.js via koffi).
//
// ABI: every exported function returns a C string (void*) holding a JSON
// "envelope" — {"err": "..."} on failure, or {"result": <value>} on success
// (result omitted when there's nothing to return). The caller reads the string
// and MUST free it with nteedb_free. A *nteedb.DB is referenced by an opaque
// uint32 handle. Binary value INPUT (put) is passed as (uint8* ptr, int len);
// value OUTPUT (get) mirrors the on-disk record split — valid-UTF-8 values as
// a plain JSON string ("s"), binary as base64 ("v") — so text payloads cross
// the boundary without a base64 round-trip.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"unsafe"

	nteedb "codeberg.org/nickoan/ntee-r1quest/ntee-db"
)

func main() {}

var errInvalidHandle = errors.New("nteedb: invalid handle")

// reply marshals a JSON envelope and returns it as a C string (caller frees).
func reply(result any, err error) *C.char {
	env := map[string]any{}
	if err != nil {
		env["err"] = err.Error()
	} else if result != nil {
		env["result"] = result
	}
	b, mErr := json.Marshal(env)
	if mErr != nil {
		b = []byte(`{"err":"nteedb: failed to encode result"}`)
	}
	return C.CString(string(b))
}

//export nteedb_open
func nteedb_open(dir *C.char, optsJSON *C.char) *C.char {
	opts, err := parseOptions(C.GoString(dir), C.GoString(optsJSON))
	if err != nil {
		return reply(nil, err)
	}
	db, err := nteedb.Open(opts)
	if err != nil {
		return reply(nil, err)
	}
	return reply(regPut(db), nil)
}

// nteedb_close and nteedb_drop are deliberately idempotent: an unknown or
// already-released handle succeeds silently (unlike data ops, which return
// errInvalidHandle) so teardown paths can never fail on a double close.
//
//export nteedb_close
func nteedb_close(h C.uint) *C.char {
	if db := regDelete(uint32(h)); db != nil {
		return reply(nil, db.Close())
	}
	return reply(nil, nil)
}

//export nteedb_drop
func nteedb_drop(h C.uint) *C.char {
	if db := regDelete(uint32(h)); db != nil {
		return reply(nil, db.Drop())
	}
	return reply(nil, nil)
}

//export nteedb_destroy
func nteedb_destroy(dir *C.char) *C.char {
	return reply(nil, nteedb.Destroy(C.GoString(dir)))
}

//export nteedb_put
func nteedb_put(h C.uint, key *C.char, val *C.uchar, valLen C.int, ixJSON *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	value := C.GoBytes(unsafe.Pointer(val), valLen)
	ixStr := C.GoString(ixJSON)
	if ixStr == "" {
		return reply(nil, db.Put(C.GoString(key), value))
	}
	var ix map[string]any
	if err := json.Unmarshal([]byte(ixStr), &ix); err != nil {
		return reply(nil, err)
	}
	return reply(nil, db.PutIndexed(C.GoString(key), value, ix))
}

// valJSON is one value on the inline-JSON read path. A value that is valid JSON
// is spliced verbatim into JSON (no escaping) so the JS envelope parse yields
// the object directly; binary / non-JSON falls back to base64.
type valJSON struct {
	Found bool            `json:"found"`
	JSON  json.RawMessage `json:"json,omitempty"`
	V     string          `json:"v,omitempty"`
}

func encodeValJSON(v []byte, found bool) valJSON {
	rec := valJSON{Found: found}
	if found {
		if json.Valid(v) {
			rec.JSON = json.RawMessage(v)
		} else {
			rec.V = base64.StdEncoding.EncodeToString(v)
		}
	}
	return rec
}

// recordJSON is one {key, value} row on the records-by-query read path: the key
// plus the same inline-JSON value encoding as valJSON. Lets a records search
// return keys + values in a single FFI crossing (index walk + batched read),
// instead of a separate keys query followed by getMany.
type recordJSON struct {
	Key   string          `json:"key"`
	Found bool            `json:"found"`
	JSON  json.RawMessage `json:"json,omitempty"`
	V     string          `json:"v,omitempty"`
}

// encodeRecords zips keys with their read values into records, aligned to keys.
func encodeRecords(keys []string, values [][]byte, found []bool) []recordJSON {
	out := make([]recordJSON, len(keys))
	for i, k := range keys {
		vj := encodeValJSON(values[i], found[i])
		out[i] = recordJSON{Key: k, Found: vj.Found, JSON: vj.JSON, V: vj.V}
	}
	return out
}

// readRecordsReply resolves keys to values and marshals the record envelope.
func readRecordsReply(db *nteedb.DB, keys []string, err error) *C.char {
	if err != nil {
		return reply(nil, err)
	}
	values, found, err := db.GetMany(keys)
	if err != nil {
		return reply(nil, err)
	}
	return reply(encodeRecords(keys, values, found), nil)
}

//export nteedb_get_json
func nteedb_get_json(h C.uint, key *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	v, ok, err := db.Get(C.GoString(key))
	if err != nil {
		return reply(nil, err)
	}
	return reply(encodeValJSON(v, ok), nil)
}

//export nteedb_get_many_json
func nteedb_get_many_json(h C.uint, keysJSON *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	var keys []string
	if err := json.Unmarshal([]byte(C.GoString(keysJSON)), &keys); err != nil {
		return reply(nil, err)
	}
	values, found, err := db.GetMany(keys)
	if err != nil {
		return reply(nil, err)
	}
	out := make([]valJSON, len(keys))
	for i := range keys {
		out[i] = encodeValJSON(values[i], found[i])
	}
	return reply(out, nil)
}

//export nteedb_has
func nteedb_has(h C.uint, key *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	return reply(db.Has(C.GoString(key)), nil)
}

//export nteedb_stats
func nteedb_stats(h C.uint) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	return reply(db.Stats(), nil)
}

//export nteedb_delete
func nteedb_delete(h C.uint, key *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	return reply(nil, db.Delete(C.GoString(key)))
}

// jsonBatchItem is one PutBatch record on the FFI boundary. The value carries
// the same split as the get envelope and the on-disk record: valid-UTF-8 as a
// plain string ("s"), binary as base64 ("v").
type jsonBatchItem struct {
	K  string         `json:"k"`
	S  string         `json:"s,omitempty"` // inline value, valid UTF-8: plain string
	V  string         `json:"v,omitempty"` // inline value, binary: base64
	IX map[string]any `json:"ix,omitempty"`
}

//export nteedb_put_batch
func nteedb_put_batch(h C.uint, itemsJSON *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	var jitems []jsonBatchItem
	if err := json.Unmarshal([]byte(C.GoString(itemsJSON)), &jitems); err != nil {
		return reply(nil, err)
	}
	items := make([]nteedb.PutItem, len(jitems))
	for i, ji := range jitems {
		var v []byte
		if ji.S != "" {
			v = []byte(ji.S)
		} else if ji.V != "" {
			var err error
			if v, err = base64.StdEncoding.DecodeString(ji.V); err != nil {
				return reply(nil, err)
			}
		}
		items[i] = nteedb.PutItem{Key: ji.K, Value: v, IX: ji.IX}
	}
	if err := db.PutBatch(items); err != nil {
		return reply(nil, err)
	}
	return reply(len(items), nil)
}

// binBatchItem is one PutBatch record on the binary-batch boundary: only the
// key, the value's byte LENGTH, and optional index values travel as JSON — the
// value bytes ride a single concatenated blob, so text is never escaped and
// binary is never base64'd.
type binBatchItem struct {
	K  string         `json:"k"`
	N  int            `json:"n"` // value length in bytes within the blob
	IX map[string]any `json:"ix,omitempty"`
}

//export nteedb_put_batch_bin
func nteedb_put_batch_bin(h C.uint, metaJSON *C.char, blob *C.uchar, blobLen C.int) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	var metas []binBatchItem
	if err := json.Unmarshal([]byte(C.GoString(metaJSON)), &metas); err != nil {
		return reply(nil, err)
	}
	// One copy of the whole value blob; each item's value is a subslice of it.
	buf := C.GoBytes(unsafe.Pointer(blob), blobLen)
	items := make([]nteedb.PutItem, len(metas))
	off := 0
	for i, m := range metas {
		if m.N < 0 || off+m.N > len(buf) {
			return reply(nil, errors.New("nteedb: batch blob length mismatch"))
		}
		items[i] = nteedb.PutItem{Key: m.K, Value: buf[off : off+m.N], IX: m.IX}
		off += m.N
	}
	if off != len(buf) {
		return reply(nil, errors.New("nteedb: batch blob length mismatch"))
	}
	if err := db.PutBatch(items); err != nil {
		return reply(nil, err)
	}
	return reply(len(items), nil)
}

//export nteedb_prefix_scan
func nteedb_prefix_scan(h C.uint, prefix *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	keys, err := db.PrefixScan(C.GoString(prefix))
	if err != nil {
		return reply(nil, err)
	}
	return reply(emptyIfNil(keys), nil)
}

//export nteedb_by_index
func nteedb_by_index(h C.uint, name *C.char, valJSON *C.char, limit C.int) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	var val any
	if err := json.Unmarshal([]byte(C.GoString(valJSON)), &val); err != nil {
		return reply(nil, err)
	}
	keys, err := db.ByIndex(C.GoString(name), val, int(limit))
	if err != nil {
		return reply(nil, err)
	}
	return reply(emptyIfNil(keys), nil)
}

//export nteedb_by_index_records_json
func nteedb_by_index_records_json(h C.uint, name *C.char, valJSONStr *C.char, limit C.int) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	var val any
	if err := json.Unmarshal([]byte(C.GoString(valJSONStr)), &val); err != nil {
		return reply(nil, err)
	}
	keys, err := db.ByIndex(C.GoString(name), val, int(limit))
	return readRecordsReply(db, keys, err)
}

//export nteedb_by_index_prefix_records_json
func nteedb_by_index_prefix_records_json(h C.uint, name *C.char, prefix *C.char, limit C.int) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	keys, err := db.ByIndexPrefix(C.GoString(name), C.GoString(prefix), int(limit))
	return readRecordsReply(db, keys, err)
}

//export nteedb_prefix_scan_records_json
func nteedb_prefix_scan_records_json(h C.uint, prefix *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	keys, err := db.PrefixScan(C.GoString(prefix))
	return readRecordsReply(db, keys, err)
}

//export nteedb_by_index_has
func nteedb_by_index_has(h C.uint, name *C.char, valJSON *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	var val any
	if err := json.Unmarshal([]byte(C.GoString(valJSON)), &val); err != nil {
		return reply(nil, err)
	}
	ok, err := db.ByIndexHas(C.GoString(name), val)
	if err != nil {
		return reply(nil, err)
	}
	return reply(ok, nil)
}

//export nteedb_by_index_prefix
func nteedb_by_index_prefix(h C.uint, name *C.char, prefix *C.char, limit C.int) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	keys, err := db.ByIndexPrefix(C.GoString(name), C.GoString(prefix), int(limit))
	if err != nil {
		return reply(nil, err)
	}
	return reply(emptyIfNil(keys), nil)
}

//export nteedb_by_index_range
func nteedb_by_index_range(h C.uint, name *C.char, loJSON *C.char, hiJSON *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	var lo, hi any
	if err := json.Unmarshal([]byte(C.GoString(loJSON)), &lo); err != nil {
		return reply(nil, err)
	}
	if err := json.Unmarshal([]byte(C.GoString(hiJSON)), &hi); err != nil {
		return reply(nil, err)
	}
	keys, err := db.ByIndexRange(C.GoString(name), lo, hi)
	if err != nil {
		return reply(nil, err)
	}
	return reply(emptyIfNil(keys), nil)
}

//export nteedb_remove_by_pk_less
func nteedb_remove_by_pk_less(h C.uint, cutoff *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	n, err := db.RemoveByPkLess(C.GoString(cutoff))
	if err != nil {
		return reply(nil, err)
	}
	return reply(n, nil)
}

//export nteedb_remove_by_pk_greater
func nteedb_remove_by_pk_greater(h C.uint, cutoff *C.char) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	n, err := db.RemoveByPkGreater(C.GoString(cutoff))
	if err != nil {
		return reply(nil, err)
	}
	return reply(n, nil)
}

//export nteedb_compact
func nteedb_compact(h C.uint) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	return reply(nil, db.Compact())
}

//export nteedb_reindex
func nteedb_reindex(h C.uint) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	return reply(nil, db.Reindex())
}

//export nteedb_dropped_indexes
func nteedb_dropped_indexes(h C.uint) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	return reply(emptyIfNil(db.DroppedIndexes()), nil)
}

//export nteedb_prospective_indexes
func nteedb_prospective_indexes(h C.uint) *C.char {
	db := regGet(uint32(h))
	if db == nil {
		return reply(nil, errInvalidHandle)
	}
	return reply(emptyIfNil(db.ProspectiveIndexes()), nil)
}

//export nteedb_free
func nteedb_free(p unsafe.Pointer) {
	C.free(p)
}

// emptyIfNil ensures a nil slice marshals to [] (not null) for the JS side.
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
