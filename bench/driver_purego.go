package bakeoff

import (
	"github.com/pjstein/purego-sqlite/sqlite3"
)

// PuregoDriver is this repository's binding: upstream SQLite compiled here,
// called through purego.
type PuregoDriver struct{}

func (PuregoDriver) Name() string { return "purego-sqlite (native)" }

func (PuregoDriver) Open(path string) (DB, error) {
	c, err := sqlite3.Open(path)
	if err != nil {
		return nil, err
	}
	db := &puregoDB{c: c}
	for _, p := range Pragmas {
		if err := c.Exec(p); err != nil {
			c.Close()
			return nil, err
		}
	}
	if err := verifyPragmas(db); err != nil {
		c.Close()
		return nil, err
	}
	return db, nil
}

type puregoDB struct{ c *sqlite3.Conn }

func (d *puregoDB) SQLiteVersion() (string, error) { return sqlite3.Version() }
func (d *puregoDB) Close() error                   { return d.c.Close() }

func (d *puregoDB) Exec(sql string, args ...any) error { return d.c.Exec(sql, args...) }

func (d *puregoDB) QueryText(sql string, args ...any) ([]string, error) {
	st, err := d.c.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if err := st.Bind(args...); err != nil {
		return nil, err
	}
	var out []string
	for {
		row, err := st.Step()
		if err != nil {
			return nil, err
		}
		if !row {
			return out, nil
		}
		out = append(out, st.ColumnText(0))
	}
}

func (d *puregoDB) RoundTrip(key, text string, blob []byte) (string, []byte, error) {
	if err := d.c.Exec(roundTripInsert, key, text, blob); err != nil {
		return "", nil, err
	}
	st, err := d.c.Prepare(roundTripSelect)
	if err != nil {
		return "", nil, err
	}
	defer st.Close()
	if err := st.Bind(key); err != nil {
		return "", nil, err
	}
	row, err := st.Step()
	if err != nil {
		return "", nil, err
	}
	if !row {
		return "", nil, errNoRow
	}
	return st.ColumnText(0), st.ColumnBlob(1), nil
}

func (d *puregoDB) NewAppender() (Appender, error) {
	st, err := d.c.PrepareFlags(AppendSQL, sqlite3.PreparePersistent)
	if err != nil {
		return nil, err
	}
	return &puregoAppender{st: st}, nil
}

type puregoAppender struct{ st *sqlite3.Stmt }

func (a *puregoAppender) Append(stream, kind string, ts int64, payload []byte) error {
	if err := a.st.BindText(1, stream); err != nil {
		return err
	}
	if err := a.st.BindText(2, kind); err != nil {
		return err
	}
	if err := a.st.BindInt64(3, ts); err != nil {
		return err
	}
	if err := a.st.BindBlob(4, payload); err != nil {
		return err
	}
	return a.st.Run()
}

func (a *puregoAppender) Close() error { return a.st.Close() }

func (d *puregoDB) NewTailer() (Tailer, error) {
	st, err := d.c.PrepareFlags(TailSQL, sqlite3.PreparePersistent)
	if err != nil {
		return nil, err
	}
	return &puregoTailer{st: st}, nil
}

type puregoTailer struct{ st *sqlite3.Stmt }

func (t *puregoTailer) Poll(after int64) (int64, int, error) {
	if err := t.st.BindInt64(1, after); err != nil {
		return after, 0, err
	}
	last, rows := after, 0
	for {
		row, err := t.st.Step()
		if err != nil {
			t.st.Reset()
			return last, rows, err
		}
		if !row {
			break
		}
		last = t.st.ColumnInt64(0)
		_ = t.st.ColumnText(1)
		_ = t.st.ColumnInt64(2)
		_ = t.st.ColumnBlob(3)
		rows++
	}
	return last, rows, t.st.Reset()
}

func (t *puregoTailer) Close() error { return t.st.Close() }
