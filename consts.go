package sqlite3

// OpenFlag values for [OpenFlags]. These mirror SQLITE_OPEN_*.
type OpenFlag int32

const (
	OpenReadOnly     OpenFlag = 0x00000001
	OpenReadWrite    OpenFlag = 0x00000002
	OpenCreate       OpenFlag = 0x00000004
	OpenURI          OpenFlag = 0x00000040
	OpenMemory       OpenFlag = 0x00000080
	OpenNoMutex      OpenFlag = 0x00008000
	OpenFullMutex    OpenFlag = 0x00010000
	OpenSharedCache  OpenFlag = 0x00020000
	OpenPrivateCache OpenFlag = 0x00040000
	OpenNoFollow     OpenFlag = 0x01000000
	OpenExResCode    OpenFlag = 0x02000000

	// OpenDefault is what [Open] uses: create if absent, read and write, and
	// report extended result codes so a constraint violation says which kind.
	OpenDefault = OpenReadWrite | OpenCreate | OpenExResCode
)

// ColumnType is the storage class of a result column, mirroring SQLITE_*
// datatype codes. It is the type of the value actually stored, not the
// column's declared type.
type ColumnType int32

const (
	TypeInteger ColumnType = 1
	TypeFloat   ColumnType = 2
	TypeText    ColumnType = 3
	TypeBlob    ColumnType = 4
	TypeNull    ColumnType = 5
)

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

// Statement preparation flags, mirroring SQLITE_PREPARE_*.
type PrepareFlag uint32

const (
	// PreparePersistent tells SQLite the statement will be reused many times,
	// so it should not be aggressively reclaimed from the statement cache.
	PreparePersistent PrepareFlag = 0x01
	PrepareNoVTab     PrepareFlag = 0x04
)

// Destructor sentinels for bind calls. Only transient is used by this package:
// SQLite copies the bytes before the call returns, which is what makes it safe
// to hand it a pointer into Go memory.
const (
	sqliteStatic    = uintptr(0)
	sqliteTransient = ^uintptr(0) // (sqlite3_destructor_type)-1
)
