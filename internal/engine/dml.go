package engine

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/integral0909/cairn/internal/btree"
	"github.com/integral0909/cairn/internal/pager"
	"github.com/integral0909/cairn/internal/sql"
	"github.com/integral0909/cairn/internal/value"
)

// writer runs one statement that changes data or schema inside a write
// transaction. Its catalog becomes the transaction's if the statement
// succeeds.
type writer struct {
	tx  *pager.WriteTx
	cat *Catalog
	ctx *execCtx
}

func (w *writer) exec(s sql.Stmt) (*Rows, Result, error) {
	switch st := s.(type) {
	case *sql.Insert:
		return w.insert(st)
	case *sql.Update:
		return w.update(st)
	case *sql.Delete:
		return w.delete(st)
	case *sql.CreateTable:
		return nil, Result{}, w.createTable(st)
	case *sql.DropTable:
		return nil, Result{}, w.dropTable(st)
	case *sql.CreateIndex:
		return nil, Result{}, w.createIndex(st)
	case *sql.DropIndex:
		return nil, Result{}, w.dropIndex(st)
	case *sql.AlterTable:
		return nil, Result{}, w.alterTable(st)
	case *sql.Analyze:
		return nil, Result{}, errors.New("ANALYZE is not needed: the planner chooses plans by rules, not statistics")
	}
	return nil, Result{}, fmt.Errorf("unsupported statement %T", s)
}

func (w *writer) table(name string) (*Table, error) {
	t := w.cat.Table(name)
	if t == nil {
		return nil, fmt.Errorf("no such table: %s", name)
	}
	return t, nil
}

// ---- rows and index entries ----------------------------------------------------

func indexKey(ix *Index, row []value.Value, rowid int64) ([]byte, bool, error) {
	var k []byte
	hasNull := false
	for _, c := range ix.Columns {
		v := row[c]
		if v.IsNull() {
			hasNull = true
		}
		if v.K == value.Bool {
			v = value.NewInt(v.I)
		}
		k = value.AppendKey(k, v)
	}
	k = append(k, value.RowidKey(rowid)...)
	if len(k) > btree.MaxKey {
		return nil, false, fmt.Errorf("value too large for index %s (indexed values are limited to about %d bytes)", ix.Name, btree.MaxKey-8)
	}
	return k, hasNull, nil
}

// conflictRowid looks for another row with the same unique key.
func (w *writer) conflictRowid(ix *Index, row []value.Value, rowid int64) (int64, bool, error) {
	key, hasNull, err := indexKey(ix, row, rowid)
	if err != nil || hasNull {
		return 0, false, err // NULLs never conflict
	}
	prefix := key[:len(key)-8]
	c := btree.NewCursor(w.tx, ix.Root)
	for c.Seek(prefix); c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Next() {
		if other := value.RowidFromKey(c.Key()); other != rowid {
			return other, true, nil
		}
	}
	return 0, false, c.Err()
}

func (w *writer) readRow(t *Table, rowid int64) ([]value.Value, bool, error) {
	key := value.RowidKey(rowid)
	raw, ok, err := btree.Get(w.tx, t.Root, key)
	if err != nil || !ok {
		return nil, false, err
	}
	row, err := tableRow(t, key, raw, w.ctx)
	return row, err == nil, err
}

func (w *writer) writeRow(t *Table, rowid int64, row []value.Value) error {
	stored := row
	if t.RowidCol >= 0 {
		// The rowid alias lives in the key, not the record.
		stored = append([]value.Value(nil), row...)
		stored[t.RowidCol] = value.Value{}
	}
	if err := btree.Put(w.tx, t.Root, value.RowidKey(rowid), value.EncodeRecord(stored)); err != nil {
		return err
	}
	for _, ix := range t.Indexes {
		k, _, err := indexKey(ix, row, rowid)
		if err != nil {
			return err
		}
		if err := btree.Put(w.tx, ix.Root, k, nil); err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) removeRow(t *Table, rowid int64, row []value.Value) error {
	if _, err := btree.Delete(w.tx, t.Root, value.RowidKey(rowid)); err != nil {
		return err
	}
	for _, ix := range t.Indexes {
		k, _, err := indexKey(ix, row, rowid)
		if err != nil {
			return err
		}
		if _, err := btree.Delete(w.tx, ix.Root, k); err != nil {
			return err
		}
	}
	return nil
}

func nextRowid(r btree.Reader, t *Table) (int64, error) {
	c := btree.NewCursor(r, t.Root)
	c.Last()
	if err := c.Err(); err != nil {
		return 0, err
	}
	if !c.Valid() {
		return 1, nil
	}
	last := value.RowidFromKey(c.Key())
	if last == math.MaxInt64 {
		return 0, fmt.Errorf("table %s has used the largest rowid; give rows explicit ids", t.Name)
	}
	return max(last+1, 1), nil
}

func constraintName(t *Table, cols []int) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = t.Name + "." + t.Columns[c].Name
	}
	return strings.Join(names, ", ")
}

// checkRow validates a complete row: types, NOT NULL.
func checkRow(t *Table, row []value.Value) error {
	for i, col := range t.Columns {
		v, err := coerce(row[i], col.Type, t.Name+"."+col.Name)
		if err != nil {
			return err
		}
		row[i] = v
		if v.IsNull() && col.NotNull && i != t.RowidCol {
			return fmt.Errorf("NOT NULL constraint failed: %s.%s", t.Name, col.Name)
		}
	}
	return nil
}

// insertRow adds a row, handling conflicts as mode says ("", "nothing" or
// "replace"). It returns the rowid and whether a row was written.
func (w *writer) insertRow(t *Table, row []value.Value, mode string) (int64, bool, error) {
	return w.insertRowAt(t, row, mode, 0, false)
}

// insertRowAt is insertRow with the rowid fixed in advance (used by UPDATE,
// which keeps a row's identity).
func (w *writer) insertRowAt(t *Table, row []value.Value, mode string, fixed int64, hasFixed bool) (int64, bool, error) {
	if err := checkRow(t, row); err != nil {
		return 0, false, err
	}
	var rowid int64
	if hasFixed {
		rowid = fixed
		if t.RowidCol >= 0 {
			row[t.RowidCol] = value.NewInt(rowid)
		}
	} else if t.RowidCol >= 0 && !row[t.RowidCol].IsNull() {
		rowid = row[t.RowidCol].I
	} else {
		id, err := nextRowid(w.tx, t)
		if err != nil {
			return 0, false, err
		}
		rowid = id
		if t.RowidCol >= 0 {
			row[t.RowidCol] = value.NewInt(rowid)
		}
	}
	// Collect conflicts: the rowid itself, then each unique index.
	type conflict struct {
		rowid int64
		what  string
	}
	var conflicts []conflict
	if _, ok, err := btree.Get(w.tx, t.Root, value.RowidKey(rowid)); err != nil {
		return 0, false, err
	} else if ok {
		what := "PRIMARY KEY constraint failed: " + t.Name + "." + t.rowidName()
		conflicts = append(conflicts, conflict{rowid, what})
	}
	for _, ix := range t.Indexes {
		if !ix.Unique {
			continue
		}
		other, ok, err := w.conflictRowid(ix, row, rowid)
		if err != nil {
			return 0, false, err
		}
		if ok {
			kind := "UNIQUE"
			if ix.Auto && sameCols(ix.Columns, t.PrimaryKey) {
				kind = "PRIMARY KEY"
			}
			conflicts = append(conflicts, conflict{other, kind + " constraint failed: " + constraintName(t, ix.Columns)})
		}
	}
	if len(conflicts) > 0 {
		switch mode {
		case "nothing":
			return rowid, false, nil
		case "replace":
			for _, cf := range conflicts {
				old, ok, err := w.readRow(t, cf.rowid)
				if err != nil {
					return 0, false, err
				}
				if ok {
					if err := w.removeRow(t, cf.rowid, old); err != nil {
						return 0, false, err
					}
				}
			}
		default:
			return 0, false, errors.New(conflicts[0].what)
		}
	}
	return rowid, true, w.writeRow(t, rowid, row)
}

func sameCols(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- INSERT ------------------------------------------------------------------------

func (w *writer) insert(st *sql.Insert) (*Rows, Result, error) {
	t, err := w.table(st.Table)
	if err != nil {
		return nil, Result{}, err
	}
	// Which table column each supplied value goes to.
	var target []int
	if st.Columns == nil {
		for i := range t.Columns {
			target = append(target, i)
		}
	} else {
		seen := map[int]bool{}
		for _, name := range st.Columns {
			c := t.column(name)
			if c < 0 {
				return nil, Result{}, fmt.Errorf("table %s has no column %s", t.Name, name)
			}
			if seen[c] {
				return nil, Result{}, fmt.Errorf("column %s is listed twice", name)
			}
			seen[c] = true
			target = append(target, c)
		}
	}

	comp := &compiler{cat: w.cat}
	var source [][]value.Value
	if st.Select != nil {
		p, err := comp.planSelect(st.Select)
		if err != nil {
			return nil, Result{}, err
		}
		if len(p.columns()) != len(target) {
			return nil, Result{}, fmt.Errorf("INSERT has %d target columns but the SELECT gives %d", len(target), len(p.columns()))
		}
		if err := checkArgs(comp.maxParam, w.ctx.params); err != nil {
			return nil, Result{}, err
		}
		// Read everything first, so INSERT INTO t SELECT ... FROM t sees
		// only the rows that existed before.
		if source, err = drain(p, w.ctx, nil); err != nil {
			return nil, Result{}, err
		}
	} else {
		empty := &scope{}
		for _, exprs := range st.Rows {
			if len(exprs) != len(target) && !(len(exprs) == 0 && st.Columns == nil) {
				return nil, Result{}, fmt.Errorf("INSERT has %d target columns but %d values", len(target), len(exprs))
			}
			row := make([]value.Value, len(exprs))
			for i, e := range exprs {
				f, err := comp.compileExpr(e, empty)
				if err != nil {
					return nil, Result{}, err
				}
				if err := checkArgs(comp.maxParam, w.ctx.params); err != nil {
					return nil, Result{}, err
				}
				if row[i], err = f(&env{ctx: w.ctx}); err != nil {
					return nil, Result{}, err
				}
			}
			source = append(source, row)
		}
	}

	ret, err := w.returning(t, st.Returning)
	if err != nil {
		return nil, Result{}, err
	}
	var res Result
	for _, vals := range source {
		if err := w.ctx.tick(); err != nil {
			return nil, Result{}, err
		}
		row := make([]value.Value, len(t.Columns))
		supplied := make([]bool, len(t.Columns))
		if len(vals) > 0 {
			for i, c := range target {
				row[c] = vals[i]
				supplied[c] = true
			}
		}
		for i, col := range t.Columns {
			if !supplied[i] && col.Default != nil {
				if row[i], err = constEval(col.Default, w.ctx); err != nil {
					return nil, Result{}, err
				}
			}
		}
		rowid, written, err := w.insertRow(t, row, st.OnConflict)
		if err != nil {
			return nil, Result{}, err
		}
		if written {
			res.RowsAffected++
			res.LastInsertID = rowid
			if err := ret.add(row); err != nil {
				return nil, Result{}, err
			}
		}
	}
	return ret.rows(), res, nil
}

// returningList evaluates RETURNING expressions over changed rows.
type returningList struct {
	exprs []evalFn
	names []string
	out   [][]value.Value
	ctx   *execCtx
}

func (w *writer) returning(t *Table, cols []sql.ResultCol) (*returningList, error) {
	if len(cols) == 0 {
		return nil, nil
	}
	sc := &scope{sources: []*source{{alias: t.Name, table: t, columns: columnNames(t)}}}
	comp := &compiler{cat: w.cat}
	rl := &returningList{ctx: w.ctx}
	for _, rc := range cols {
		if rc.Star {
			for i, c := range t.Columns {
				idx := i
				rl.exprs = append(rl.exprs, func(en *env) (value.Value, error) { return en.row[idx], nil })
				rl.names = append(rl.names, c.Name)
			}
			continue
		}
		if hasAggregate(rc.Expr) {
			return nil, fmt.Errorf("aggregates are not allowed in RETURNING")
		}
		f, err := comp.compileExpr(rc.Expr, sc)
		if err != nil {
			return nil, fmt.Errorf("RETURNING: %w", err)
		}
		rl.exprs = append(rl.exprs, f)
		rl.names = append(rl.names, resultName(rc))
	}
	return rl, nil
}

func (rl *returningList) add(row []value.Value) error {
	if rl == nil {
		return nil
	}
	en := &env{row: row, ctx: rl.ctx}
	out := make([]value.Value, len(rl.exprs))
	for i, f := range rl.exprs {
		v, err := f(en)
		if err != nil {
			return err
		}
		out[i] = v
	}
	rl.out = append(rl.out, out)
	return nil
}

func (rl *returningList) rows() *Rows {
	if rl == nil {
		return nil
	}
	return &Rows{cols: rl.names, buffered: rl.out}
}

// ---- UPDATE and DELETE ---------------------------------------------------------------

// dmlScan plans the rows an UPDATE or DELETE touches; its rows carry the
// rowid as an extra last column.
func (c *compiler) dmlScan(table string, where sql.Expr) (plan, error) {
	t := c.cat.Table(table)
	if t == nil {
		return nil, fmt.Errorf("no such table: %s", table)
	}
	if where != nil && hasAggregate(where) {
		return nil, fmt.Errorf("aggregates are not allowed in WHERE")
	}
	u := &unit{table: t, alias: t.Name, width: len(t.Columns), sources: []*source{{alias: t.Name, table: t, columns: columnNames(t)}}}
	sc := &scope{sources: u.sources}
	units := []*unit{u}
	var conds []*conjunct
	for _, e := range splitAnd(where, nil) {
		conds = append(conds, &conjunct{expr: e, fromWhere: true})
	}
	var refErr error
	refs := func(e sql.Expr) uint64 {
		r, err := c.refsOf(e, sc, units)
		if err != nil && refErr == nil {
			refErr = err
		}
		return r
	}
	for _, cj := range conds {
		cj.refs = refs(cj.expr)
	}
	if refErr != nil {
		return nil, refErr
	}
	p, err := c.accessPlan(u, 0, conds, nil, &scope{}, refs)
	if err != nil {
		return nil, err
	}
	switch x := p.(type) {
	case *scanPlan:
		x.withRowid = true
	case *indexPlan:
		x.withRowid = true
	}
	return p, nil
}

type target struct {
	rowid int64
	row   []value.Value
}

func (w *writer) targets(table string, where sql.Expr) (*Table, []target, *compiler, error) {
	t, err := w.table(table)
	if err != nil {
		return nil, nil, nil, err
	}
	comp := &compiler{cat: w.cat}
	p, err := comp.dmlScan(table, where)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := checkArgs(comp.maxParam, w.ctx.params); err != nil {
		return nil, nil, nil, err
	}
	rows, err := drain(p, w.ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	out := make([]target, len(rows))
	for i, r := range rows {
		out[i] = target{rowid: r[len(r)-1].I, row: r[:len(r)-1]}
	}
	return t, out, comp, nil
}

func (w *writer) update(st *sql.Update) (*Rows, Result, error) {
	t, rows, comp, err := w.targets(st.Table, st.Where)
	if err != nil {
		return nil, Result{}, err
	}
	sc := &scope{sources: []*source{{alias: t.Name, table: t, columns: columnNames(t)}}}
	type assign struct {
		col int
		fn  evalFn
	}
	var sets []assign
	seen := map[int]bool{}
	for _, a := range st.Set {
		col := t.column(a.Column)
		if col < 0 {
			return nil, Result{}, fmt.Errorf("table %s has no column %s", t.Name, a.Column)
		}
		if seen[col] {
			return nil, Result{}, fmt.Errorf("column %s is assigned twice", a.Column)
		}
		seen[col] = true
		if hasAggregate(a.Value) {
			return nil, Result{}, fmt.Errorf("aggregates are not allowed in SET")
		}
		f, err := comp.compileExpr(a.Value, sc)
		if err != nil {
			return nil, Result{}, err
		}
		sets = append(sets, assign{col, f})
	}
	if err := checkArgs(comp.maxParam, w.ctx.params); err != nil {
		return nil, Result{}, err
	}
	ret, err := w.returning(t, st.Returning)
	if err != nil {
		return nil, Result{}, err
	}
	var res Result
	for _, tg := range rows {
		if err := w.ctx.tick(); err != nil {
			return nil, Result{}, err
		}
		next := append([]value.Value(nil), tg.row...)
		en := &env{row: tg.row, ctx: w.ctx}
		for _, s := range sets {
			if next[s.col], err = s.fn(en); err != nil {
				return nil, Result{}, err
			}
		}
		newRowid := tg.rowid
		if t.RowidCol >= 0 {
			v, err := coerce(next[t.RowidCol], value.Int, t.Name+"."+t.Columns[t.RowidCol].Name)
			if err != nil {
				return nil, Result{}, err
			}
			if v.IsNull() {
				return nil, Result{}, fmt.Errorf("NOT NULL constraint failed: %s.%s", t.Name, t.Columns[t.RowidCol].Name)
			}
			newRowid = v.I
		}
		if err := w.removeRow(t, tg.rowid, tg.row); err != nil {
			return nil, Result{}, err
		}
		if _, _, err := w.insertRowAt(t, next, "", newRowid, true); err != nil {
			return nil, Result{}, err
		}
		res.RowsAffected++
		if err := ret.add(next); err != nil {
			return nil, Result{}, err
		}
	}
	return ret.rows(), res, nil
}

func (w *writer) delete(st *sql.Delete) (*Rows, Result, error) {
	t, err := w.table(st.Table)
	if err != nil {
		return nil, Result{}, err
	}
	ret, err := w.returning(t, st.Returning)
	if err != nil {
		return nil, Result{}, err
	}
	if st.Where == nil && ret == nil {
		// Delete everything: empty the trees instead of row by row.
		n := int64(0)
		c := btree.NewCursor(w.tx, t.Root)
		for c.First(); c.Valid(); c.Next() {
			n++
		}
		if err := c.Err(); err != nil {
			return nil, Result{}, err
		}
		if err := btree.Truncate(w.tx, t.Root); err != nil {
			return nil, Result{}, err
		}
		for _, ix := range t.Indexes {
			if err := btree.Truncate(w.tx, ix.Root); err != nil {
				return nil, Result{}, err
			}
		}
		return nil, Result{RowsAffected: n}, nil
	}
	_, rows, _, err := w.targets(st.Table, st.Where)
	if err != nil {
		return nil, Result{}, err
	}
	for _, tg := range rows {
		if err := w.ctx.tick(); err != nil {
			return nil, Result{}, err
		}
		if err := w.removeRow(t, tg.rowid, tg.row); err != nil {
			return nil, Result{}, err
		}
		if err := ret.add(tg.row); err != nil {
			return nil, Result{}, err
		}
	}
	return ret.rows(), Result{RowsAffected: int64(len(rows))}, nil
}
