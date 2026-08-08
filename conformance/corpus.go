// Package conformance holds the value corpus this repository checks SQLite
// round-trips byte-for-byte, and the SQL expression cases whose results must
// agree across implementations.
//
// It is a separate package with no dependency on the binding so that anything
// can drive it: this repository's own test suite, and the bake-off under
// bench/, which runs the same corpus through competing SQLite drivers to check
// that they agree with each other and with upstream.
//
// The value corpus is not a random sample. Every entry is a class that has
// been observed to survive a naive implementation's tests and then lose data
// in production: text carrying real newlines, embedded NUL, characters outside
// the basic multilingual plane, and byte sequences that are not valid UTF-8 at
// all. Invisible characters are written as escapes so that what the corpus
// tests is legible in the source rather than only in a hex dump.
package conformance

import "strings"

// Value is one round-trip case: a name, the text to store in a TEXT column,
// and the bytes to store in a BLOB column.
type Value struct {
	Name string
	Text string
	Blob []byte
}

const (
	emojiAstral    = "black box \U0001F5A5️ and \U0001F680"
	emojiZWJ       = "\U0001F468\u200d\U0001F4BB"
	cjk            = "日本語のテキスト、中文文本，한국어"
	combiningNFC   = "é"
	combiningNFD   = "é"
	combining      = combiningNFC + " vs " + combiningNFD
	rtl            = "אבג العربية"
	zeroWidth      = "a\u200bb\ufeffc"
	bomLeading     = "\ufeffvalue"
	highCodepoints = "\ufffd\uffff\U0010FFFF"
)

// Values is the round-trip corpus. Storing Text in a TEXT column and Blob in a
// BLOB column and reading both back must return exactly what went in, byte for
// byte, with no truncation at a NUL and no normalization of anything.
func Values() []Value {
	text := func(name, s string) Value { return Value{Name: name, Text: s, Blob: []byte(s)} }

	return []Value{
		{Name: "empty", Text: "", Blob: []byte{}},
		text("ascii", "plain ascii value"),

		// The class that convicted a formatter: a scalar whose value contains
		// a real newline, not an escape sequence.
		text("newline-lf", "first line\nsecond line"),
		text("newline-crlf", "first line\r\nsecond line"),
		text("newline-cr", "first line\rsecond line"),
		text("newline-trailing", "value\n"),
		text("newline-only", "\n\n\n"),
		text("tabs-and-spaces", "  leading\tinner\ttrailing  "),

		// Embedded NUL: the class a length-inferring binding silently
		// truncates, in both directions.
		text("nul-embedded", "before\x00after"),
		text("nul-leading", "\x00trailing"),
		text("nul-trailing", "leading\x00"),
		text("nul-only", "\x00"),
		text("nul-many", "a\x00b\x00c\x00d"),

		// High Unicode: astral-plane codepoints, combining marks that are not
		// the same value as their precomposed forms, right-to-left text, and
		// zero-width characters that a trimming implementation eats.
		text("emoji-astral", emojiAstral),
		text("emoji-zwj-sequence", emojiZWJ),
		text("cjk", cjk),
		text("combining-marks", combining),
		text("rtl", rtl),
		text("zero-width", zeroWidth),
		text("bom-leading", bomLeading),
		text("high-codepoints", highCodepoints),

		// Not valid UTF-8. SQLite stores TEXT as the bytes it was given and
		// does not validate them; a binding that round-trips through a
		// validating conversion corrupts these.
		{Name: "invalid-utf8-lone-continuation", Text: "\x80\x81", Blob: []byte{0x80, 0x81}},
		{Name: "invalid-utf8-truncated", Text: "ok\xE2\x82", Blob: []byte{'o', 'k', 0xE2, 0x82}},
		text("all-bytes", allBytes()),

		// SQL metacharacters, which must be data and never syntax.
		text("quotes", `it's "quoted" and `+"`backticked`"),
		text("semicolons", "one; two; DROP TABLE t; --"),
		text("percent-underscore", "100% _ [bracket]"),

		// Size, because truncation bugs hide behind small test values.
		text("long-64k", strings.Repeat("abcdefgh", 8192)),
		text("long-with-nuls", strings.Repeat("x\x00y", 10000)),
	}
}

func allBytes() string {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return string(b)
}

// Expr is a SQL expression whose single-column result every implementation
// must agree on. These check the semantics a shared SQLite gives you --
// integer range, floating-point rendering, type affinity, NULL propagation,
// and the compiled-in function set -- rather than the transport.
type Expr struct {
	Name string
	SQL  string
	Want string // result rendered as text
}

// Exprs is the expression corpus.
func Exprs() []Expr {
	return []Expr{
		{"int-max", `SELECT 9223372036854775807`, "9223372036854775807"},
		{"int-min", `SELECT -9223372036854775807 - 1`, "-9223372036854775808"},
		{"int-overflow-to-float", `SELECT typeof(9223372036854775807 + 1)`, "real"},
		{"float-repr", `SELECT 0.1 + 0.2`, "0.30000000000000004"},
		{"float-int-division", `SELECT 7 / 2`, "3"},
		{"float-real-division", `SELECT 7.0 / 2`, "3.5"},
		{"null-propagation", `SELECT typeof(NULL + 1)`, "null"},
		{"null-concat", `SELECT quote('a' || NULL)`, "NULL"},
		{"affinity-text-to-int", `SELECT typeof(CAST('42' AS INTEGER))`, "integer"},
		{"affinity-numeric-string", `SELECT '10' + 5`, "15"},
		{"string-compare", `SELECT 'a' < 'B'`, "0"},
		{"like-case-insensitive-ascii", `SELECT 'ABC' LIKE 'abc'`, "1"},
		{"like-does-not-match-blob", `SELECT x'414243' LIKE 'ABC'`, "0"},
		{"length-of-nul-text", `SELECT length(CAST(x'610062' AS TEXT))`, "1"},
		{"octet-length-of-nul-text", `SELECT octet_length(CAST(x'610062' AS TEXT))`, "3"},
		{"unicode-length", `SELECT length('日本語')`, "3"},
		{"unicode-octet-length", `SELECT octet_length('日本語')`, "9"},
		{"hex-roundtrip", `SELECT hex(x'00ff10')`, "00FF10"},
		{"quote-blob", `SELECT quote(x'0001')`, "X'0001'"},
		{"json-extract", `SELECT json_extract('{"a":[1,{"b":2}]}', '$.a[1].b')`, "2"},
		{"json-type", `SELECT json_type('{"a":null}', '$.a')`, "null"},
		{"json-quote-newline", `SELECT json_quote('a` + "\n" + `b')`, `"a\nb"`},
		{"date-julian", `SELECT strftime('%Y-%m-%d', 2451545.0)`, "2000-01-01"},
		{"date-unix", `SELECT datetime(0, 'unixepoch')`, "1970-01-01 00:00:00"},
		{"printf-float", `SELECT printf('%.3f', 2.0/3)`, "0.667"},
		{"replace-newline", `SELECT replace('a` + "\n" + `b', char(10), '|')`, "a|b"},
		{"char-zero-is-empty", `SELECT length(char(0))`, "0"},
		{"substr-negative", `SELECT substr('abcdef', -2)`, "ef"},
		{"instr-unicode", `SELECT instr('日本語', '本')`, "2"},
		{"aggregate-empty", `SELECT quote(sum(x)) FROM (SELECT 1 AS x WHERE 0)`, "NULL"},
		{"aggregate-total-empty", `SELECT total(x) FROM (SELECT 1 AS x WHERE 0)`, "0.0"},
		{"group-concat-order", `SELECT group_concat(v, ',') FROM (SELECT 'a' AS v UNION ALL SELECT 'b')`, "a,b"},
		{"window-function", `SELECT sum(v) OVER (ORDER BY v) FROM (SELECT 2 AS v) LIMIT 1`, "2"},
		{"cte-recursive", `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<10) SELECT sum(i) FROM n`, "55"},
		{"collate-nocase", `SELECT 'ABC' = 'abc' COLLATE NOCASE`, "1"},
		{"typeof-blob", `SELECT typeof(x'00')`, "blob"},
		{"empty-blob-not-null", `SELECT typeof(zeroblob(0))`, "blob"},
		{"math-functions", `SELECT CAST(pow(2,10) AS INTEGER)`, "1024"},
		{"fts5-available", `SELECT sqlite_compileoption_used('SQLITE_ENABLE_FTS5')`, "1"},
	}
}
