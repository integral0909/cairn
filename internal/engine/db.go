package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/integral0909/cairn/internal/btree"
	"github.com/integral0909/cairn/internal/pager"
	"github.com/integral0909/cairn/internal/sql"
	"github.com/integral0909/cairn/internal/value"
)

// DB is an open database.
type DB struct {
	p   *pager.Pager
	mu  sync.Mutex // guards cat
	cat *Catalog   // the committed schema
	// stmtCache keeps parsed statements, keyed by source text.
	stmtCache sync.Map
}

// Open opens a database over a pager, creating the schema tree in a new
// database.
func Open(p *pager.Pager) (*DB, error) {
	db := &DB{p: p}
	tx := p.BeginWrite()
	if tx.PageCount() == 1 {
		root, err := btree.Create(tx)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if root != schemaRoot {
			tx.Rollback()
			return nil, fmt.Errorf("unexpected schema root %d", root)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	} else {
		tx.Rollback()
	}
	r := p.BeginRead()
	defer r.Close()
	cat, err := loadCatalog(r)
	if err != nil {
		return nil, err
	}
	db.cat = cat
	return db, nil
}

func (db *DB) Close() error { return db.p.Close() }

// Pager exposes storage statistics and checkpoints.
func (db *DB) Pager() *pager.Pager { return db.p }

// Catalog returns the committed schema.
func (db *DB) Catalog() *Catalog { return db.catalog() }

func (db *DB) catalog() *Catalog {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.cat
}

// Conn is a session: it runs statements one at a time and may hold an
// explicit transaction. Conns are not safe for concurrent use; open one
// per goroutine.
type Conn struct {
	db  *DB
	tx  *pager.WriteTx // explicit transaction, if any
	cat *Catalog       // the schema as this transaction sees it
	// failed is set when a statement in an explicit transaction failed in
	// a way that could not be undone; only ROLLBACK is accepted then.
	failed error
}

func (db *DB) Conn() *Conn { return &Conn{db: db} }

// InTransaction reports whether an explicit transaction is open.
func (c *Conn) InTransaction() bool { return c.tx != nil }

// Result describes a statement that changed data.
type Result struct {
	RowsAffected int64
	LastInsertID int64
}

var ErrNoTransaction = errors.New("no transaction is open")

func (db *DB) parse(src string) ([]sql.Stmt, error) {
	if v, ok := db.stmtCache.Load(src); ok {
		return v.([]sql.Stmt), nil
	}
	stmts, err := sql.Parse(src)
	if err != nil {
		return nil, err
	}
	db.stmtCache.Store(src, stmts)
	return stmts, nil
}

// Exec runs one or more statements, discarding any rows. With several
// statements, args go to the last one.
func (c *Conn) Exec(ctx context.Context, src string, args ...value.Value) (Result, error) {
	stmts, err := c.db.parse(src)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for i, s := range stmts {
		a := args
		if i < len(stmts)-1 {
			a = nil
		}
		rows, r, err := c.run(ctx, s, a)
		if err != nil {
			return res, err
		}
		if rows != nil {
			for {
				row, err := rows.Next()
				if err != nil {
					rows.Close()
					return res, err
				}
				if row == nil {
					break
				}
			}
			rows.Close()
		}
		res = r
	}
	return res, nil
}

// Query runs a single statement that returns rows (SELECT, EXPLAIN, or a
// change with RETURNING). Close the rows when done.
func (c *Conn) Query(ctx context.Context, src string, args ...value.Value) (*Rows, error) {
	stmts, err := c.db.parse(src)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("Query runs exactly one statement, got %d", len(stmts))
	}
	rows, res, err := c.run(ctx, stmts[0], args)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = &Rows{affected: &res}
	}
	return rows, nil
}

// Rows streams a result.
type Rows struct {
	cols     []string
	it       iter
	buffered [][]value.Value
	i        int
	release  func()
	done     bool
	affected *Result
}

func (r *Rows) Columns() []string { return r.cols }

// Result is set for statements that changed data without RETURNING.
func (r *Rows) Result() *Result { return r.affected }

// Next returns the next row, or nil at the end.
func (r *Rows) Next() ([]value.Value, error) {
	if r.done {
		return nil, nil
	}
	if r.it == nil {
		if r.i >= len(r.buffered) {
			r.Close()
			return nil, nil
		}
		r.i++
		return r.buffered[r.i-1], nil
	}
	row, err := r.it.next()
	if err != nil || row == nil {
		r.Close()
	}
	return row, err
}

func (r *Rows) Close() {
	if r.done {
		return
	}
	r.done = true
	if r.it != nil {
		r.it.close()
	}
	if r.release != nil {
		r.release()
	}
}

// run executes one statement in the right kind of transaction.
func (c *Conn) run(ctx context.Context, s sql.Stmt, args []value.Value) (*Rows, Result, error) {
	switch s.(type) {
	case *sql.Begin:
		if c.tx != nil {
			return nil, Result{}, errors.New("a transaction is already open")
		}
		c.tx = c.db.p.BeginWrite()
		c.cat = c.db.catalog()
		c.failed = nil
		return nil, Result{}, nil
	case *sql.Commit:
		if c.tx == nil {
			return nil, Result{}, ErrNoTransaction
		}
		tx := c.tx
		c.tx = nil
		if c.failed != nil {
			tx.Rollback()
			return nil, Result{}, fmt.Errorf("transaction rolled back after an earlier error: %w", c.failed)
		}
		c.db.mu.Lock()
		err := tx.Commit()
		if err == nil {
			c.db.cat = c.cat
		}
		c.db.mu.Unlock()
		return nil, Result{}, err
	case *sql.Rollback:
		if c.tx == nil {
			return nil, Result{}, ErrNoTransaction
		}
		c.tx.Rollback()
		c.tx = nil
		return nil, Result{}, nil
	}
	if c.failed != nil {
		return nil, Result{}, fmt.Errorf("the transaction has failed; ROLLBACK it: %w", c.failed)
	}
	cancel := func() error { return ctx.Err() }

	explain, isExplain := s.(*sql.Explain)
	if isExplain {
		s = explain.Stmt
	}
	sel, isSelect := s.(*sql.Select)
	if isSelect || isExplain {
		// Read-only work: inside a transaction it sees the transaction's
		// changes; otherwise it reads a snapshot without blocking writers.
		var r btree.Reader
		var cat *Catalog
		release := func() {}
		if c.tx != nil {
			r, cat = c.tx, c.cat
		} else {
			rt := c.db.p.BeginRead()
			r, cat, release = rt, c.db.catalog(), rt.Close
		}
		if isExplain {
			defer release()
			lines, err := explainStmt(cat, s)
			if err != nil {
				return nil, Result{}, err
			}
			rows := make([][]value.Value, len(lines))
			for i, l := range lines {
				rows[i] = []value.Value{value.NewText(l)}
			}
			return &Rows{cols: []string{"plan"}, buffered: rows}, Result{}, nil
		}
		comp := &compiler{cat: cat}
		p, err := comp.planSelect(sel)
		if err != nil {
			release()
			return nil, Result{}, err
		}
		if err := checkArgs(comp.maxParam, args); err != nil {
			release()
			return nil, Result{}, err
		}
		ectx := &execCtx{r: r, params: args, cancel: cancel}
		it, err := p.open(ectx, nil)
		if err != nil {
			release()
			return nil, Result{}, err
		}
		return &Rows{cols: p.columns(), it: it, release: release}, Result{}, nil
	}

	// Changes run in the open transaction, or in one of their own.
	tx, cat := c.tx, c.cat
	auto := tx == nil
	if auto {
		tx = c.db.p.BeginWrite()
		cat = c.db.catalog()
	}
	tx.BeginStatement()
	w := &writer{tx: tx, cat: cat, ctx: &execCtx{r: tx, params: args, cancel: cancel}}
	out, res, err := w.exec(s)
	if err != nil {
		tx.RollbackStatement()
		if auto {
			tx.Rollback()
		}
		return nil, Result{}, err
	}
	tx.EndStatement()
	if auto {
		c.db.mu.Lock()
		err := tx.Commit()
		if err == nil {
			c.db.cat = w.cat
		}
		c.db.mu.Unlock()
		if err != nil {
			return nil, Result{}, err
		}
	} else {
		c.cat = w.cat
	}
	if out != nil {
		return out, res, nil
	}
	return nil, res, nil
}

func checkArgs(want int, args []value.Value) error {
	if len(args) < want {
		return fmt.Errorf("the statement needs %d parameters, %d given", want, len(args))
	}
	return nil
}

// Close rolls back any open transaction.
func (c *Conn) Close() {
	if c.tx != nil {
		c.tx.Rollback()
		c.tx = nil
	}
}

func explainStmt(cat *Catalog, s sql.Stmt) ([]string, error) {
	comp := &compiler{cat: cat}
	switch st := s.(type) {
	case *sql.Select:
		p, err := comp.planSelect(st)
		if err != nil {
			return nil, err
		}
		return explainPlan(p), nil
	case *sql.Update:
		p, err := comp.dmlScan(st.Table, st.Where)
		if err != nil {
			return nil, err
		}
		return append([]string{"Update " + st.Table}, indent(explainPlan(p))...), nil
	case *sql.Delete:
		p, err := comp.dmlScan(st.Table, st.Where)
		if err != nil {
			return nil, err
		}
		return append([]string{"Delete from " + st.Table}, indent(explainPlan(p))...), nil
	case *sql.Insert:
		lines := []string{"Insert into " + st.Table}
		if st.Select != nil {
			p, err := comp.planSelect(st.Select)
			if err != nil {
				return nil, err
			}
			lines = append(lines, indent(explainPlan(p))...)
		} else {
			lines = append(lines, fmt.Sprintf("└─ Values (%d rows)", len(st.Rows)))
		}
		return lines, nil
	}
	return []string{strings.TrimPrefix(fmt.Sprintf("%T", s), "*sql.")}, nil
}

func indent(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if i == 0 {
			out[i] = "└─ " + l
		} else {
			out[i] = "   " + l
		}
	}
	return out
}

// constEval evaluates a constant expression such as a column default.
func constEval(e sql.Expr, ctx *execCtx) (value.Value, error) {
	comp := &compiler{}
	f, err := comp.compileExpr(e, &scope{})
	if err != nil {
		return value.Value{}, err
	}
	if ctx == nil {
		ctx = &execCtx{}
	}
	return f(&env{ctx: ctx})
}
