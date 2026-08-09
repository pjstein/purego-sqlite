include sqlite-version.lock

GOOS_ARCH := $(shell go env GOOS)_$(shell go env GOARCH)

.PHONY: all fetch build ship-lib manifest test bench vet fmt check clean distclean version

all: build test

## fetch: download and checksum-verify the pinned SQLite amalgamation
fetch:
	@build/fetch.sh

## build: compile the pinned amalgamation for the host platform, no extensions
build: fetch
	@build/build-$(shell go env GOOS)-$(shell go env GOARCH).sh

## ship-lib: compile the exact configuration committed under lib/ -- which is
## `build` plus sqlite-vec. Use this and not `build` when re-pinning, or the
## committed library will quietly lose an extension consumers already have.
ship-lib: fetch
	@WITH_SQLITE_VEC=1 build/build-$(shell go env GOOS)-$(shell go env GOARCH).sh
	@$(MAKE) --no-print-directory manifest

## manifest: rewrite lib/MANIFEST.txt from the libraries actually on disk
manifest:
	@build/write-manifest.sh

## test: run the package suite on both read paths, batched and per-call
test:
	go test ./...
	PUREGO_SQLITE_BATCH=0 go test ./...

## vet: go vet over the package
vet:
	go vet ./...

## fmt: report anything gofmt would rewrite (empty output means clean)
fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

## check: what CI runs -- formatting, vet, and the suite both ways
check: fmt vet test

## bench: run the three-way bake-off (separate module under bench/)
bench:
	cd bench && go test -run '^$$' -bench . -benchtime 10x ./...

## version: what this repository pins and what the built library reports
version:
	@echo "pinned:  SQLite $(SQLITE_VERSION)  sha256 $(SQLITE_SHA256)"
	@go run ./cmd/sqliteversion 2>/dev/null || true

clean:
	rm -rf build/upstream

distclean: clean
	rm -f lib/*/libsqlite3.dylib lib/*/libsqlite3.so
