package nteedb

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
)

// errStopScan is returned by a scanMainLog callback to stop the scan and treat
// the current record as the start of a torn tail (goodEnd excludes it).
var errStopScan = errors.New("nteedb: stop scan")

// mainLog is the append-only writer for main.jsonl — the store's main table and
// source of truth (not an auxiliary action log). It tracks the file size so each
// append can report the byte offset of the record it wrote without an extra stat.
type mainLog struct {
	f    *os.File
	size int64
	sync bool // fsync after every append
}

// openMainLog opens (creating if necessary) the main log for appending.
func openMainLog(path string, sync bool) (*mainLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &mainLog{f: f, size: info.Size(), sync: sync}, nil
}

// append writes r as one JSONL line and returns the byte offset at which it was
// written and the total bytes written (including the trailing newline). It
// fsyncs per write when the log was opened in durable mode.
func (l *mainLog) append(r record) (off int64, n int32, err error) {
	return l.appendSync(r, l.sync)
}

// appendSync is append with an explicit per-write fsync decision: batch writers
// pass false and issue one flush at the end of the batch instead.
func (l *mainLog) appendSync(r record, sync bool) (off int64, n int32, err error) {
	data, err := marshalRecord(r)
	if err != nil {
		return 0, 0, err
	}
	data = append(data, '\n')
	off = l.size
	if _, err := l.f.Write(data); err != nil {
		return 0, 0, err
	}
	if sync {
		if err := l.f.Sync(); err != nil {
			return 0, 0, err
		}
	}
	n = int32(len(data))
	l.size += int64(n)
	return off, n, nil
}

// flush fsyncs the underlying file.
func (l *mainLog) flush() error { return l.f.Sync() }

// close closes the underlying file.
func (l *mainLog) close() error { return l.f.Close() }

// scanMainLog reads records from the log at path starting at byte offset `from`,
// invoking fn for each complete, parseable record with its offset and total
// byte length (including the trailing newline).
//
// It returns goodEnd: the offset just past the last complete, parseable record.
// A torn final line (crash mid-append: no trailing newline) or a line that
// fails to parse is treated as the end of valid data — fn is not called for it,
// and goodEnd points at its start so the caller can truncate the file there.
//
// A missing file is not an error: goodEnd == from and fn is never called.
func scanMainLog(path string, from int64, fn func(r record, off int64, n int32) error) (goodEnd int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return from, nil
		}
		return 0, err
	}
	defer f.Close()

	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReader(f)
	off := from
	for {
		line, rerr := r.ReadBytes('\n')
		if rerr == io.EOF {
			// Any bytes here lack a trailing newline → torn tail; ignore them.
			return off, nil
		}
		if rerr != nil {
			return 0, rerr
		}
		rec, perr := unmarshalRecord(bytes.TrimSuffix(line, []byte{'\n'}))
		if perr != nil {
			// Corrupt line → treat as the start of the torn tail.
			return off, nil
		}
		n := int32(len(line))
		if ferr := fn(rec, off, n); ferr != nil {
			if errors.Is(ferr, errStopScan) {
				// The callback declared this record the start of the torn
				// tail: stop cleanly, goodEnd excludes it.
				return off, nil
			}
			return 0, ferr
		}
		off += int64(n)
	}
}
