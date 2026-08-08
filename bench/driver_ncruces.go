package bakeoff

import (
	"errors"

	ncruces "github.com/ncruces/go-sqlite3"
)

var errNoRow = errors.New("bakeoff: query returned no row")

// NcrucesDriver is ncruces/go-sqlite3 through its direct API -- the shape
// closest to this repository's binding, and the fairest comparison of the two.
type NcrucesDriver struct{}

func (NcrucesDriver) Name() string { return "ncruces/go-sqlite3 (native)" }

func (NcrucesDriver) Open(path string) (DB, error) {
	c, err := ncruces.Open(path)
	if err != nil {
		return nil, err
	}
	for _, p := range Pragmas {
		if err := c.Exec(p); err != nil {
			c.Close()
			return nil, err
		}
	}
	db := &ncrucesDB{c: c}
	if err := verifyPragmas(db); err != nil {
		c.Close()
		return nil, err
	}
	return db, nil
}

type ncrucesDB struct{ c *ncruces.Conn }

func (d *ncrucesDB) Close() error { return d.c.Close() }

func (d *ncrucesDB) SQLiteVersion() (string, error) {
	out, err := d.QueryText(`SELECT sqlite_version()`)
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", errNoRow
	}
	return out[0], nil
}

func (d *ncrucesDB) Exec(sql string, args ...any) error {
	if len(args) == 0 {
		return d.c.Exec(sql)
	}
	st, _, err := d.c.Prepare(sql)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := bindNcruces(st, args); err != nil {
		return err
	}
	return st.Exec()
}

func (d *ncrucesDB) QueryText(sql string, args ...any) ([]string, error) {
	st, _, err := d.c.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if err := bindNcruces(st, args); err != nil {
		return nil, err
	}
	var out []string
	for st.Step() {
		out = append(out, st.ColumnText(0))
	}
	return out, st.Err()
}

func (d *ncrucesDB) RoundTrip(key, text string, blob []byte) (string, []byte, error) {
	if err := d.Exec(roundTripInsert, key, text, blob); err != nil {
		return "", nil, err
	}
	st, _, err := d.c.Prepare(roundTripSelect)
	if err != nil {
		return "", nil, err
	}
	defer st.Close()
	if err := st.BindText(1, key); err != nil {
		return "", nil, err
	}
	if !st.Step() {
		if err := st.Err(); err != nil {
			return "", nil, err
		}
		return "", nil, errNoRow
	}
	gotText := st.ColumnText(0)
	gotBlob := st.ColumnBlob(1, nil)
	// ColumnBlob returns nil for both NULL and a zero-length blob when given a
	// nil buffer; distinguish them the way the corpus needs.
	if gotBlob == nil && st.ColumnType(1) == ncruces.BLOB {
		gotBlob = []byte{}
	}
	return gotText, gotBlob, st.Err()
}

func bindNcruces(st *ncruces.Stmt, args []any) error {
	for i, a := range args {
		var err error
		switch v := a.(type) {
		case nil:
			err = st.BindNull(i + 1)
		case int:
			err = st.BindInt64(i+1, int64(v))
		case int64:
			err = st.BindInt64(i+1, v)
		case float64:
			err = st.BindFloat(i+1, v)
		case string:
			err = st.BindText(i+1, v)
		case []byte:
			if v == nil {
				err = st.BindNull(i + 1)
			} else {
				err = st.BindBlob(i+1, v)
			}
		default:
			return errors.New("bakeoff: unsupported bind type")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *ncrucesDB) NewAppender() (Appender, error) {
	st, _, err := d.c.PrepareFlags(AppendSQL, ncruces.PREPARE_PERSISTENT)
	if err != nil {
		return nil, err
	}
	return &ncrucesAppender{st: st}, nil
}

type ncrucesAppender struct{ st *ncruces.Stmt }

func (a *ncrucesAppender) Append(stream, kind string, ts int64, payload []byte) error {
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
	return a.st.Exec() // steps to completion and resets
}

func (a *ncrucesAppender) Close() error { return a.st.Close() }

func (d *ncrucesDB) NewTailer() (Tailer, error) {
	st, _, err := d.c.PrepareFlags(TailSQL, ncruces.PREPARE_PERSISTENT)
	if err != nil {
		return nil, err
	}
	return &ncrucesTailer{st: st}, nil
}

type ncrucesTailer struct{ st *ncruces.Stmt }

func (t *ncrucesTailer) Poll(after int64) (int64, int, error) {
	if err := t.st.BindInt64(1, after); err != nil {
		return after, 0, err
	}
	last, rows := after, 0
	for t.st.Step() {
		last = t.st.ColumnInt64(0)
		_ = t.st.ColumnText(1)
		_ = t.st.ColumnInt64(2)
		_ = t.st.ColumnBlob(3, nil)
		rows++
	}
	if err := t.st.Err(); err != nil {
		t.st.Reset()
		return last, rows, err
	}
	return last, rows, t.st.Reset()
}

func (t *ncrucesTailer) Close() error { return t.st.Close() }
