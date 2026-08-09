package sqlite3

// OpenFlag values for [OpenFlags]. These mirror SQLITE_OPEN_*.
type OpenFlag int32

// The open flags, each the value of the SQLITE_OPEN_* constant it is named
// for. Combine them with | and pass them to [OpenFlags].
//
// OpenNoMutex and OpenFullMutex select a connection's threading mode, and both
// are inert here: the shipped library is compiled SQLITE_THREADSAFE=2, in which
// SQLite honours neither. The contract is the one described in the package
// documentation — one goroutine per connection at a time — whatever is passed.
const (
	// OpenReadOnly opens an existing database for reading only. Writes fail
	// with [ReadOnly], and the database is not created if it is absent.
	OpenReadOnly OpenFlag = 0x00000001
	// OpenReadWrite opens for reading and writing, falling back to read-only
	// when the file permissions do not allow writing.
	OpenReadWrite OpenFlag = 0x00000002
	// OpenCreate creates the database if it does not exist. Meaningful only
	// alongside OpenReadWrite.
	OpenCreate OpenFlag = 0x00000004
	// OpenURI interprets the filename as a URI, which is what enables
	// "file:name?mode=memory&cache=shared" and the other query parameters.
	OpenURI OpenFlag = 0x00000040
	// OpenMemory opens a private in-memory database, ignoring the filename.
	OpenMemory OpenFlag = 0x00000080
	// OpenNoMutex asks for multi-thread mode. Inert here; see above.
	OpenNoMutex OpenFlag = 0x00008000
	// OpenFullMutex asks for serialized mode. Inert here; see above.
	OpenFullMutex OpenFlag = 0x00010000
	// OpenSharedCache enables shared-cache mode, which upstream discourages
	// for new code and which WAL mode makes largely unnecessary.
	OpenSharedCache OpenFlag = 0x00020000
	// OpenPrivateCache disables shared-cache mode even where it is the default.
	OpenPrivateCache OpenFlag = 0x00040000
	// OpenNoFollow refuses to open the database if the path is a symbolic link.
	OpenNoFollow OpenFlag = 0x01000000
	// OpenExResCode makes the connection report extended result codes, so a
	// constraint failure says which constraint. Part of [OpenDefault].
	OpenExResCode OpenFlag = 0x02000000

	// OpenDefault is what [Open] uses: create if absent, read and write, and
	// report extended result codes so a constraint violation says which kind.
	OpenDefault = OpenReadWrite | OpenCreate | OpenExResCode
)

// ColumnType is the storage class of a result column, mirroring SQLITE_*
// datatype codes. It is the type of the value actually stored, not the
// column's declared type.
type ColumnType int32

// The five storage classes SQLite has. A column's [ColumnType] is decided per
// row by the value stored in it, so the same column can report different types
// on different rows.
const (
	TypeInteger ColumnType = 1 // signed integer, 1 to 8 bytes as stored
	TypeFloat   ColumnType = 2 // IEEE 754 double
	TypeText    ColumnType = 3 // text in the database encoding, UTF-8 here
	TypeBlob    ColumnType = 4 // bytes, stored and returned exactly as given
	TypeNull    ColumnType = 5 // SQL NULL
)

// String returns SQLite's own name for the storage class — "INTEGER", "FLOAT",
// "TEXT", "BLOB", "NULL" — or "UNKNOWN" for a value that is none of them.
func (t ColumnType) String() string {
	switch t {
	case TypeInteger:
		return "INTEGER"
	case TypeFloat:
		return "FLOAT"
	case TypeText:
		return "TEXT"
	case TypeBlob:
		return "BLOB"
	case TypeNull:
		return "NULL"
	}
	return "UNKNOWN"
}

// CheckpointMode selects how hard a WAL checkpoint tries, mirroring
// SQLITE_CHECKPOINT_*.
type CheckpointMode int32

const (
	// CheckpointPassive copies what it can without blocking anyone.
	CheckpointPassive CheckpointMode = 0
	// CheckpointFull waits for writers, then copies the whole WAL.
	CheckpointFull CheckpointMode = 1
	// CheckpointRestart is CheckpointFull, then waits for readers so the next
	// writer starts the WAL from its beginning.
	CheckpointRestart CheckpointMode = 2
	// CheckpointTruncate is CheckpointRestart, then truncates the WAL file to
	// zero bytes.
	CheckpointTruncate CheckpointMode = 3
)

// PrepareFlag is a statement preparation flag for [Conn.PrepareFlags],
// mirroring SQLITE_PREPARE_*.
type PrepareFlag uint32

const (
	// PreparePersistent tells SQLite the statement will be reused many times,
	// so it should not be aggressively reclaimed from the statement cache.
	PreparePersistent PrepareFlag = 0x01
	// PrepareNoVTab refuses to prepare the statement if it would use a virtual
	// table. Useful as a guard where a virtual table would mean the schema is
	// not the one the caller expects.
	PrepareNoVTab PrepareFlag = 0x04
)

// Destructor sentinels for bind calls. Only transient is used by this package:
// SQLite copies the bytes before the call returns, which is what makes it safe
// to hand it a pointer into Go memory.
const (
	sqliteStatic    = uintptr(0)
	sqliteTransient = ^uintptr(0) // (sqlite3_destructor_type)-1
)
