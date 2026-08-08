include sqlite-version.lock

GOOS_ARCH := $(shell go env GOOS)_$(shell go env GOARCH)

.PHONY: all fetch build test bench clean distclean version

all: build test

## fetch: download and checksum-verify the pinned SQLite amalgamation
fetch:
	@build/fetch.sh

## build: compile the pinned amalgamation for the host platform
build: fetch
	@build/build-$(shell go env GOOS)-$(shell go env GOARCH).sh

## test: run the package suite against the shipped library
test:
	go test ./...

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
