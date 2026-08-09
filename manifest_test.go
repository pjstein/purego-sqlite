package sqlite3_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// TestManifestMatchesTheCommittedLibraries is the check that keeps lib/MANIFEST.txt
// honest. The manifest is what a consumer is told to verify a downloaded binary
// against, so a manifest that drifts from the files beside it is worse than no
// manifest at all: it fails a legitimate library and passes nothing.
//
// It hashes every library the manifest names, not only the host platform's, so
// running the suite on darwin still catches a linux .so that was rebuilt
// without `make manifest`.
func TestManifestMatchesTheCommittedLibraries(t *testing.T) {
	raw, err := os.ReadFile("lib/MANIFEST.txt")
	if err != nil {
		t.Fatalf("reading lib/MANIFEST.txt: %v", err)
	}

	type entry struct {
		platform string
		path     string
		size     string
		sha256   string
	}
	var entries []entry
	var cur *entry
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#"):
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			entries = append(entries, entry{platform: strings.Trim(trimmed, "[]")})
			cur = &entries[len(entries)-1]
		case cur == nil:
		default:
			field, value, ok := strings.Cut(trimmed, " ")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch field {
			case "path":
				cur.path = value
			case "size":
				cur.size = strings.TrimSuffix(value, " bytes")
			case "sha256":
				cur.sha256 = value
			}
		}
	}

	// A manifest that parsed to nothing would let this test pass without
	// having checked anything, which is the failure mode it exists to prevent.
	if len(entries) == 0 {
		t.Fatal("lib/MANIFEST.txt named no platforms; either it is malformed or the parser is")
	}

	for _, e := range entries {
		t.Run(e.platform, func(t *testing.T) {
			if e.path == "" || e.sha256 == "" {
				t.Fatalf("manifest entry [%s] is missing a path or sha256", e.platform)
			}
			data, err := os.ReadFile(e.path)
			if err != nil {
				t.Fatalf("manifest names %s but it cannot be read: %v", e.path, err)
			}
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != e.sha256 {
				t.Errorf("%s\n  sha256 on disk  %s\n  sha256 in manifest %s\nrun `make manifest` after rebuilding a library", e.path, got, e.sha256)
			}
		})
	}
}
