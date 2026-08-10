package sqlite3_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjstein/purego-sqlite/sqlite3"
)

// TestCompiledExtensionsMatchTheBuild states which library this run got. It
// does not fail on absence, because extensions are opt-in at build time — but
// it does fail if the package claims an extension is registered and the
// extension then does not answer, which is the discrepancy that matters.
func TestCompiledExtensionsMatchTheBuild(t *testing.T) {
	exts, err := sqlite3.CompiledExtensions()
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) == 0 {
		t.Log("no extensions compiled in; build with `WITH_SQLITE_VEC=1 make build` to include sqlite-vec")
		return
	}
	t.Logf("compiled in: %v", exts)

	for _, e := range exts {
		has, err := sqlite3.HasExtension(e)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Errorf("CompiledExtensions lists %s but HasExtension says no", e)
		}
	}
}

// TestSQLiteVec proves the compiled-in extension answers a real query rather
// than merely linking. Skipped when the library was built without it.
func TestSQLiteVec(t *testing.T) {
	has, err := sqlite3.HasExtension(sqlite3.ExtSQLiteVec)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Skip("library built without sqlite-vec; `WITH_SQLITE_VEC=1 make build` to include it")
	}

	c := open(t)

	var version string
	if err := queryOne(c, `SELECT vec_version()`, &version); err != nil {
		t.Fatalf("vec_version(): %v", err)
	}
	if !strings.HasPrefix(version, "v") {
		t.Errorf("vec_version() = %q, want something version-shaped", version)
	}
	t.Logf("sqlite-vec %s", version)

	mustExec(t, c, `CREATE VIRTUAL TABLE v USING vec0(embedding float[4])`)
	mustExec(t, c, `INSERT INTO v(rowid, embedding) VALUES (1, '[1,2,3,4]'), (2, '[9,9,9,9]'), (3, '[1,2,3,5]')`)

	st, err := c.Prepare(`SELECT rowid, distance FROM v WHERE embedding MATCH ? ORDER BY distance LIMIT 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Bind("[1,2,3,4]"); err != nil {
		t.Fatal(err)
	}

	var got []int64
	for {
		row, err := st.Step()
		if err != nil {
			t.Fatal(err)
		}
		if !row {
			break
		}
		got = append(got, st.ColumnInt64(0))
	}
	if len(got) != 2 {
		t.Fatalf("KNN returned %d rows, want 2", len(got))
	}
	// The exact vector is nearest, then the one differing in a single component.
	if got[0] != 1 || got[1] != 3 {
		t.Errorf("nearest neighbours %v, want [1 3]", got)
	}

	// The extension must be registered on every connection, not just the first.
	second := open(t)
	if err := queryOne(second, `SELECT vec_version()`, &version); err != nil {
		t.Errorf("sqlite-vec absent on a second connection: %v", err)
	}
}

// TestRuntimeExtensionLoading exercises the other path: a shared library
// loaded at run time rather than compiled in.
//
// It builds its own tiny extension, so it needs a C compiler and the pinned
// SQLite headers, and skips when either is missing. The extension is written
// the way SQLite requires — every call through the sqlite3_api routines table.
// An extension that references libsqlite3's symbols directly links against
// nothing and jumps to a null pointer on first use, which is how the first
// draft of this test crashed the process rather than failing.
func TestRuntimeExtensionLoading(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler")
	}
	headers, err := filepath.Glob("build/upstream/sqlite-amalgamation-*/sqlite3ext.h")
	if err != nil || len(headers) == 0 {
		t.Skip("pinned SQLite headers absent; run `make fetch`")
	}
	includeDir := filepath.Dir(headers[0])

	dir := t.TempDir()
	src := filepath.Join(dir, "testext.c")
	if err := os.WriteFile(src, []byte(testExtensionSource), 0o644); err != nil {
		t.Fatal(err)
	}
	dylib := filepath.Join(dir, "testext.dylib")
	build := exec.Command(cc, "-O1", "-fPIC", "-dynamiclib", "-I", includeDir, "-o", dylib, src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build the test extension: %v\n%s", err, out)
	}

	c := open(t)

	// Loading must be refused until the application asks for it.
	if err := c.LoadExtension(dylib, "sqlite3_testext_init"); err == nil {
		t.Error("an extension loaded without EnableLoadExtension")
	}

	if err := c.EnableLoadExtension(true); err != nil {
		t.Fatalf("EnableLoadExtension: %v", err)
	}
	if err := c.LoadExtension(dylib, "sqlite3_testext_init"); err != nil {
		t.Fatalf("LoadExtension: %v", err)
	}

	var got string
	if err := queryOne(c, `SELECT runtime_hello()`, &got); err != nil {
		t.Fatalf("calling the loaded function: %v", err)
	}
	if got != "loaded-at-runtime" {
		t.Errorf("runtime_hello() = %q", got)
	}

	// Enabling the C API must not open the SQL door: a SQL string that can
	// load a dylib is a far larger blast radius than a Go call that can.
	if err := c.Exec(`SELECT load_extension('` + dylib + `')`); err == nil {
		t.Error("SQL load_extension() succeeded; the C-API-only enable is not holding")
	}

	if err := c.EnableLoadExtension(false); err != nil {
		t.Errorf("disabling: %v", err)
	}
}

const testExtensionSource = `
#include "sqlite3ext.h"
SQLITE_EXTENSION_INIT1

static void hello_func(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
  (void)argc; (void)argv;
  sqlite3_result_text(ctx, "loaded-at-runtime", -1, SQLITE_STATIC);
}

int sqlite3_testext_init(sqlite3 *db, char **pzErrMsg, const sqlite3_api_routines *pApi) {
  SQLITE_EXTENSION_INIT2(pApi);
  (void)pzErrMsg;
  return sqlite3_create_function(db, "runtime_hello", 0, SQLITE_UTF8, 0, hello_func, 0, 0);
}
`
