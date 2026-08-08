package bakeoff

import (
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"

	_ "github.com/ncruces/go-sqlite3/driver" // registers "sqlite3"
	_ "modernc.org/sqlite"                   // registers "sqlite"
)

// SQLDriver drives a registered database/sql driver.
//
// modernc.org/sqlite has no supported API other than database/sql, so this is
// the only way to measure it, and it is how anyone would actually use it.
// ncruces is measured both natively and through here, which is what makes the
// modernc number readable: the gap between the two ncruces rows is the cost of
// database/sql itself, and it applies to the modernc row too.
type SQLDriver struct {
	driverName string
	label      string
}

func (d SQLDriver) Name() string { return d.label }

func (d SQLDriver) Open(path string) (DB, error) {
	db, err := sql.Open(d.driverName, path)
	if err != nil {
		return nil, err
	}
	// One connection, held open. The workload gives every worker its own DB
	// handle, so a pool would only add a layer that the direct drivers do not
	// have -- and a recycled connection would silently lose the per-connection
	// pragmas below.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	for _, p := range Pragmas {
		rows, err := db.Query(p)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		// The rows must be drained, not merely closed. A pragma that returns a
		// value is a statement that has not run until something steps it;
		// closing the result set first abandons it. Measured the hard way:
		// with rows.Close() alone, PRAGMA journal_mode=WAL silently did
		// nothing on one driver and an entire benchmark ran in rollback-journal
		// mode while reporting success.
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	wrapped := &sqlDB{db: db}
	if err := verifyPragmas(wrapped); err != nil {
		db.Close()
		return nil, err
	}
	return wrapped, nil
}

type sqlDB struct{ db *sql.DB }

func (d *sqlDB) Close() error { return d.db.Close() }

func (d *sqlDB) SQLiteVersion() (string, error) {
	out, err := d.QueryText(`SELECT sqlite_version()`)
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", errNoRow
	}
	return out[0], nil
}

func (d *sqlDB) Exec(sqlText string, args ...any) error {
	// A statement that returns rows must go through Query for some drivers,
	// and multi-statement text must go through Exec. Split on the first
	// keyword, which is enough for the statements this harness runs.
	if returnsRows(sqlText) {
		rows, err := d.db.Query(sqlText, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		return rows.Close()
	}
	_, err := d.db.Exec(sqlText, args...)
	return err
}

func returnsRows(s string) bool {
	t := strings.TrimSpace(s)
	for len(t) > 0 && (t[0] == '(' || t[0] == ';') {
		t = strings.TrimSpace(t[1:])
	}
	upper := strings.ToUpper(t)
	return strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") || strings.HasPrefix(upper, "PRAGMA")
}

func (d *sqlDB) QueryText(sqlText string, args ...any) ([]string, error) {
	rows, err := d.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, renderSQLText(v))
	}
	return out, rows.Err()
}

// renderSQLText turns the Go value a database/sql driver produced back into
// the text SQLite itself would have produced for the same value.
//
// This exists so the expression corpus compares SQL semantics rather than Go
// type mapping. The only rule that is not obvious: SQLite always renders a
// REAL with a decimal point or an exponent, so 0.0 is "0.0" where Go's
// shortest formatting gives "0".
func renderSQLText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return strconv.FormatFloat(x, 'g', -1, 64)
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	default:
		return fmt.Sprint(x)
	}
}

func (d *sqlDB) RoundTrip(key, text string, blob []byte) (string, []byte, error) {
	if _, err := d.db.Exec(roundTripInsert, key, text, blob); err != nil {
		return "", nil, err
	}
	var gotText string
	var gotBlob []byte
	err := d.db.QueryRow(roundTripSelect, key).Scan(&gotText, &gotBlob)
	if err != nil {
		return "", nil, err
	}
	return gotText, gotBlob, nil
}

func (d *sqlDB) NewAppender() (Appender, error) {
	st, err := d.db.Prepare(AppendSQL)
	if err != nil {
		return nil, err
	}
	return &sqlAppender{st: st}, nil
}

type sqlAppender struct{ st *sql.Stmt }

func (a *sqlAppender) Append(stream, kind string, ts int64, payload []byte) error {
	_, err := a.st.Exec(stream, kind, ts, payload)
	return err
}

func (a *sqlAppender) Close() error { return a.st.Close() }

func (d *sqlDB) NewTailer() (Tailer, error) {
	st, err := d.db.Prepare(TailSQL)
	if err != nil {
		return nil, err
	}
	return &sqlTailer{st: st}, nil
}

type sqlTailer struct{ st *sql.Stmt }

func (t *sqlTailer) Poll(after int64) (int64, int, error) {
	rows, err := t.st.Query(after)
	if err != nil {
		return after, 0, err
	}
	defer rows.Close()

	last, n := after, 0
	var seq, ts int64
	var kind string
	var payload []byte
	for rows.Next() {
		if err := rows.Scan(&seq, &kind, &ts, &payload); err != nil {
			return last, n, err
		}
		last = seq
		n++
	}
	return last, n, rows.Err()
}

func (t *sqlTailer) Close() error { return t.st.Close() }
