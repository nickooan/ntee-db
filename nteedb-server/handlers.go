package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"strconv"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

var jsonNull = json.RawMessage("null")

// dispatch runs one command. A returned error is fatal for the connection
// (stream desync / oversized frame); ordinary command failures are written to
// rw and return nil.
func (s *Server) dispatch(rw respWriter, r *bufio.Reader, line []byte, st *connState) (quit bool, fatal error) {
	cmd, args := splitCommand(line)

	switch cmd {
	// ---- session (no auth needed; same pre-auth allowlist as redis: AUTH,
	// HELLO, QUIT — notably ping is NOT on it) ----
	case "quit":
		return true, rw.ok(true)

	case "hello":
		// The handshake works pre-auth so a client can discover how to log
		// in, but the store's schema is only revealed once authenticated.
		result := map[string]any{
			"server":  "nteedb",
			"version": serverVersion,
			"auth":    s.auth.mode,
		}
		if st.authed {
			ixs := make([]map[string]string, 0, len(s.schema.Indexes))
			for _, ix := range s.schema.Indexes {
				ixs = append(ixs, map[string]string{"name": ix.Name, "kind": ix.Kind})
			}
			result["indexes"] = ixs
		}
		return false, rw.ok(result)

	case "auth":
		role, err := s.auth.check(args)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		st.authed, st.role = true, role
		return false, rw.ok(true)
	}

	if err := st.requireAuth(); err != nil {
		return false, rw.fail("%v", err)
	}

	switch cmd {
	case "ping":
		return false, rw.ok("pong")

	// ---- reads (parallel under the core's RLock) ----
	case "get":
		if len(args) != 1 {
			return false, rw.fail("usage: get <pk>")
		}
		value, ok, err := s.db.Get(args[0])
		if err != nil {
			return false, rw.fail("%v", err)
		}
		if !ok {
			return false, rw.found(false, jsonNull)
		}
		return false, rw.found(true, jsonValue(value))

	case "getm":
		if len(args) == 0 {
			return false, rw.fail("usage: getm <pk> [<pk> ...]")
		}
		values, found, err := s.db.GetMany(args)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		out := make([]map[string]any, len(args))
		for i, key := range args {
			entry := map[string]any{"key": key, "found": found[i], "value": jsonNull}
			if found[i] {
				entry["value"] = jsonValue(values[i])
			}
			out[i] = entry
		}
		return false, rw.ok(out)

	case "has":
		if len(args) != 1 {
			return false, rw.fail("usage: has <pk>")
		}
		return false, rw.ok(s.db.Has(args[0]))

	case "scan":
		prefix := ""
		if len(args) > 1 {
			return false, rw.fail("usage: scan [<prefix>]")
		}
		if len(args) == 1 {
			prefix = args[0]
		}
		keys, err := s.db.PrefixScan(prefix)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(nonNil(keys))

	case "ix":
		name, val, limit, err := s.indexArgs(cmd, args, true)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		keys, err := s.db.ByIndex(name, val, limit...)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(nonNil(keys))

	case "ixh":
		name, val, _, err := s.indexArgs(cmd, args, false)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		ok, err := s.db.ByIndexHas(name, val)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(ok)

	case "ixp":
		if len(args) < 2 || len(args) > 3 {
			return false, rw.fail("usage: ixp <index> <prefix> [±N]")
		}
		limit, err := parseLimit(args[2:])
		if err != nil {
			return false, rw.fail("%v", err)
		}
		keys, err := s.db.ByIndexPrefix(args[0], args[1], limit...)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(nonNil(keys))

	case "ixr":
		if len(args) != 3 {
			return false, rw.fail("usage: ixr <index> <lo> <hi>")
		}
		lo, err := s.indexValue(args[0], args[1])
		if err != nil {
			return false, rw.fail("%v", err)
		}
		hi, err := s.indexValue(args[0], args[2])
		if err != nil {
			return false, rw.fail("%v", err)
		}
		keys, err := s.db.ByIndexRange(args[0], lo, hi)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(nonNil(keys))

	case "ixrec":
		name, val, limit, err := s.indexArgs(cmd, args, true)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		keys, err := s.db.ByIndex(name, val, limit...)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		values, found, err := s.db.GetMany(keys)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		out := make([]map[string]any, 0, len(keys))
		for i, key := range keys {
			if !found[i] {
				continue // deleted between the index read and the value read
			}
			out = append(out, map[string]any{"key": key, "value": jsonValue(values[i])})
		}
		return false, rw.ok(out)

	// ---- writes (serialized by the core's write lock) ----
	case "put":
		if len(args) < 2 {
			return false, rw.fail("usage: put <pk> <nbytes> | put <pk> <inline json>")
		}
		var value []byte
		if n, err := strconv.Atoi(args[1]); err == nil && len(args) == 2 {
			// length-prefixed form: the data block follows
			value, err = readData(r, n, s.cfg.MaxValue)
			if err != nil {
				return false, err // stream position unknown → fatal
			}
		} else {
			value = bytes.Clone(restAfterTokens(line, 2))
		}
		if err := s.db.Put(args[0], value); err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(true)

	case "putx":
		if len(args) != 3 {
			return false, rw.fail("usage: putx <pk> <ixbytes> <nbytes>")
		}
		ixLen, err1 := strconv.Atoi(args[1])
		valLen, err2 := strconv.Atoi(args[2])
		if err1 != nil || err2 != nil {
			return false, rw.fail("usage: putx <pk> <ixbytes> <nbytes>")
		}
		ixRaw, err := readData(r, ixLen, s.cfg.MaxValue)
		if err != nil {
			return false, err
		}
		value, err := readData(r, valLen, s.cfg.MaxValue)
		if err != nil {
			return false, err
		}
		var ix nteedb.IndexValues
		if err := json.Unmarshal(ixRaw, &ix); err != nil {
			return false, rw.fail("putx: index values are not a JSON object: %v", err)
		}
		if err := s.db.PutIndexed(args[0], value, ix); err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(true)

	case "incr", "decr":
		if len(args) < 1 || len(args) > 2 {
			return false, rw.fail("usage: %s <pk> [delta]", cmd)
		}
		delta := int64(1)
		if len(args) == 2 {
			var err error
			delta, err = strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return false, rw.fail("%s: delta must be an integer, got %q", cmd, args[1])
			}
		}
		if cmd == "decr" {
			if delta == math.MinInt64 {
				return false, rw.fail("decr: delta out of range")
			}
			delta = -delta
		}
		v, err := s.db.Incr(args[0], delta)
		if err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(v)

	case "del":
		if len(args) != 1 {
			return false, rw.fail("usage: del <pk>")
		}
		if err := s.db.Delete(args[0]); err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(true)

	case "rml", "rmg":
		if len(args) != 1 {
			return false, rw.fail("usage: %s <cutoff-pk>", cmd)
		}
		var n int
		var err error
		if cmd == "rml" {
			n, err = s.db.RemoveByPkLess(args[0])
		} else {
			n, err = s.db.RemoveByPkGreater(args[0])
		}
		if err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(n)

	// ---- introspection ----
	case "stats":
		st := s.db.Stats()
		s.mu.Lock()
		open := len(s.conns)
		s.mu.Unlock()
		return false, rw.ok(map[string]any{
			"records":      st.Records,
			"mainBytes":    st.MainBytes,
			"liveBytes":    s.db.LiveBytes(),
			"blobBytes":    st.BlobBytes,
			"connections":  open,
			"totalConns":   s.totalConns.Load(),
			"commands":     s.commands.Load(),
			"autoCompacts": s.autoCompacts.Load(),
			"blobCompacts": s.blobCompacts.Load(),
		})

	case "dropped":
		return false, rw.ok(nonNil(s.db.DroppedIndexes()))

	case "prospective":
		return false, rw.ok(nonNil(s.db.ProspectiveIndexes()))

	// ---- admin (write lock held for a long time) ----
	case "compact":
		if err := st.requireAdmin(cmd); err != nil {
			return false, rw.fail("%v", err)
		}
		if err := s.db.Compact(); err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(true)

	case "reindex":
		if err := st.requireAdmin(cmd); err != nil {
			return false, rw.fail("%v", err)
		}
		if err := s.db.Reindex(); err != nil {
			return false, rw.fail("%v", err)
		}
		return false, rw.ok(true)

	case "relieve":
		// Explicit operator action: unconditional blob rewrite (the policy
		// thresholds only govern the automatic path).
		if err := st.requireAdmin(cmd); err != nil {
			return false, rw.fail("%v", err)
		}
		if err := s.db.BlobsRelieve(); err != nil {
			return false, rw.fail("%v", err)
		}
		s.blobCompacts.Add(1)
		return false, rw.ok(true)
	}

	return false, rw.fail("unknown command %q", cmd)
}

// indexArgs parses `<index> <value> [±N]` (limit only when allowed).
func (s *Server) indexArgs(cmd string, args []string, withLimit bool) (string, any, []int, error) {
	max := 2
	usage := "usage: " + cmd + " <index> <value>"
	if withLimit {
		max = 3
		usage += " [±N]"
	}
	if len(args) < 2 || len(args) > max {
		return "", nil, nil, errUsage(usage)
	}
	val, err := s.indexValue(args[0], args[1])
	if err != nil {
		return "", nil, nil, err
	}
	limit, err := parseLimit(args[2:])
	if err != nil {
		return "", nil, nil, err
	}
	return args[0], val, limit, nil
}

// indexValue converts a protocol token to the type the named index expects
// (float64 for number indexes). Unknown index names pass through as strings —
// the core reports them with its own error.
func (s *Server) indexValue(name, token string) (any, error) {
	if s.kinds[name] == nteedb.KindNumber {
		f, err := strconv.ParseFloat(token, 64)
		if err != nil {
			return nil, errUsage("index " + name + " holds numbers; " + strconv.Quote(token) + " is not a number")
		}
		return f, nil
	}
	return token, nil
}

func parseLimit(rest []string) ([]int, error) {
	if len(rest) == 0 {
		return nil, nil
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		return nil, errUsage("limit must be an integer (±N), got " + strconv.Quote(rest[0]))
	}
	return []int{n}, nil
}

type errUsage string

func (e errUsage) Error() string { return string(e) }

// nonNil keeps empty lists rendering as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
