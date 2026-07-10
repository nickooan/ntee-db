package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Wire format: requests are single lines of space-separated tokens (\r\n or
// \n), optionally followed by length-prefixed raw data blocks (put/putx).
// Every response is exactly one line of JSON:
//
//	{"ok":true,"result":…}            success
//	{"ok":true,"found":…,"result":…}  get
//	{"ok":false,"err":"…"}            failure
var (
	errLineTooLong = errors.New("protocol: line too long")
	errValueTooBig = errors.New("protocol: value exceeds size limit")
	errBadDataEnd  = errors.New("protocol: data block not terminated by newline")
)

// readLine reads one command line, tolerating \r\n or \n. The returned slice
// is only valid until the next read on r (it aliases the bufio buffer).
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errLineTooLong
	}
	if err != nil {
		return nil, err // io.EOF and friends: connection is done
	}
	line = line[:len(line)-1] // strip \n
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line, nil
}

// readData reads an n-byte raw data block plus its trailing newline.
func readData(r *bufio.Reader, n, max int) ([]byte, error) {
	if n < 0 || n > max {
		return nil, errValueTooBig
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	// Expect \r\n or \n.
	b, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if b == '\r' {
		if b, err = r.ReadByte(); err != nil {
			return nil, err
		}
	}
	if b != '\n' {
		return nil, errBadDataEnd
	}
	return buf, nil
}

// splitCommand tokenizes a command line into (command, args). The command is
// lower-cased; args keep their case.
func splitCommand(line []byte) (string, []string) {
	fields := strings.Fields(string(line))
	if len(fields) == 0 {
		return "", nil
	}
	return strings.ToLower(fields[0]), fields[1:]
}

// restAfterTokens returns the remainder of line after the first n
// space-separated tokens — used by inline put, where the value is
// "rest of line" and may itself contain spaces.
func restAfterTokens(line []byte, n int) []byte {
	rest := bytes.TrimLeft(line, " ")
	for i := 0; i < n; i++ {
		sp := bytes.IndexByte(rest, ' ')
		if sp < 0 {
			return nil
		}
		rest = bytes.TrimLeft(rest[sp+1:], " ")
	}
	return rest
}

// jsonValue prepares a stored value for embedding in the response envelope:
// valid JSON is embedded verbatim; anything else (binary, or text that is not
// JSON) is wrapped as {"bin":true,"base64":"…"} — responses must stay
// single-line pure JSON.
func jsonValue(value []byte) any {
	if json.Valid(value) {
		return json.RawMessage(value)
	}
	return map[string]any{"bin": true, "base64": base64.StdEncoding.EncodeToString(value)}
}

type respWriter struct {
	w *bufio.Writer
}

type envelope struct {
	OK     bool   `json:"ok"`
	Found  *bool  `json:"found,omitempty"`
	Result any    `json:"result,omitempty"`
	Err    string `json:"err,omitempty"`
}

func (rw respWriter) send(env envelope) error {
	buf, err := json.Marshal(env)
	if err != nil {
		// Should be unreachable (we only marshal marshalable values); keep the
		// connection consistent by reporting instead of writing nothing.
		buf, _ = json.Marshal(envelope{OK: false, Err: fmt.Sprintf("encode response: %v", err)})
	}
	if _, err := rw.w.Write(buf); err != nil {
		return err
	}
	return rw.w.WriteByte('\n')
}

func (rw respWriter) ok(result any) error {
	if result == nil {
		result = true // commands with no natural payload still return a result
	}
	return rw.send(envelope{OK: true, Result: result})
}

func (rw respWriter) found(found bool, result any) error {
	return rw.send(envelope{OK: true, Found: &found, Result: result})
}

func (rw respWriter) fail(format string, args ...any) error {
	return rw.send(envelope{OK: false, Err: fmt.Sprintf(format, args...)})
}
