package nteedb

import "os"

const blobFile = "blobs.dat"

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
