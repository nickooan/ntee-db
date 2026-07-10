package nteedb

import (
	"bufio"
	"encoding/json"
	"os"
)

const indexHintFormatVersion = 1

// indexHintMeta is the first line of an index-hint file — the boot-time
// snapshot of the in-memory indexes (main.jsonl.hint), a pure fast-boot
// optimization and never the source of truth.
type indexHintMeta struct {
	Version int   `json:"v"`
	Covers  int64 `json:"covers"` // main.jsonl is indexed up to this byte offset
}

// indexHintLine is one index entry in the index-hint file (sorted by key). IX
// carries the key's secondary index values so both the primary and secondary
// indexes can be rebuilt from the hint alone, without scanning the main log.
type indexHintLine struct {
	Key string         `json:"k"`
	Off int64          `json:"o"`
	N   int32          `json:"n"`
	IX  map[string]any `json:"ix,omitempty"`
}

// writeIndexHint atomically writes the index entries (in sorted order) plus
// the covers watermark to path, via a temp file + rename so a crash never
// leaves a half-written hint. The pk index passed in is either the live tree
// (sync checkpoints, under db.mu) or the background writer's COW clone; each
// entry carries its own ix values.
func writeIndexHint(path string, pk *pkIndex, covers int64) (err error) {
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

	w := bufio.NewWriter(f)
	if err = encodeJSONLine(w, indexHintMeta{Version: indexHintFormatVersion, Covers: covers}); err != nil {
		return err
	}
	pk.scan(func(e pkEntry) bool { // ascending key order
		err = encodeJSONLine(w, indexHintLine{Key: e.key, Off: e.off, N: e.n, IX: e.ix})
		return err == nil
	})
	if err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
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

func encodeJSONLine(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.WriteByte('\n')
}

// loadIndexHint reads an index-hint file and returns its entries (sorted by
// key) and the covers watermark. ok is false if the hint is missing or
// unparseable, in which case the caller should fall back to a full log scan —
// the hint is a pure optimization, never the source of truth.
func loadIndexHint(path string) (entries []indexHintLine, covers int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20) // allow long lines

	if !sc.Scan() {
		return nil, 0, false // no meta line
	}
	var meta indexHintMeta
	if json.Unmarshal(sc.Bytes(), &meta) != nil || meta.Version != indexHintFormatVersion {
		return nil, 0, false
	}

	for sc.Scan() {
		var hl indexHintLine
		if json.Unmarshal(sc.Bytes(), &hl) != nil {
			return nil, 0, false
		}
		entries = append(entries, hl)
	}
	if sc.Err() != nil {
		return nil, 0, false
	}
	return entries, meta.Covers, true
}
