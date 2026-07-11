package nteedb

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const blobFile = "blobs.dat"

// blobGenPath returns the path of a blob-file generation: gen 0 is the
// original "blobs.dat"; later generations (created by Relieve) are
// "blobs.<gen>.dat".
func blobGenPath(dir string, gen int) string {
	if gen == 0 {
		return filepath.Join(dir, blobFile)
	}
	return filepath.Join(dir, fmt.Sprintf("blobs.%d.dat", gen))
}

// discoverBlobGens lists the blob-file generations present in dir, in no
// particular order. A fresh store yields none.
func discoverBlobGens(dir string) ([]int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "blobs*.dat"))
	if err != nil {
		return nil, err
	}
	gens := make([]int, 0, len(matches))
	for _, m := range matches {
		name := filepath.Base(m)
		if name == blobFile {
			gens = append(gens, 0)
			continue
		}
		numeric := strings.TrimSuffix(strings.TrimPrefix(name, "blobs."), ".dat")
		if g, err := strconv.Atoi(numeric); err == nil && g > 0 {
			gens = append(gens, g)
		}
	}
	return gens, nil
}

// blobStore is the append-only side file holding large values. Keeping big
// values out of main.jsonl keeps that log's lines small, so index rebuilds and
// compaction stay cheap, and keeps large values off the heap (read on demand).
type blobStore struct {
	wf   *os.File // append writer
	rf   *os.File // read handle (ReadAt is concurrency-safe)
	size int64
}

func openBlobs(path string) (*blobStore, error) {
	wf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := wf.Stat()
	if err != nil {
		_ = wf.Close()
		return nil, err
	}
	rf, err := os.Open(path)
	if err != nil {
		_ = wf.Close()
		return nil, err
	}
	return &blobStore{wf: wf, rf: rf, size: info.Size()}, nil
}

// append writes value at the end of the blob file and returns a ref to it.
func (b *blobStore) append(value []byte) (blobRef, error) {
	off := b.size
	if _, err := b.wf.Write(value); err != nil {
		return blobRef{}, err
	}
	b.size += int64(len(value))
	return blobRef{Off: off, Size: int32(len(value))}, nil
}

// readAt returns the bytes referenced by ref.
func (b *blobStore) readAt(ref blobRef) ([]byte, error) {
	buf := make([]byte, ref.Size)
	if _, err := b.rf.ReadAt(buf, ref.Off); err != nil {
		return nil, err
	}
	return buf, nil
}

func (b *blobStore) flush() error { return b.wf.Sync() }

func (b *blobStore) close() error {
	err := b.wf.Close()
	if e := b.rf.Close(); e != nil && err == nil {
		err = e
	}
	return err
}
