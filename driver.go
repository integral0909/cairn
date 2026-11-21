package cairn

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"

	"github.com/integral0909/cairn/internal/value"
)

func init() { sql.Register("cairn", &Driver{}) }

// Driver implements database/sql/driver. The data source name is a path,
// optionally followed by ?nosync=1. Connections to the same path share one
// database, so they see each other's commits and readers get snapshots.
type Driver struct{}

var (
	sharedMu sync.Mutex
	shared   = map[string]*sharedDB{}
)

type sharedDB struct {
	db   *DB
	refs int
}

func (d *Driver) Open(dsn string) (driver.Conn, error) {
	path, opts := parseDSN(dsn)
	sharedMu.Lock()
	defer sharedMu.Unlock()
	key := path
	if path == Memory {
		key = dsn // each :memory: DSN is its own database while it is open
	}
	s := shared[key]
	if s == nil {
		db, err := Open(path, opts)
		if err != nil {
			return nil, err
		}
		s = &sharedDB{db: db}
		shared[key] = s
	}
	s.refs++
	return &conn{key: key, s: s, c: s.db.Conn()}, nil
}

func parseDSN(dsn string) (string, *Options) {
	opts := &Options{}
	path, query, _ := strings.Cut(dsn, "?")
	if q, err := url.ParseQuery(query); err == nil {
		opts.NoSync = q.Get("nosync") == "1" || q.Get("nosync") == "true"
	}
	if path == "" {
		path = Memory
	}
	return path, opts
}

type conn struct {
	key string
	s   *sharedDB
	c   *Conn
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{c: c, query: query}, nil
}

func (c *conn) Close() error {
	c.c.Close()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	c.s.refs--
	if c.s.refs == 0 {
		delete(shared, c.key)
		return c.s.db.Close()
	}
	return nil
}

func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

// BeginTx starts a transaction. Transactions take the database's single
// writer lock at BEGIN (read-only ones too); for plain reads, query outside
// a transaction to get a snapshot that never blocks.
func (c *conn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if _, err := c.c.WithContext(ctx).Exec("BEGIN"); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}

func namedToValues(args []driver.NamedValue) ([]any, error) {
	out := make([]any, len(args))
	for i, a := range args {
		if a.Name != "" {
			return nil, errors.New("cairn: named parameters are not supported; use ? or $n")
		}
		out[i] = a.Value
	}
	return out, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals, err := namedToValues(args)
	if err != nil {
		return nil, err
	}
	res, err := c.c.WithContext(ctx).Exec(query, vals...)
	if err != nil {
		return nil, err
	}
	return result{res}, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals, err := namedToValues(args)
	if err != nil {
		return nil, err
	}
	r, err := c.c.WithContext(ctx).Query(query, vals...)
	if err != nil {
		return nil, err
	}
	return &rows{r: r}, nil
}

func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	_, err := value.FromGo(nv.Value)
	return err
}

func (c *conn) Ping(ctx context.Context) error { return nil }

type result struct{ r Result }

func (r result) LastInsertId() (int64, error) { return r.r.LastInsertID, nil }
func (r result) RowsAffected() (int64, error) { return r.r.RowsAffected, nil }

type stmt struct {
	c     *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), toNamed(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), toNamed(args))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func toNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

type tx struct{ c *conn }

func (t *tx) Commit() error {
	_, err := t.c.c.Exec("COMMIT")
	return err
}

func (t *tx) Rollback() error {
	if !t.c.c.InTransaction() {
		return nil
	}
	_, err := t.c.c.Exec("ROLLBACK")
	return err
}

type rows struct{ r *Rows }

func (r *rows) Columns() []string { return r.r.Columns() }
func (r *rows) Close() error      { r.r.Close(); return nil }

func (r *rows) Next(dest []driver.Value) error {
	if !r.r.Next() {
		if err := r.r.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	for i, v := range r.r.row {
		dest[i] = v.Go()
	}
	return nil
}
