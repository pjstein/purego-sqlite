//go:build (darwin || freebsd || linux || netbsd) && !android && !faketime

package sqlite3

import "github.com/ebitengine/purego"

// dlopen and dlsym are this package's single seam onto purego's dlfcn calls.
//
// purego defines Dlopen/Dlsym under exactly the constraint at the top of this
// file — its own dlfcn.go carries it verbatim — and mirroring it here rather
// than approximating it is deliberate: an approximation drifts the first time
// purego adds a platform, and it drifts into a build failure on somebody else's
// machine rather than ours.
//
// Everything that resolves a symbol goes through these two functions so the
// platform question is answered once. Before this file existed the calls were
// spread across four files with no build tags at all, and the package simply
// did not compile for GOOS=windows — which CI states it must, and which nothing
// had ever run to find out.
func dlopen(path string, mode int) (uintptr, error) { return purego.Dlopen(path, mode) }

func dlsym(handle uintptr, name string) (uintptr, error) { return purego.Dlsym(handle, name) }

// rtldNow and rtldGlobal are purego's constants under another name, for the
// same reason: they are only defined where dlfcn is.
const (
	rtldNow    = purego.RTLD_NOW
	rtldGlobal = purego.RTLD_GLOBAL
)
