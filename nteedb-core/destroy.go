package nteedb

import (
	"os"
	"path/filepath"
)

// storeFiles are the files a store owns within its directory, including the
// transient temp files compaction/hint writes may leave behind. Blob files are
// generation-numbered (blobs.dat, blobs.<n>.dat) and removed by glob instead.
var storeFiles = []string{
	mainFile,
	mainFile + ".compact",
	hintFile,
	hintFile + ".tmp",
	metaFile,
	metaFile + ".tmp",
	lockFile,
}

// Destroy deletes all of a store's files in dir. The store must not be open. The
// directory itself is left in place. Missing files are ignored, so it is safe to
// call on a partially-created or already-destroyed store.
func Destroy(dir string) error {
	for _, name := range storeFiles {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	gens, err := discoverBlobGens(dir)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if err := os.Remove(blobGenPath(dir, g)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Drop closes the DB and deletes all of its files. The DB must not be used
// afterward. It is the "drop the whole store" operation.
func (db *DB) Drop() error {
	dir := db.opts.Dir
	if err := db.Close(); err != nil {
		return err
	}
	return Destroy(dir)
}
