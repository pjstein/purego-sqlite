package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	bakeoff "github.com/pjstein/purego-sqlite/bench"
	_ "modernc.org/sqlite"
)

func probe(name, dsn string) {
	dir, _ := filepath.Abs("/private/tmp/claude-501/-Users-pjstein-source-pjstein-blackbox-main/4ed16b4f-4091-46f7-b36b-018f07fa09f0/scratchpad/probe2")
	path := filepath.Join(dir, name+".db")
	db, err := sql.Open(name, path)
	if err != nil {
		fmt.Println(name, "open:", err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	for _, p := range bakeoff.Pragmas {
		r, err := db.Query(p)
		if err != nil {
			fmt.Println(name, p, err)
			return
		}
		r.Close()
	}
	for _, q := range []string{"PRAGMA journal_mode", "PRAGMA synchronous", "PRAGMA busy_timeout", "PRAGMA wal_autocheckpoint", "PRAGMA cache_size"} {
		var v any
		if err := db.QueryRow(q).Scan(&v); err == nil {
			fmt.Printf("   %-26s %v\n", q, v)
		} else {
			fmt.Printf("   %-26s err %v\n", q, err)
		}
	}
	if _, err := db.Exec(bakeoff.Schema); err != nil {
		fmt.Println(name, "schema:", err)
		return
	}
	st, err := db.Prepare(bakeoff.AppendSQL)
	if err != nil {
		fmt.Println(name, "prepare:", err)
		return
	}
	defer st.Close()
	payload := bakeoff.SeedPayload(bakeoff.DefaultPayloadSize)

	const n = 2000
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if _, err := st.Exec("s", "k", int64(i), payload); err != nil {
			fmt.Println(name, "exec:", err)
			return
		}
	}
	auto := time.Since(t0)

	tx, _ := db.Begin()
	tst := tx.Stmt(st)
	t1 := time.Now()
	for i := 0; i < n; i++ {
		if _, err := tst.Exec("s", "k", int64(i), payload); err != nil {
			fmt.Println(name, "tx exec:", err)
			return
		}
	}
	inTx := time.Since(t1)
	tx.Commit()

	fmt.Printf("   autocommit %8.1f us/append   in one tx %8.1f us/append\n",
		float64(auto.Microseconds())/n, float64(inTx.Microseconds())/n)
}

func main() {
	fmt.Println("== ncruces via database/sql (driver name sqlite3)")
	probe("sqlite3", "")
	fmt.Println("== modernc via database/sql (driver name sqlite)")
	probe("sqlite", "")
}
