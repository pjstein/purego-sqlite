//go:build !(cgo && darwin && arm64)

package cgoref

import "errors"

// Available reports that this build has no cgo reference, so the purego-tax
// benchmarks that need one must be skipped rather than silently reporting a
// comparison they did not make.
const Available = false

var errUnavailable = errors.New("cgoref: not built (needs cgo on darwin/arm64)")

type Conn struct{}
type Stmt struct{}

func Open(string) (*Conn, error)            { return nil, errUnavailable }
func (*Conn) Close() error                  { return errUnavailable }
func (*Conn) Exec(string) error             { return errUnavailable }
func (*Conn) Prepare(string) (*Stmt, error) { return nil, errUnavailable }
func (*Stmt) Close() error                  { return errUnavailable }
func (*Stmt) BindInt64(int, int64) error    { return errUnavailable }
func (*Stmt) BindText(int, string) error    { return errUnavailable }
func (*Stmt) BindBlob(int, []byte) error    { return errUnavailable }
func (*Stmt) Step() (bool, error)           { return false, errUnavailable }
func (*Stmt) Reset() error                  { return errUnavailable }
func (*Stmt) ColumnInt64(int) int64         { return 0 }
func (*Stmt) ColumnText(int) string         { return "" }
func (*Stmt) ColumnBlob(int) []byte         { return nil }
func NullCall() int                         { return 0 }
func Version() string                       { return "" }
