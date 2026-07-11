package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestJSONValue(t *testing.T) {
	// Valid JSON is embedded verbatim.
	if v, ok := jsonValue([]byte(`{"a":1}`)).(json.RawMessage); !ok || string(v) != `{"a":1}` {
		t.Errorf("valid JSON not passed through: %v", v)
	}
	// Non-JSON (binary or plain text) is base64-wrapped.
	wrapped, ok := jsonValue([]byte("hello world")).(map[string]any)
	if !ok || wrapped["bin"] != true || wrapped["base64"] != "aGVsbG8gd29ybGQ=" {
		t.Errorf("non-JSON not wrapped: %v", wrapped)
	}
	if _, ok := jsonValue([]byte{0xff, 0x00, 0x01}).(map[string]any); !ok {
		t.Error("binary not wrapped")
	}
}

func TestRestAfterTokens(t *testing.T) {
	line := []byte(`put  k1   {"a": "b c", "n": 1}`)
	if got := string(restAfterTokens(line, 2)); got != `{"a": "b c", "n": 1}` {
		t.Errorf("got %q", got)
	}
	if got := restAfterTokens([]byte("put k1"), 2); got != nil {
		t.Errorf("expected nil for missing rest, got %q", got)
	}
}

func TestReadLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("get a\r\nget b\nx"), 16)
	for _, want := range []string{"get a", "get b"} {
		line, err := readLine(r)
		if err != nil || string(line) != want {
			t.Fatalf("got %q, %v; want %q", line, err, want)
		}
	}
	// Unterminated trailing bytes surface the underlying error.
	if _, err := readLine(r); err == nil {
		t.Fatal("expected error on unterminated line")
	}
	// A line longer than the buffer is rejected, not silently split.
	r = bufio.NewReaderSize(strings.NewReader(strings.Repeat("a", 64)+"\n"), 16)
	if _, err := readLine(r); !errors.Is(err, errLineTooLong) {
		t.Fatalf("want errLineTooLong, got %v", err)
	}
}

func TestReadData(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("abc\r\ndef\nrest"))
	got, err := readData(r, 3, 10)
	if err != nil || string(got) != "abc" {
		t.Fatalf("crlf block: %q %v", got, err)
	}
	got, err = readData(r, 3, 10)
	if err != nil || string(got) != "def" {
		t.Fatalf("lf block: %q %v", got, err)
	}
	// Data may contain \r\n — the reader trusts the length, not delimiters.
	r = bufio.NewReader(bytes.NewReader([]byte("a\r\nb\n")))
	if got, err = readData(r, 4, 10); err != nil || string(got) != "a\r\nb" {
		t.Fatalf("embedded newline block: %q %v", got, err)
	}
	// Over the limit.
	if _, err := readData(bufio.NewReader(strings.NewReader("xxxxx\n")), 5, 4); !errors.Is(err, errValueTooBig) {
		t.Fatalf("want errValueTooBig, got %v", err)
	}
	// Missing terminator.
	if _, err := readData(bufio.NewReader(strings.NewReader("abcX")), 3, 10); !errors.Is(err, errBadDataEnd) {
		t.Fatalf("want errBadDataEnd, got %v", err)
	}
}

func TestEnvelopeShapes(t *testing.T) {
	var buf bytes.Buffer
	rw := respWriter{w: bufio.NewWriter(&buf)}

	rw.ok("pong")
	rw.found(false, jsonNull)
	rw.fail("nope %d", 7)
	rw.w.Flush()

	want := `{"ok":true,"result":"pong"}` + "\n" +
		`{"ok":true,"found":false,"result":null}` + "\n" +
		`{"ok":false,"err":"nope 7"}` + "\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}
