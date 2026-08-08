/*
** bbx_step_row -- fetch an entire result row in one foreign call.
**
** Why this exists. Reading a four-column row through the SQLite C API costs
** seven calls: one step, one column_type or accessor per column, and a second
** call per text or blob column to learn its length. Through cgo those calls
** are cheap enough to ignore. Through purego each one costs roughly 70ns and
** one 208-byte heap allocation, so a 64-row page pays about 450 crossings and
** 900 allocations, and the garbage that produces hurts more than the calls
** once several readers run at once.
**
** Since this repository compiles its own SQLite, it can also ship a shim that
** does the whole row inside C and returns once. Nothing about SQLite's
** behaviour changes: this file calls the same public API a caller would, in
** the same order, with the same conversion semantics.
**
** The binding treats this as optional. It looks the symbol up at load and
** falls back to per-call accessors when it is absent, so pointing the package
** at a stock libsqlite3 still works -- it is only slower.
**
** Deliberately NOT done here: type conversion. This returns each value in its
** stored representation and the caller asks SQLite directly whenever it wants
** a value as some other type, because SQLite's conversion rules are its own
** and reimplementing them in Go would be a source of quiet disagreement.
*/

#include <stdint.h>

typedef struct sqlite3_stmt sqlite3_stmt;

extern int sqlite3_step(sqlite3_stmt *);
extern int sqlite3_column_count(sqlite3_stmt *);
extern int sqlite3_column_type(sqlite3_stmt *, int);
extern int64_t sqlite3_column_int64(sqlite3_stmt *, int);
extern double sqlite3_column_double(sqlite3_stmt *, int);
extern const void *sqlite3_column_blob(sqlite3_stmt *, int);
extern const unsigned char *sqlite3_column_text(sqlite3_stmt *, int);
extern int sqlite3_column_bytes(sqlite3_stmt *, int);

/* Layout is mirrored exactly by colValue in the Go binding. Field order is
** chosen so that natural alignment leaves no padding on any target: two
** 8-byte scalars, a pointer, then two 4-byte fields. */
typedef struct {
  int64_t ival;
  double fval;
  const void *ptr;
  int32_t len;
  int32_t type;
} bbx_col;

#define BBX_SQLITE_ROW 100
#define BBX_SQLITE_INTEGER 1
#define BBX_SQLITE_FLOAT 2
#define BBX_SQLITE_TEXT 3
#define BBX_SQLITE_BLOB 4

/*
** Steps the statement. On any result other than SQLITE_ROW the output buffer
** is untouched and the result code is returned unchanged. On SQLITE_ROW, the
** first min(ncol, column_count) columns are written into out.
**
** The pointers written for text and blob columns are SQLite's own and obey
** SQLite's usual lifetime rule: they are valid until the next step, reset, or
** finalize of this statement.
*/
int bbx_step_row(sqlite3_stmt *stmt, int ncol, bbx_col *out) {
  int rc = sqlite3_step(stmt);
  if (rc != BBX_SQLITE_ROW) {
    return rc;
  }

  int n = sqlite3_column_count(stmt);
  if (n > ncol) {
    n = ncol;
  }

  for (int i = 0; i < n; i++) {
    int t = sqlite3_column_type(stmt, i);
    out[i].type = t;
    out[i].ival = 0;
    out[i].fval = 0.0;
    out[i].ptr = 0;
    out[i].len = 0;

    switch (t) {
    case BBX_SQLITE_INTEGER:
      out[i].ival = sqlite3_column_int64(stmt, i);
      break;
    case BBX_SQLITE_FLOAT:
      out[i].fval = sqlite3_column_double(stmt, i);
      break;
    case BBX_SQLITE_TEXT:
      /* Pointer before length: fetching the pointer can convert the value and
      ** change its length, which is SQLite's own documented ordering rule. */
      out[i].ptr = sqlite3_column_text(stmt, i);
      out[i].len = sqlite3_column_bytes(stmt, i);
      break;
    case BBX_SQLITE_BLOB:
      out[i].ptr = sqlite3_column_blob(stmt, i);
      out[i].len = sqlite3_column_bytes(stmt, i);
      break;
    default: /* NULL: the zeroed fields above are the answer. */
      break;
    }
  }
  return rc;
}

/* Reports the size the Go side expects bbx_col to be, so a mismatch between
** this file and the binding is caught at load rather than by corrupting a
** result row. */
int bbx_col_size(void) { return (int)sizeof(bbx_col); }
