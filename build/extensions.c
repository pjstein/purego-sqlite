/*
** Auto-registration for extensions compiled into this library.
**
** A statically linked SQLite extension is just an init function; something has
** to call it. sqlite3_auto_extension registers an init function to run on every
** new connection, which is what makes a compiled-in extension behave like a
** built-in rather than something each connection has to remember to enable.
**
** This file is compiled only when at least one extension is enabled, and the
** Go binding looks its entry point up at load. A library built without
** extensions simply does not have the symbol, and the binding carries on --
** the same feature-detection contract the row-batching shim uses, for the same
** reason: one Go package has to work against several builds of the library.
*/

#include <stdint.h>

typedef struct sqlite3 sqlite3;
extern int sqlite3_auto_extension(void (*xEntryPoint)(void));

#ifdef SQLITE_VEC_ENABLED
extern int sqlite3_vec_init(sqlite3 *db, char **pzErrMsg, const void *pApi);
#endif

/*
** Registers every compiled-in extension for auto-loading and returns a bitmask
** of what was registered, so the Go side can report capability without having
** to run a query per extension to discover it.
**
**   bit 0 -- sqlite-vec
*/
int sqlite_ext_autoregister(void) {
  int mask = 0;
#ifdef SQLITE_VEC_ENABLED
  /* The cast is upstream's own documented idiom for this call: the registry
  ** stores a generic function pointer and passes the three real arguments
  ** through when SQLite invokes it. */
  if (sqlite3_auto_extension((void (*)(void))sqlite3_vec_init) == 0) {
    mask |= 1;
  }
#endif
  return mask;
}

/* Reports the mask this build could ever set, so "not compiled in" and
** "compiled in but failed to register" stay distinguishable. */
int sqlite_ext_available(void) {
  int mask = 0;
#ifdef SQLITE_VEC_ENABLED
  mask |= 1;
#endif
  return mask;
}
