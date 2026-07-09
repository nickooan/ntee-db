package nteedb

import (
	"encoding/json"
	"unicode/utf8"
)

// blobRef points at a value stored in the blob side file (blobs.dat).
type blobRef struct {
	Off  int64 `json:"o"`
	Size int32 `json:"n"`
}

// record is one line of the main JSONL log. Exactly one of the following holds:
//
//   - Deleted: a tombstone removing Key.
//   - Blob != nil: the value lives in blobs.dat at the given ref.
//   - otherwise: the inline value (possibly empty) — on disk as EITHER a
//     readable JSON string ("s", valid-UTF-8 values) or base64 ("v", binary).
//
// In memory the canonical form is always Value []byte; Text exists only across
// marshal/unmarshal. The split is decided by utf8.Valid — an exact predicate,
// not a heuristic: a JSON string carries any valid UTF-8 losslessly (escaping
// preserves newlines, quotes, even NUL byte-exactly), and the only bytes it
// CANNOT carry are invalid UTF-8, which is precisely what json.Marshal would
// corrupt (U+FFFD substitution) — so those stay base64. This keeps text
// payloads grep-able in main.jsonl and ~25% smaller.
//
// Version skew: binaries older than the "s" field decode such records as an
// EMPTY value (unknown field ignored) — not corrupted, but invisible.
// Acceptable for a best-effort store; compaction rewrites old "v" text records
// into "s" form as a side effect of its read-transform-write pass.
//
// The JSON keys are kept short ("k","s","v","b","del") because every record is
// written to disk; omitempty keeps small records compact.
type record struct {
	Key     string         `json:"k"`
	Text    string         `json:"s,omitempty"` // inline value, valid UTF-8: readable string
	Value   []byte         `json:"v,omitempty"` // inline value, binary: base64 in JSON
	Blob    *blobRef       `json:"b,omitempty"` // set when value is stored as a blob
	Deleted bool           `json:"del,omitempty"`
	IX      map[string]any `json:"ix,omitempty"` // secondary index values for this key
}

// isTombstone reports whether the record deletes its key.
func (r record) isTombstone() bool { return r.Deleted }

// marshalRecord encodes a record as a single JSON object with no trailing
// newline. The log layer is responsible for framing (appending "\n").
// Valid-UTF-8 inline values are moved to the readable Text field here (see the
// record doc for why utf8.Valid is exact, not heuristic).
func marshalRecord(r record) ([]byte, error) {
	if len(r.Value) > 0 && utf8.Valid(r.Value) {
		r.Text, r.Value = string(r.Value), nil
	}
	return json.Marshal(r)
}

// unmarshalRecord decodes a single JSON record line, normalizing the value back
// to the canonical Value []byte form regardless of which on-disk field carried
// it. The caller must strip any trailing newline first.
func unmarshalRecord(line []byte) (record, error) {
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return record{}, err
	}
	if r.Text != "" {
		r.Value, r.Text = []byte(r.Text), ""
	}
	return r, nil
}
