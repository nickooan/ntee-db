package nteedb_test

import (
	"fmt"
	"os"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

func Example() {
	dir, _ := os.MkdirTemp("", "nteedb-example")
	defer os.RemoveAll(dir)

	db, err := nteedb.Open(nteedb.Options{Dir: dir})
	if err != nil {
		panic(err)
	}

	// Namespacing keys turns a prefix scan into a "secondary index" by group.
	_ = db.Put("input:GetOrders", []byte("..."))
	_ = db.Put("input:GetProperty", []byte("..."))
	_ = db.Put("api:/orders", []byte("..."))

	keys, _ := db.PrefixScan("input:Get")
	for _, k := range keys {
		fmt.Println(k)
	}
	_ = db.Close()

	// Output:
	// input:GetOrders
	// input:GetProperty
}
