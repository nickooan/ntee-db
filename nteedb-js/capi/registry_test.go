package main

import (
	"sync"
	"testing"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

// TestRegistryConcurrent hammers regPut/regGet/regDelete from many goroutines so
// `go test -race ./capi` proves the handle registry is race-free (the FFI exports
// call these from libuv worker threads for async ops). A dummy *nteedb.DB pointer
// is fine — the registry only stores/loads the pointer, it never dereferences it.
func TestRegistryConcurrent(t *testing.T) {
	const goroutines, iters = 16, 2000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				h := regPut(&nteedb.DB{})
				_ = regGet(h)
				if got := regDelete(h); got == nil {
					t.Errorf("regDelete(%d) = nil, want the stored handle", h)
					return
				}
				if regGet(h) != nil {
					t.Errorf("regGet(%d) after delete should be nil", h)
					return
				}
			}
		}()
	}
	wg.Wait()
}
