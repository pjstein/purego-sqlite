package sqlite3

import "errors"

// Backup is an online copy from one open database to another, running
// incrementally so a large database can be copied without holding a read lock
// for the whole of it.
type Backup struct {
	b   uintptr
	dst *Conn
}

// BackupFrom starts a backup that copies the src connection's srcName database
// into this connection's dstName database. An empty name means "main".
//
// The destination must be a different connection than the source. Call
// [Backup.Step] until it reports done, then [Backup.Close].
func (c *Conn) BackupFrom(dstName string, src *Conn, srcName string) (*Backup, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if err := src.check(); err != nil {
		return nil, err
	}
	if c == src {
		return nil, errors.New("sqlite3: backup source and destination must be different connections")
	}
	if dstName == "" {
		dstName = "main"
	}
	if srcName == "" {
		srcName = "main"
	}
	b := lib.backupInit(c.db, dstName, src.db, srcName)
	if b == 0 {
		// sqlite3_backup_init reports failure on the destination connection.
		return nil, c.err(cErrcode(c.db), "backup_init")
	}
	return &Backup{b: b, dst: c}, nil
}

// BackupTo copies this connection's main database into dst's main database,
// running the copy to completion in one call.
func (c *Conn) BackupTo(dst *Conn) error {
	b, err := dst.BackupFrom("main", c, "main")
	if err != nil {
		return err
	}
	defer b.Close()
	for {
		done, err := b.Step(-1)
		if err != nil {
			return err
		}
		if done {
			return b.Close()
		}
	}
}

// Step copies up to pages pages, or all remaining pages when pages is
// negative. It reports done when the whole database has been copied.
func (b *Backup) Step(pages int) (done bool, err error) {
	if b == nil || b.b == 0 {
		return false, errors.New("sqlite3: backup is closed")
	}
	rc := Code(lib.backupStep(b.b, int32(pages)))
	switch rc {
	case OK:
		return false, nil
	case Done:
		return true, nil
	default:
		return false, b.dst.err(rc, "backup_step")
	}
}

// Remaining is the number of pages still to copy, as of the last Step.
func (b *Backup) Remaining() int {
	if b == nil || b.b == 0 {
		return 0
	}
	return int(lib.backupRemaining(b.b))
}

// PageCount is the total size of the source database in pages, as of the last
// Step.
func (b *Backup) PageCount() int {
	if b == nil || b.b == 0 {
		return 0
	}
	return int(lib.backupPagecount(b.b))
}

// Close finishes the backup, releasing its resources. It is safe to call more
// than once, and it reports any error the copy accumulated.
func (b *Backup) Close() error {
	if b == nil || b.b == 0 {
		return nil
	}
	h := b.b
	b.b = 0
	rc := Code(lib.backupFinish(h))
	if rc != OK {
		return b.dst.err(rc, "backup_finish")
	}
	return nil
}
