package main

import (
	"fmt"

	"github.com/pjstein/purego-sqlite/bench/cgoref"
)

func main() {
	fmt.Println("cgoref available:", cgoref.Available, "version:", cgoref.Version(), "nullcall:", cgoref.NullCall())
}
