package main

import (
	"sync"
	"sync/atomic"

	nteedb "codeberg.org/nickoan/ntee-r1quest/ntee-db"
)

// The handle registry maps opaque uint32 handles (held by the caller across the
// C/FFI boundary) to live *nteedb.DB instances. A Go pointer must never cross
// into C, so callers get an integer handle instead. uint32 maps cleanly to a JS
// number (no BigInt), and a 32-bit counter is far more than enough opens.
//
// It is a sync.Map because the access pattern is write-once / read-many: a
// handle is stored once at open and removed once at close/drop, but read on
// every operation. sync.Map makes regGet a lock-free atomic load in that steady
// state, so scans/puts/gets don't serialize on a mutex for a one-entry lookup.
var (
	regNext  atomic.Uint32 // hands out handles; first is 1 (0 stays invalid)
	registry sync.Map      // uint32 handle -> *nteedb.DB
)

func regPut(db *nteedb.DB) uint32 {
	h := regNext.Add(1)
	registry.Store(h, db)
	return h
}

func regGet(h uint32) *nteedb.DB {
	v, ok := registry.Load(h)
	if !ok {
		return nil
	}
	return v.(*nteedb.DB)
}

func regDelete(h uint32) *nteedb.DB {
	v, ok := registry.LoadAndDelete(h)
	if !ok {
		return nil
	}
	return v.(*nteedb.DB)
}
