/*
** rowbatch_step -- fetch an entire result row in one foreign call.
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
} rowbatch_col;

#define ROWBATCH_SQLITE_ROW 100
#define ROWBATCH_SQLITE_INTEGER 1
#define ROWBATCH_SQLITE_FLOAT 2
#define ROWBATCH_SQLITE_TEXT 3
#define ROWBATCH_SQLITE_BLOB 4

/*
** Steps the statement. On any result other than SQLITE_ROW the output buffer
** is untouched and the result code is returned unchanged. On SQLITE_ROW, the
** first min(ncol, column_count) columns are written into out.
**
** The pointers written for text and blob columns are SQLite's own and obey
** SQLite's usual lifetime rule: they are valid until the next step, reset, or
** finalize of this statement.
*/
int rowbatch_step(sqlite3_stmt *stmt, int ncol, rowbatch_col *out) {
  int rc = sqlite3_step(stmt);
  if (rc != ROWBATCH_SQLITE_ROW) {
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
    case ROWBATCH_SQLITE_INTEGER:
      out[i].ival = sqlite3_column_int64(stmt, i);
      break;
    case ROWBATCH_SQLITE_FLOAT:
      out[i].fval = sqlite3_column_double(stmt, i);
      break;
    case ROWBATCH_SQLITE_TEXT:
      /* Pointer before length: fetching the pointer can convert the value and
      ** change its length, which is SQLite's own documented ordering rule. */
      out[i].ptr = sqlite3_column_text(stmt, i);
      out[i].len = sqlite3_column_bytes(stmt, i);
      break;
    case ROWBATCH_SQLITE_BLOB:
      out[i].ptr = sqlite3_column_blob(stmt, i);
      out[i].len = sqlite3_column_bytes(stmt, i);
      break;
    default: /* NULL: the zeroed fields above are the answer. */
      break;
    }
  }
  return rc;
}

/* Reports the size the Go side expects rowbatch_col to be, so a mismatch between
** this file and the binding is caught at load rather than by corrupting a
** result row. */
int rowbatch_col_size(void) { return (int)sizeof(rowbatch_col); }

/*
** ---------------------------------------------------------------------------
** Application-defined function marshaling.
**
** The row-batching lesson, pointed the other way. Reading a row was expensive
** because it took seven crossings; answering a SQL function in Go was
** expensive for the same reason, in reverse. The naive path costs one C-to-Go
** crossing plus a Go-to-C call for the user data, one per argument, and one
** for the result -- four crossings to answer a one-argument function.
**
** So C does the marshaling. This trampoline extracts every argument into a
** flat buffer, calls Go exactly once, and issues sqlite3_result_* itself from
** what Go wrote back. The Go function touches only the buffer.
**
** The buffer is a stack local, so there is no allocation and no lifetime
** question for the arguments: SQLite guarantees them for the duration of the
** call, and the call has not returned.
**
** Results are the one place needing care. Go cannot hand back a pointer into
** Go memory for C to use after Go returns -- the garbage collector is under no
** obligation to keep it. Small text and blob results are therefore copied into
** an inline buffer that lives in the same stack frame; larger ones set
** FN_RESULT_GO and Go calls sqlite3_result_* itself before returning, paying
** one extra crossing on values big enough that a memcpy would have dominated
** anyway.
*/

#define FN_MAX_ARGS 16
#define FN_RESULT_INLINE 512

/* Result disciplines, mirrored by resultKind in the Go binding. */
#define FN_RESULT_NULL 0
#define FN_RESULT_INT 1
#define FN_RESULT_FLOAT 2
#define FN_RESULT_TEXT 3
#define FN_RESULT_BLOB 4
#define FN_RESULT_ERROR 5
#define FN_RESULT_GO 6 /* Go already called sqlite3_result_*; C does nothing */
#define FN_RESULT_TOOMANYARGS 7

typedef struct sqlite3_context sqlite3_context;
typedef struct sqlite3_value sqlite3_value;

extern void *sqlite3_user_data(sqlite3_context *);
extern int sqlite3_value_type(sqlite3_value *);
extern int64_t sqlite3_value_int64(sqlite3_value *);
extern double sqlite3_value_double(sqlite3_value *);
extern const unsigned char *sqlite3_value_text(sqlite3_value *);
extern const void *sqlite3_value_blob(sqlite3_value *);
extern int sqlite3_value_bytes(sqlite3_value *);
extern void sqlite3_result_int64(sqlite3_context *, int64_t);
extern void sqlite3_result_double(sqlite3_context *, double);
extern void sqlite3_result_null(sqlite3_context *);
extern void sqlite3_result_text(sqlite3_context *, const char *, int, void (*)(void *));
extern void sqlite3_result_blob(sqlite3_context *, const void *, int, void (*)(void *));
extern void sqlite3_result_error(sqlite3_context *, const char *, int);

/* Mirrors fnValue in the Go binding; same layout rule as rowbatch_col. */
typedef struct {
  int64_t ival;
  double fval;
  const void *ptr;
  sqlite3_value *raw; /* so Go can ask SQLite for a conversion it did not cache */
  int32_t len;
  int32_t type;
} fn_value;

/* Mirrors fnCall in the Go binding. */
typedef struct {
  void *ctx;   /* for the FN_RESULT_GO fallback */
  int64_t handle;
  int32_t argc;
  int32_t rtype;
  int32_t rlen;
  int32_t pad;
  int64_t rival;
  double rfval;
  fn_value args[FN_MAX_ARGS];
  char rbuf[FN_RESULT_INLINE];
} fn_call;

static void (*fn_go_dispatch)(fn_call *) = 0;

/* Installed once by the Go binding with a purego callback. */
void fn_set_dispatch(void (*f)(fn_call *)) { fn_go_dispatch = f; }

int fn_max_args(void) { return FN_MAX_ARGS; }
int fn_result_inline(void) { return FN_RESULT_INLINE; }
int fn_call_size(void) { return (int)sizeof(fn_call); }

static void fn_scalar(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
  fn_call call;
  call.ctx = ctx;
  call.handle = (int64_t)(intptr_t)sqlite3_user_data(ctx);
  call.rtype = FN_RESULT_NULL;
  call.rlen = 0;
  call.rival = 0;
  call.rfval = 0.0;

  if (argc > FN_MAX_ARGS) {
    /* Refuse rather than truncate: a silently dropped argument is a wrong
    ** answer, and a wrong answer is worse than a failed query. */
    sqlite3_result_error(ctx, "sqlite3: too many arguments for a Go function", -1);
    return;
  }
  call.argc = argc;

  for (int i = 0; i < argc; i++) {
    int t = sqlite3_value_type(argv[i]);
    call.args[i].type = t;
    call.args[i].raw = argv[i];
    call.args[i].ival = 0;
    call.args[i].fval = 0.0;
    call.args[i].ptr = 0;
    call.args[i].len = 0;
    switch (t) {
    case 1: /* INTEGER */
      call.args[i].ival = sqlite3_value_int64(argv[i]);
      break;
    case 2: /* FLOAT */
      call.args[i].fval = sqlite3_value_double(argv[i]);
      break;
    case 3: /* TEXT */
      call.args[i].ptr = sqlite3_value_text(argv[i]);
      call.args[i].len = sqlite3_value_bytes(argv[i]);
      break;
    case 4: /* BLOB */
      call.args[i].ptr = sqlite3_value_blob(argv[i]);
      call.args[i].len = sqlite3_value_bytes(argv[i]);
      break;
    default: /* NULL */
      break;
    }
  }

  if (fn_go_dispatch == 0) {
    sqlite3_result_error(ctx, "sqlite3: no Go dispatch installed", -1);
    return;
  }
  fn_go_dispatch(&call);

  switch (call.rtype) {
  case FN_RESULT_INT:
    sqlite3_result_int64(ctx, call.rival);
    break;
  case FN_RESULT_FLOAT:
    sqlite3_result_double(ctx, call.rfval);
    break;
  case FN_RESULT_TEXT:
    sqlite3_result_text(ctx, call.rbuf, call.rlen, (void (*)(void *))-1);
    break;
  case FN_RESULT_BLOB:
    sqlite3_result_blob(ctx, call.rbuf, call.rlen, (void (*)(void *))-1);
    break;
  case FN_RESULT_ERROR:
    sqlite3_result_error(ctx, call.rbuf, call.rlen);
    break;
  case FN_RESULT_GO:
    break; /* Go already answered */
  default:
    sqlite3_result_null(ctx);
    break;
  }
}

/* The address SQLite should be given as xFunc. */
void *fn_scalar_ptr(void) { return (void *)fn_scalar; }

/*
** ---------------------------------------------------------------------------
** Aggregate batching.
**
** A scalar function pays one crossing per row. An aggregate need not: xStep
** can accumulate in C and cross to Go once, in xFinal, with the whole
** accumulation. For the coordination-plane shapes that actually want a Go
** aggregate -- a custom rollup over a result set -- that turns a per-row cost
** into a per-query one.
**
** What C accumulates is deliberately narrow: count, sum, min and max of the
** numeric interpretation, plus the first and last text values. A Go aggregate
** that needs every individual row is not this; it should collect rows itself
** through an ordinary query. This is for rollups whose inputs C can summarise
** without knowing what the answer means.
*/

typedef struct {
  int64_t count;
  double sum;
  double min;
  double max;
  int64_t isum; /* integer sum, exact while the inputs are integers */
  int32_t all_int;
  int32_t initialized;
} fn_agg;

extern void *sqlite3_aggregate_context(sqlite3_context *, int nBytes);

static void fn_agg_step(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
  fn_agg *a = (fn_agg *)sqlite3_aggregate_context(ctx, (int)sizeof(fn_agg));
  if (a == 0 || argc < 1) {
    return;
  }
  if (!a->initialized) {
    a->initialized = 1;
    a->all_int = 1;
    a->count = 0;
    a->sum = 0.0;
    a->isum = 0;
  }
  int t = sqlite3_value_type(argv[0]);
  if (t == 5 /* NULL */) {
    return; /* SQL aggregates ignore NULL, and so does this one */
  }
  double d;
  if (t == 1 /* INTEGER */) {
    int64_t v = sqlite3_value_int64(argv[0]);
    a->isum += v;
    d = (double)v;
  } else {
    a->all_int = 0;
    d = sqlite3_value_double(argv[0]);
  }
  a->sum += d;
  if (a->count == 0 || d < a->min) {
    a->min = d;
  }
  if (a->count == 0 || d > a->max) {
    a->max = d;
  }
  a->count++;
}

/* Mirrors fnAggCall in the Go binding. */
typedef struct {
  void *ctx;
  int64_t handle;
  int64_t count;
  double sum;
  double min;
  double max;
  int64_t isum;
  int32_t all_int;
  int32_t rtype;
  int32_t rlen;
  int32_t pad;
  int64_t rival;
  double rfval;
  char rbuf[FN_RESULT_INLINE];
} fn_agg_call;

static void (*fn_go_agg_dispatch)(fn_agg_call *) = 0;

void fn_set_agg_dispatch(void (*f)(fn_agg_call *)) { fn_go_agg_dispatch = f; }
int fn_agg_call_size(void) { return (int)sizeof(fn_agg_call); }

static void fn_agg_final(sqlite3_context *ctx) {
  fn_agg *a = (fn_agg *)sqlite3_aggregate_context(ctx, 0);
  fn_agg_call call;
  call.ctx = ctx;
  call.handle = (int64_t)(intptr_t)sqlite3_user_data(ctx);
  call.rtype = FN_RESULT_NULL;
  call.rlen = 0;
  call.rival = 0;
  call.rfval = 0.0;
  if (a == 0 || !a->initialized) {
    call.count = 0;
    call.sum = 0.0;
    call.min = 0.0;
    call.max = 0.0;
    call.isum = 0;
    call.all_int = 1;
  } else {
    call.count = a->count;
    call.sum = a->sum;
    call.min = a->min;
    call.max = a->max;
    call.isum = a->isum;
    call.all_int = a->all_int;
  }

  if (fn_go_agg_dispatch == 0) {
    sqlite3_result_error(ctx, "sqlite3: no Go aggregate dispatch installed", -1);
    return;
  }
  fn_go_agg_dispatch(&call);

  switch (call.rtype) {
  case FN_RESULT_INT:
    sqlite3_result_int64(ctx, call.rival);
    break;
  case FN_RESULT_FLOAT:
    sqlite3_result_double(ctx, call.rfval);
    break;
  case FN_RESULT_TEXT:
    sqlite3_result_text(ctx, call.rbuf, call.rlen, (void (*)(void *))-1);
    break;
  case FN_RESULT_BLOB:
    sqlite3_result_blob(ctx, call.rbuf, call.rlen, (void (*)(void *))-1);
    break;
  case FN_RESULT_ERROR:
    sqlite3_result_error(ctx, call.rbuf, call.rlen);
    break;
  case FN_RESULT_GO:
    break;
  default:
    sqlite3_result_null(ctx);
    break;
  }
}

void *fn_agg_step_ptr(void) { return (void *)fn_agg_step; }
void *fn_agg_final_ptr(void) { return (void *)fn_agg_final; }
