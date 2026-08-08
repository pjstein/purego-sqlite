// Command sqliteversion reports what SQLite the package actually loaded, so
// the shipped binary can be checked against the pin without running the suite.
package main

import (
	"fmt"
	"os"

	sqlite3 "github.com/pjstein/purego-sqlite"
)

func main() {
	version, err := sqlite3.Version()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqliteversion:", err)
		os.Exit(1)
	}
	source, _ := sqlite3.SourceID()
	mode, _ := sqlite3.ThreadSafeMode()

	fmt.Printf("loaded:  SQLite %s\n", version)
	fmt.Printf("source:  %s\n", source)
	fmt.Printf("threads: SQLITE_THREADSAFE=%d\n", mode)
	fmt.Printf("library: %s\n", sqlite3.LoadedLibraryPath())
}
