// Package cairn is an embedded relational database with a SQL interface,
// written from scratch on the Go standard library: B+tree storage, a
// write-ahead log with crash recovery, snapshot reads, and a planner that
// uses indexes and hash joins.
//
// Use it through database/sql:
//
//	db, err := sql.Open("cairn", "app.db")
//
// or directly with Open, which returns a *DB with Exec and Query.
package cairn

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/integral0909/cairn/internal/engine"
	"github.com/integral0909/cairn/internal/pager"
	"github.com/integral0909/cairn/internal/value"
)

// Options configure a database.
type Options struct {
	// NoSync skips fsync at commit. A crash can then lose the most recent
	// commits, but never corrupts the database.
	NoSync bool
	// CachePages bounds the page cache (4 KiB pages). Default 2048.
	CachePages int
}

// DB is an open database. It is safe for concurrent use.
type DB struct {
	eng  *engine.DB
	path string
}

// Memory is the path that opens a private in-memory database.
const Memory = ":memory:"

// Open opens (creating if needed) the database at path and its write-ahead
// log at path + "-wal".
func Open(path string, opts *Options) (*DB, error) {
	if opts == nil {
		opts = &Options{}
	}
	var dbf, walf pager.File
	if path == Memory {
		fs := pager.NewMemFS()
		dbf, walf = fs.Open("db"), fs.Open("wal")
	} else {
		var err error
		if dbf, err = pager.OpenFile(path); err != nil {
			return nil, err
		}
		if walf, err = pager.OpenFile(path + "-wal"); err != nil {
			dbf.Close()
			return nil, err
		}
	}
	p, err := pager.Open(dbf, walf, pager.Options{NoSync: opts.NoSync, CachePages: opts.CachePages})
	if err != nil {
		dbf.Close()
		walf.Close()
		return nil, err
	}
	eng, err := engine.Open(p)
	if err != nil {
		p.Close()
		return nil, err
	}
	return &DB{eng: eng, path: path}, nil
}

// Close checkpoints the log and closes the files.
func (db *DB) Close() error { return db.eng.Close() }

// Result reports what a statement changed.
type Result = engine.Result

// Exec runs statements (separated by semicolons) in their own session.
func (db *DB) Exec(query string, args ...any) (Result, error) {
	c := db.Conn()
	defer c.Close()
	return c.Exec(query, args...)
}

// Query runs a query in its own session.
func (db *DB) Query(query string, args ...any) (*Rows, error) {
	c := db.Conn()
	rows, err := c.Query(query, args...)
	if err != nil {
		c.Close()
		return nil, err
	}
	rows.onClose = c.Close
	return rows, nil
}

// Checkpoint copies the log into the database file (when no reader holds
// an old snapshot).
func (db *DB) Checkpoint() error { return db.eng.Pager().Checkpoint() }

// Stats describes storage.
type Stats struct {
	Pages     uint32
	LogFrames int
	Tables    int
}

func (db *DB) Stats() Stats {
	ps := db.eng.Pager().Stats()
	return Stats{Pages: ps.Pages, LogFrames: ps.LogFrames, Tables: len(db.Tables())}
}

// Tables lists table names.
func (db *DB) Tables() []string {
	c := db.Conn()
	defer c.Close()
	var names []string
	for _, t := range db.eng.Catalog().Tables() {
		names = append(names, t.Name)
	}
	return names
}

// Schema returns the CREATE statements for every table and index.
func (db *DB) Schema() []string {
	var out []string
	for _, t := range db.eng.Catalog().Tables() {
		out = append(out, engine.TableSQL(t))
		for _, ix := range t.Indexes {
			if !ix.Auto {
				out = append(out, engine.IndexSQL(t, ix))
			}
		}
	}
	return out
}

// Conn is a session. Statements on one Conn run in order and may share an
// explicit transaction (BEGIN ... COMMIT). Not safe for concurrent use.
type Conn struct {
	c   *engine.Conn
	ctx context.Context
}

func (db *DB) Conn() *Conn { return &Conn{c: db.eng.Conn(), ctx: context.Background()} }

// WithContext returns a Conn whose statements stop when ctx is done.
func (c *Conn) WithContext(ctx context.Context) *Conn { return &Conn{c: c.c, ctx: ctx} }

func (c *Conn) Close() { c.c.Close() }

func convertArgs(args []any) ([]value.Value, error) {
	out := make([]value.Value, len(args))
	for i, a := range args {
		v, err := value.FromGo(a)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		out[i] = v
	}
	return out, nil
}

func (c *Conn) Exec(query string, args ...any) (Result, error) {
	vals, err := convertArgs(args)
	if err != nil {
		return Result{}, err
	}
	return c.c.Exec(c.ctx, query, vals...)
}

func (c *Conn) Query(query string, args ...any) (*Rows, error) {
	vals, err := convertArgs(args)
	if err != nil {
		return nil, err
	}
	r, err := c.c.Query(c.ctx, query, vals...)
	if err != nil {
		return nil, err
	}
	return &Rows{r: r}, nil
}

// InTransaction reports whether BEGIN has been run without COMMIT or
// ROLLBACK.
func (c *Conn) InTransaction() bool { return c.c.InTransaction() }

// Rows iterates over a result.
type Rows struct {
	r       *engine.Rows
	row     []value.Value
	err     error
	onClose func()
	closed  bool
}

func (r *Rows) Columns() []string { return r.r.Columns() }

// Next advances to the next row.
func (r *Rows) Next() bool {
	if r.closed {
		return false
	}
	r.row, r.err = r.r.Next()
	if r.row == nil {
		r.Close()
		return false
	}
	return true
}

func (r *Rows) Err() error { return r.err }

// Values returns the current row as Go values (nil, int64, float64,
// string, []byte or bool).
func (r *Rows) Values() []any {
	out := make([]any, len(r.row))
	for i, v := range r.row {
		out[i] = v.Go()
	}
	return out
}

// Strings returns the current row as printed by the shell.
func (r *Rows) Strings() []string {
	out := make([]string, len(r.row))
	for i, v := range r.row {
		out[i] = v.String()
	}
	return out
}

// Scan copies the current row into pointers (*int64, *int, *float64,
// *string, *[]byte, *bool or *any).
func (r *Rows) Scan(dest ...any) error {
	if len(dest) != len(r.row) {
		return fmt.Errorf("Scan expects %d destinations, got %d", len(r.row), len(dest))
	}
	for i, d := range dest {
		if err := scanValue(r.row[i], d); err != nil {
			return fmt.Errorf("column %s: %w", r.r.Columns()[i], err)
		}
	}
	return nil
}

func scanValue(v value.Value, dest any) error {
	switch d := dest.(type) {
	case *any:
		*d = v.Go()
		return nil
	case *string:
		if v.IsNull() {
			return errors.New("cannot scan NULL into a string")
		}
		*d = v.String()
		return nil
	}
	if v.IsNull() {
		return errors.New("cannot scan NULL; use *any")
	}
	switch d := dest.(type) {
	case *int64:
		if v.K != value.Int {
			return fmt.Errorf("cannot scan %s into int64", v.K)
		}
		*d = v.I
	case *int:
		if v.K != value.Int {
			return fmt.Errorf("cannot scan %s into int", v.K)
		}
		*d = int(v.I)
	case *float64:
		if !v.IsNumeric() {
			return fmt.Errorf("cannot scan %s into float64", v.K)
		}
		*d = v.Float()
	case *bool:
		if v.K != value.Bool && v.K != value.Int {
			return fmt.Errorf("cannot scan %s into bool", v.K)
		}
		*d = v.I != 0
	case *[]byte:
		if v.K != value.Blob {
			return fmt.Errorf("cannot scan %s into []byte", v.K)
		}
		*d = append([]byte(nil), v.B...)
	default:
		return fmt.Errorf("unsupported Scan destination %T", dest)
	}
	return nil
}

func (r *Rows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	r.r.Close()
	if r.onClose != nil {
		r.onClose()
	}
}

// IsMemory reports whether the database lives only in memory.
func (db *DB) IsMemory() bool { return strings.EqualFold(db.path, Memory) }

// Check verifies the database's structure and that indexes match their
// tables, returning any problems found.
func (db *DB) Check() ([]string, error) { return db.eng.Check() }
