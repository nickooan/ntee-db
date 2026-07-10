package nteedb

import (
	"encoding/json"
	"os"
)

const (
	metaFile          = "meta.json"
	metaFormatVersion = 1
)

// metaIndex records one declared secondary index. Complete is true once the
// index is known to cover every existing record — false means it was added (or
// its kind changed) while records already existed and has not been back-filled
// via Reindex, so it is "prospective" (covers only records written since).
type metaIndex struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // "string" | "number"
	Complete bool   `json:"complete"`
	// Dropped marks an index that is no longer declared but whose values may
	// still linger in records (a soft-drop tombstone). Its data is preserved by
	// Compact and removed only by Reindex.
	Dropped bool `json:"dropped,omitempty"`
}

// metaData is the on-disk meta.json contents: the store's declared index schema.
type metaData struct {
	Version int         `json:"version"`
	Indexes []metaIndex `json:"indexes"`
}

// parseKind converts a meta kind string back into a ValueKind.
func parseKind(s string) (ValueKind, bool) {
	switch s {
	case "number":
		return KindNumber, true
	case "string":
		return KindString, true
	default:
		return KindString, false
	}
}

// writeMeta atomically writes the index schema to path (temp + rename).
func writeMeta(path string, indexes []metaIndex) (err error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(metaData{Version: metaFormatVersion, Indexes: indexes}); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadMeta reads meta.json. ok is false if it is missing or unparseable.
func loadMeta(path string) (m metaData, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return metaData{}, false
	}
	if json.Unmarshal(b, &m) != nil {
		return metaData{}, false
	}
	return m, true
}
