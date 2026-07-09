package nteedb

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenIsExclusive(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	// A second opener — even in the same process, flock conflicts across
	// separate descriptors — must fail fast with ErrLocked.
	if _, err := Open(Options{Dir: dir}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}

	// Releasing via Close hands the store to the next opener.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	db2.Close()
}

func TestLockReleasedOnFailedOpen(t *testing.T) {
	dir := t.TempDir()
	// Force Open to fail after the lock is acquired: a directory where the
	// main log file should be makes openMainLog error out.
	if err := os.Mkdir(filepath.Join(dir, mainFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir}); err == nil {
		t.Fatal("expected Open to fail with a directory at the main log path")
	}

	// The failed Open must have released the lock — a fresh Open (after
	// clearing the obstruction) succeeds.
	if err := os.Remove(filepath.Join(dir, mainFile)); err != nil {
		t.Fatal(err)
	}
	db, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open after failed Open should succeed: %v", err)
	}
	db.Close()
}

func TestDropReleasesLock(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	db.Put("k", []byte("v"))
	if err := db.Drop(); err != nil {
		t.Fatal(err)
	}
	// Drop closed (releasing the lock) and destroyed; a fresh store opens.
	db2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open after Drop: %v", err)
	}
	defer db2.Close()
	if db2.Has("k") {
		t.Error("store should be empty after Drop")
	}
}
