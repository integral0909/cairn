package engine

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/integral0909/cairn/internal/btree"
	"github.com/integral0909/cairn/internal/value"
)

// execCtx is shared by everything one statement runs.
type execCtx struct {
	r      btree.Reader
	params []value.Value
	// Results of uncorrelated subqueries, computed once per statement.
	subRows map[*subPlan][][]value.Value
	subSets map[*subPlan]*valueSet
	// Interrupt checks; set by the connection.
	cancel func() error
	steps  int
}

func (c *execCtx) tick() error {
	c.steps++
	if c.cancel != nil && c.steps%1024 == 0 {
		return c.cancel()
	}
	return nil
}

// plan is a node of a query plan. open starts a run; outer carries the rows
// of enclosing queries for correlated subqueries and lateral lookups.
type plan interface {
	open(ctx *execCtx, outer *env) (iter, error)
	columns() []string
}

// iter produces rows; next returns nil at the end.
type iter interface {
	next() ([]value.Value, error)
	close()
}

// ---- scans -------------------------------------------------------------------

// tableRow decodes a stored row, padding rows written before columns were
// added, and puts the rowid into its alias column.
func tableRow(t *Table, key, raw []byte, ctx *execCtx) ([]value.Value, error) {
	vals, err := value.DecodeRecord(raw)
	if err != nil {
		return nil, fmt.Errorf("table %s: %w", t.Name, err)
	}
	if len(vals) < len(t.Columns) {
		for i := len(vals); i < len(t.Columns); i++ {
			v := value.Value{}
			if d := t.Columns[i].Default; d != nil {
				if v, err = constEval(d, ctx); err != nil {
					return nil, err
				}
			}
			vals = append(vals, v)
		}
	}
	if t.RowidCol >= 0 {
		vals[t.RowidCol] = value.NewInt(value.RowidFromKey(key))
	}
	return vals, nil
}

// scanPlan reads a table in rowid order, optionally limited to a rowid
// range, with a filter applied to each row.
type scanPlan struct {
	table      *Table
	alias      string
	lo, hi     *bound // rowid range, from expressions over outer rows
	filter     evalFn
	filterText string
	// rowid equality: a single lookup.
	eq     evalFn
	eqText string
	// outerKeys: the lookup keys come from the outer row of a join.
	outerKeys bool
	// withRowid appends the rowid as an extra column (for UPDATE/DELETE).
	withRowid bool
}

type bound struct {
	fn        evalFn
	inclusive bool
	text      string
}

func (p *scanPlan) columns() []string { return columnNames(p.table) }

func columnNames(t *Table) []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Name
	}
	return out
}

func (p *scanPlan) open(ctx *execCtx, outer *env) (iter, error) {
	it := &scanIter{p: p, ctx: ctx, outer: outer}
	en := &env{outer: outer, ctx: ctx}
	if p.eq != nil {
		v, err := p.eq(en)
		if err != nil {
			return nil, err
		}
		id, ok, err := rowidValue(v)
		if err != nil || !ok {
			return &emptyIter{}, err
		}
		it.single = true
		it.key = value.RowidKey(id)
		return it, nil
	}
	it.c = btree.NewCursor(ctx.r, p.table.Root)
	if p.lo != nil {
		v, err := p.lo.fn(en)
		if err != nil {
			return nil, err
		}
		if v.IsNull() {
			return &emptyIter{}, nil
		}
		key, skipEqual, err := rowidBound(v, p.lo.inclusive, true)
		if err != nil {
			return nil, err
		}
		it.c.Seek(key)
		if skipEqual && it.c.Valid() && bytes.Equal(it.c.Key(), key) {
			it.c.Next()
		}
	} else {
		it.c.First()
	}
	if p.hi != nil {
		v, err := p.hi.fn(en)
		if err != nil {
			return nil, err
		}
		if v.IsNull() {
			return &emptyIter{}, nil
		}
		key, _, err := rowidBound(v, p.hi.inclusive, false)
		if err != nil {
			return nil, err
		}
		it.hiKey, it.hiIncl = key, p.hi.inclusive
		if v.K == value.Real && v.F != float64(int64(v.F)) {
			it.hiIncl = true // hi was rounded down to an integer
		}
	}
	return it, nil
}

// rowidValue converts a lookup value to a rowid; values that cannot be a
// rowid (3.5, 'abc') simply match nothing.
func rowidValue(v value.Value) (int64, bool, error) {
	switch v.K {
	case value.Int, value.Bool:
		return v.I, true, nil
	case value.Real:
		if v.F == float64(int64(v.F)) {
			return int64(v.F), true, nil
		}
		return 0, false, nil
	case value.Null:
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("cannot compare %s with INTEGER", v.K)
}

// rowidBound turns a range bound into a key. Non-integral bounds round
// toward the inside of the range.
func rowidBound(v value.Value, inclusive, lower bool) ([]byte, bool, error) {
	switch v.K {
	case value.Int, value.Bool:
		return value.RowidKey(v.I), !inclusive, nil
	case value.Real:
		f := v.F
		if f >= 9.2e18 {
			f = 9.2e18
		}
		if f <= -9.2e18 {
			f = -9.2e18
		}
		i := int64(f)
		if float64(i) == f {
			return value.RowidKey(i), !inclusive, nil
		}
		if lower {
			if f > 0 {
				i++
			}
			return value.RowidKey(i), false, nil
		}
		if f < 0 {
			i--
		}
		return value.RowidKey(i), false, nil
	}
	return nil, false, fmt.Errorf("cannot compare %s with INTEGER", v.K)
}

type scanIter struct {
	p      *scanPlan
	ctx    *execCtx
	outer  *env
	c      *btree.Cursor
	single bool
	key    []byte
	done   bool
	hiKey  []byte
	hiIncl bool
	en     env
}

func (it *scanIter) next() ([]value.Value, error) {
	for {
		if err := it.ctx.tick(); err != nil {
			return nil, err
		}
		var key, raw []byte
		if it.single {
			if it.done {
				return nil, nil
			}
			it.done = true
			v, ok, err := btree.Get(it.ctx.r, it.p.table.Root, it.key)
			if err != nil || !ok {
				return nil, err
			}
			key, raw = it.key, v
		} else {
			if !it.c.Valid() {
				return nil, it.c.Err()
			}
			key = it.c.Key()
			if it.hiKey != nil {
				c := bytes.Compare(key, it.hiKey)
				if c > 0 || (c == 0 && !it.hiIncl) {
					return nil, nil
				}
			}
			var err error
			if raw, err = it.c.Value(); err != nil {
				return nil, err
			}
			key = append([]byte(nil), key...)
			it.c.Next()
		}
		row, err := tableRow(it.p.table, key, raw, it.ctx)
		if err != nil {
			return nil, err
		}
		if it.p.filter != nil {
			it.en = env{row: row, outer: it.outer, ctx: it.ctx}
			v, err := it.p.filter(&it.en)
			if err != nil {
				return nil, err
			}
			if t, k := truth(v); !k || !t {
				continue
			}
		}
		if it.p.withRowid {
			row = append(row, value.NewInt(value.RowidFromKey(key)))
		}
		return row, nil
	}
}

func (it *scanIter) close() {}

// indexPlan finds rows through an index: equality on a prefix of the index
// columns, optionally a range on the next one.
type indexPlan struct {
	table      *Table
	alias      string
	index      *Index
	eq         []evalFn
	eqText     []string
	lo, hi     *bound
	filter     evalFn
	filterText string
	outerKeys  bool
	withRowid  bool
}

func (p *indexPlan) columns() []string { return columnNames(p.table) }

func (p *indexPlan) open(ctx *execCtx, outer *env) (iter, error) {
	en := &env{outer: outer, ctx: ctx}
	var prefix []byte
	for i, f := range p.eq {
		v, err := f(en)
		if err != nil {
			return nil, err
		}
		if v.IsNull() {
			return &emptyIter{}, nil // = NULL matches nothing
		}
		if v, err = indexValue(v, p.table.Columns[p.index.Columns[i]].Type); err != nil {
			return nil, err
		}
		prefix = value.AppendKey(prefix, v)
	}
	it := &indexIter{p: p, ctx: ctx, outer: outer, prefix: prefix, c: btree.NewCursor(ctx.r, p.index.Root)}
	start := prefix
	if p.lo != nil {
		v, err := p.lo.fn(en)
		if err != nil {
			return nil, err
		}
		if v.IsNull() {
			return &emptyIter{}, nil
		}
		if v, err = indexValue(v, p.table.Columns[p.index.Columns[len(p.eq)]].Type); err != nil {
			return nil, err
		}
		start = value.AppendKey(append([]byte(nil), prefix...), v)
		if !p.lo.inclusive {
			// Skip every entry equal to the bound: they all start with
			// start, so seek past that prefix.
			start = prefixEnd(start)
		}
	} else if p.hi != nil {
		// Range with only an upper bound: NULLs sort first and never
		// satisfy a comparison, so start after them.
		start = prefixEnd(value.AppendKey(append([]byte(nil), prefix...), value.Value{}))
	}
	if p.hi != nil {
		v, err := p.hi.fn(en)
		if err != nil {
			return nil, err
		}
		if v.IsNull() {
			return &emptyIter{}, nil
		}
		if v, err = indexValue(v, p.table.Columns[p.index.Columns[len(p.eq)]].Type); err != nil {
			return nil, err
		}
		end := value.AppendKey(append([]byte(nil), prefix...), v)
		if p.hi.inclusive {
			end = prefixEnd(end)
		}
		it.end = end
	} else {
		it.end = prefixEnd(prefix)
	}
	it.c.Seek(start)
	return it, nil
}

// indexValue checks a lookup value against the column type (so a lookup
// errs exactly where a scan's comparison would) and normalises it to encode
// like stored values.
func indexValue(v value.Value, typ value.Kind) (value.Value, error) {
	if categoryOf(v) != categoryOf(value.Value{K: typ}) {
		return value.Value{}, fmt.Errorf("cannot compare %s with %s", typ, v.K)
	}
	if v.K == value.Bool {
		return value.NewInt(v.I), nil
	}
	return v, nil
}

// prefixEnd returns the smallest key greater than every key starting with
// p (nil means "no upper limit").
func prefixEnd(p []byte) []byte {
	out := append([]byte(nil), p...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

type indexIter struct {
	p      *indexPlan
	ctx    *execCtx
	outer  *env
	prefix []byte
	end    []byte
	c      *btree.Cursor
	en     env
}

func (it *indexIter) next() ([]value.Value, error) {
	for {
		if err := it.ctx.tick(); err != nil {
			return nil, err
		}
		if !it.c.Valid() {
			return nil, it.c.Err()
		}
		k := it.c.Key()
		if it.end != nil && bytes.Compare(k, it.end) >= 0 {
			return nil, nil
		}
		rowidKey := value.RowidKey(value.RowidFromKey(k))
		it.c.Next()
		raw, ok, err := btree.Get(it.ctx.r, it.p.table.Root, rowidKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("index %s points at missing row %d", it.p.index.Name, value.RowidFromKey(rowidKey))
		}
		row, err := tableRow(it.p.table, rowidKey, raw, it.ctx)
		if err != nil {
			return nil, err
		}
		if it.p.filter != nil {
			it.en = env{row: row, outer: it.outer, ctx: it.ctx}
			v, err := it.p.filter(&it.en)
			if err != nil {
				return nil, err
			}
			if t, k := truth(v); !k || !t {
				continue
			}
		}
		if it.p.withRowid {
			row = append(row, value.NewInt(value.RowidFromKey(rowidKey)))
		}
		return row, nil
	}
}

func (it *indexIter) close() {}

type emptyIter struct{}

func (emptyIter) next() ([]value.Value, error) { return nil, nil }
func (emptyIter) close()                       {}

// oneRowPlan is the FROM-less source: a single empty row.
type oneRowPlan struct{}

func (oneRowPlan) columns() []string { return nil }
func (oneRowPlan) open(*execCtx, *env) (iter, error) {
	return &sliceIter{rows: [][]value.Value{{}}}, nil
}

type sliceIter struct {
	rows [][]value.Value
	i    int
}

func (it *sliceIter) next() ([]value.Value, error) {
	if it.i >= len(it.rows) {
		return nil, nil
	}
	it.i++
	return it.rows[it.i-1], nil
}
func (it *sliceIter) close() {}

// ---- filter and project --------------------------------------------------------

type filterPlan struct {
	input plan
	pred  evalFn
	text  string
}

func (p *filterPlan) columns() []string { return p.input.columns() }

func (p *filterPlan) open(ctx *execCtx, outer *env) (iter, error) {
	in, err := p.input.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &filterIter{in: in, pred: p.pred, outer: outer, ctx: ctx}, nil
}

type filterIter struct {
	in    iter
	pred  evalFn
	outer *env
	ctx   *execCtx
}

func (it *filterIter) next() ([]value.Value, error) {
	for {
		row, err := it.in.next()
		if row == nil || err != nil {
			return nil, err
		}
		v, err := it.pred(&env{row: row, outer: it.outer, ctx: it.ctx})
		if err != nil {
			return nil, err
		}
		if t, k := truth(v); k && t {
			return row, nil
		}
	}
}

func (it *filterIter) close() { it.in.close() }

type projectPlan struct {
	input plan
	exprs []evalFn
	names []string
	texts []string
}

func (p *projectPlan) columns() []string { return p.names }

func (p *projectPlan) open(ctx *execCtx, outer *env) (iter, error) {
	in, err := p.input.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &projectIter{in: in, p: p, outer: outer, ctx: ctx}, nil
}

type projectIter struct {
	in    iter
	p     *projectPlan
	outer *env
	ctx   *execCtx
}

func (it *projectIter) next() ([]value.Value, error) {
	row, err := it.in.next()
	if row == nil || err != nil {
		return nil, err
	}
	en := &env{row: row, outer: it.outer, ctx: it.ctx}
	out := make([]value.Value, len(it.p.exprs))
	for i, f := range it.p.exprs {
		if out[i], err = f(en); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (it *projectIter) close() { it.in.close() }

// ---- joins ------------------------------------------------------------------------

// nestedLoopPlan runs the inner plan once per outer row, with the outer
// row visible to it (so the inner side can be an index lookup keyed by
// outer columns).
type nestedLoopPlan struct {
	left, right plan
	leftWidth   int
	rightWidth  int
	on          evalFn // evaluated on the joined row; nil means always
	onText      string
	leftJoin    bool
	lookup      bool // the inner side uses the outer row (index nested loop)
}

func (p *nestedLoopPlan) columns() []string { return append(p.left.columns(), p.right.columns()...) }

func (p *nestedLoopPlan) open(ctx *execCtx, outer *env) (iter, error) {
	l, err := p.left.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &nestedLoopIter{p: p, ctx: ctx, outer: outer, left: l}, nil
}

type nestedLoopIter struct {
	p       *nestedLoopPlan
	ctx     *execCtx
	outer   *env
	left    iter
	right   iter
	lrow    []value.Value
	matched bool
}

func (it *nestedLoopIter) next() ([]value.Value, error) {
	for {
		if it.right == nil {
			row, err := it.left.next()
			if row == nil || err != nil {
				return nil, err
			}
			it.lrow, it.matched = row, false
			// The inner side sees the left row as its enclosing row.
			r, err := it.p.right.open(it.ctx, &env{row: padRow(row, it.p.leftWidth+it.p.rightWidth), outer: it.outer, ctx: it.ctx})
			if err != nil {
				return nil, err
			}
			it.right = r
		}
		rrow, err := it.right.next()
		if err != nil {
			return nil, err
		}
		if rrow == nil {
			it.right.close()
			it.right = nil
			if it.p.leftJoin && !it.matched {
				return padRow(it.lrow, it.p.leftWidth+it.p.rightWidth), nil
			}
			continue
		}
		joined := append(append(make([]value.Value, 0, it.p.leftWidth+it.p.rightWidth), it.lrow...), rrow...)
		if it.p.on != nil {
			v, err := it.p.on(&env{row: joined, outer: it.outer, ctx: it.ctx})
			if err != nil {
				return nil, err
			}
			if t, k := truth(v); !k || !t {
				continue
			}
		}
		it.matched = true
		return joined, nil
	}
}

func (it *nestedLoopIter) close() {
	if it.right != nil {
		it.right.close()
	}
	it.left.close()
}

// padRow extends a row with NULLs to width.
func padRow(row []value.Value, width int) []value.Value {
	out := make([]value.Value, width)
	copy(out, row)
	return out
}

// hashJoinPlan builds a hash table of the right side keyed by the join
// columns, then streams the left side through it.
type hashJoinPlan struct {
	left, right plan
	leftWidth   int
	rightWidth  int
	leftKeys    []evalFn // over left rows
	rightKeys   []evalFn // over right rows
	keyText     []string
	residual    evalFn // other ON conditions, over the joined row
	residText   string
	leftJoin    bool
}

func (p *hashJoinPlan) columns() []string { return append(p.left.columns(), p.right.columns()...) }

func (p *hashJoinPlan) open(ctx *execCtx, outer *env) (iter, error) {
	r, err := p.right.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	defer r.close()
	table := map[string][][]value.Value{}
	for {
		row, err := r.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		key, ok, err := joinKey(p.rightKeys, &env{row: row, outer: outer, ctx: ctx})
		if err != nil {
			return nil, err
		}
		if ok {
			table[key] = append(table[key], row)
		}
	}
	l, err := p.left.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &hashJoinIter{p: p, ctx: ctx, outer: outer, left: l, table: table}, nil
}

// joinKey encodes the key columns; a NULL key never matches anything.
func joinKey(fns []evalFn, en *env) (string, bool, error) {
	var b []byte
	for _, f := range fns {
		v, err := f(en)
		if err != nil {
			return "", false, err
		}
		if v.IsNull() {
			return "", false, nil
		}
		if v.K == value.Bool {
			v = value.NewInt(v.I)
		}
		b = value.AppendKey(b, v)
	}
	return string(b), true, nil
}

type hashJoinIter struct {
	p       *hashJoinPlan
	ctx     *execCtx
	outer   *env
	left    iter
	table   map[string][][]value.Value
	lrow    []value.Value
	matches [][]value.Value
	i       int
	matched bool
}

func (it *hashJoinIter) next() ([]value.Value, error) {
	for {
		for it.i < len(it.matches) {
			r := it.matches[it.i]
			it.i++
			joined := append(append(make([]value.Value, 0, it.p.leftWidth+it.p.rightWidth), it.lrow...), r...)
			if it.p.residual != nil {
				v, err := it.p.residual(&env{row: joined, outer: it.outer, ctx: it.ctx})
				if err != nil {
					return nil, err
				}
				if t, k := truth(v); !k || !t {
					continue
				}
			}
			it.matched = true
			return joined, nil
		}
		if it.lrow != nil && it.p.leftJoin && !it.matched {
			it.matched = true
			return padRow(it.lrow, it.p.leftWidth+it.p.rightWidth), nil
		}
		row, err := it.left.next()
		if row == nil || err != nil {
			return nil, err
		}
		if err := it.ctx.tick(); err != nil {
			return nil, err
		}
		key, ok, err := joinKey(it.p.leftKeys, &env{row: row, outer: it.outer, ctx: it.ctx})
		if err != nil {
			return nil, err
		}
		it.lrow, it.i, it.matched = row, 0, false
		it.matches = nil
		if ok {
			it.matches = it.table[key]
		}
	}
}

func (it *hashJoinIter) close() { it.left.close() }

// ---- sort, limit, distinct ---------------------------------------------------------

type sortKey struct {
	col        int
	desc       bool
	nullsFirst bool
}

type sortPlan struct {
	input plan
	keys  []sortKey
	texts []string
}

func (p *sortPlan) columns() []string { return p.input.columns() }

func (p *sortPlan) open(ctx *execCtx, outer *env) (iter, error) {
	rows, err := drain(p.input, ctx, outer)
	if err != nil {
		return nil, err
	}
	var cmpErr error
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range p.keys {
			a, b := rows[i][k.col], rows[j][k.col]
			if a.IsNull() || b.IsNull() {
				if a.IsNull() == b.IsNull() {
					continue
				}
				return a.IsNull() == k.nullsFirst
			}
			c, err := compareSQL(a, b)
			if err != nil {
				cmpErr = err
				c = value.Compare(a, b)
			}
			if c != 0 {
				return (c < 0) != k.desc
			}
		}
		return false
	})
	if cmpErr != nil {
		return nil, fmt.Errorf("ORDER BY: %w", cmpErr)
	}
	return &sliceIter{rows: rows}, nil
}

func drain(p plan, ctx *execCtx, outer *env) ([][]value.Value, error) {
	it, err := p.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	defer it.close()
	var rows [][]value.Value
	for {
		row, err := it.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			return rows, nil
		}
		rows = append(rows, row)
	}
}

type limitPlan struct {
	input         plan
	limit, offset evalFn
	limitText     string
	offsetText    string
}

func (p *limitPlan) columns() []string { return p.input.columns() }

func (p *limitPlan) open(ctx *execCtx, outer *env) (iter, error) {
	en := &env{outer: outer, ctx: ctx}
	n := int64(-1)
	if p.limit != nil {
		v, err := p.limit(en)
		if err != nil {
			return nil, err
		}
		if v.K != value.Int || v.I < 0 {
			return nil, fmt.Errorf("LIMIT must be a non-negative integer, not %s", v.Literal())
		}
		n = v.I
	}
	skip := int64(0)
	if p.offset != nil {
		v, err := p.offset(en)
		if err != nil {
			return nil, err
		}
		if v.K != value.Int || v.I < 0 {
			return nil, fmt.Errorf("OFFSET must be a non-negative integer, not %s", v.Literal())
		}
		skip = v.I
	}
	in, err := p.input.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &limitIter{in: in, left: n, skip: skip}, nil
}

type limitIter struct {
	in         iter
	left, skip int64
}

func (it *limitIter) next() ([]value.Value, error) {
	for it.skip > 0 {
		row, err := it.in.next()
		if row == nil || err != nil {
			return nil, err
		}
		it.skip--
	}
	if it.left == 0 {
		return nil, nil
	}
	row, err := it.in.next()
	if row != nil {
		it.left--
	}
	return row, err
}

func (it *limitIter) close() { it.in.close() }

type distinctPlan struct{ input plan }

func (p *distinctPlan) columns() []string { return p.input.columns() }

func (p *distinctPlan) open(ctx *execCtx, outer *env) (iter, error) {
	in, err := p.input.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &distinctIter{in: in, seen: map[string]bool{}}, nil
}

type distinctIter struct {
	in   iter
	seen map[string]bool
}

func (it *distinctIter) next() ([]value.Value, error) {
	for {
		row, err := it.in.next()
		if row == nil || err != nil {
			return nil, err
		}
		k := rowKey(row)
		if !it.seen[k] {
			it.seen[k] = true
			return row, nil
		}
	}
}

func (it *distinctIter) close() { it.in.close() }

// rowKey identifies a row for DISTINCT and set operations: NULLs are equal
// here, and 3 equals 3.0.
func rowKey(row []value.Value) string {
	var b []byte
	for _, v := range row {
		if v.K == value.Bool {
			v = value.NewInt(v.I)
		}
		b = value.AppendKey(b, v)
	}
	return string(b)
}

// compoundPlan is UNION [ALL], INTERSECT or EXCEPT.
type compoundPlan struct {
	op          string
	left, right plan
}

func (p *compoundPlan) columns() []string { return p.left.columns() }

func (p *compoundPlan) open(ctx *execCtx, outer *env) (iter, error) {
	left, err := drain(p.left, ctx, outer)
	if err != nil {
		return nil, err
	}
	right, err := drain(p.right, ctx, outer)
	if err != nil {
		return nil, err
	}
	var out [][]value.Value
	switch p.op {
	case "UNION ALL":
		out = append(left, right...)
	case "UNION":
		seen := map[string]bool{}
		for _, r := range append(left, right...) {
			if k := rowKey(r); !seen[k] {
				seen[k] = true
				out = append(out, r)
			}
		}
	case "INTERSECT", "EXCEPT":
		in := map[string]bool{}
		for _, r := range right {
			in[rowKey(r)] = true
		}
		seen := map[string]bool{}
		for _, r := range left {
			k := rowKey(r)
			if in[k] == (p.op == "INTERSECT") && !seen[k] {
				seen[k] = true
				out = append(out, r)
			}
		}
	}
	return &sliceIter{rows: out}, nil
}

// derivedPlan wraps a subquery in FROM, renaming its columns.
type derivedPlan struct {
	input plan
	alias string
	names []string
}

func (p *derivedPlan) columns() []string { return p.names }

func (p *derivedPlan) open(ctx *execCtx, outer *env) (iter, error) {
	return p.input.open(ctx, outer)
}

// ---- subqueries in expressions ----------------------------------------------------------

type subPlan struct {
	plan       plan
	correlated bool
}

type valueSet struct {
	keys    map[string]struct{}
	hasNull bool
}

// rows runs the subquery (at most limit rows; 0 for all). Uncorrelated
// subqueries run once per statement.
func (s *subPlan) rows(en *env, limit int) ([][]value.Value, error) {
	ctx := en.ctx
	if !s.correlated {
		if rows, ok := ctx.subRows[s]; ok {
			return rows, nil
		}
		limit = 0
	}
	it, err := s.plan.open(ctx, en)
	if err != nil {
		return nil, err
	}
	defer it.close()
	var rows [][]value.Value
	for limit == 0 || len(rows) < limit {
		row, err := it.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		rows = append(rows, row)
	}
	if !s.correlated {
		if ctx.subRows == nil {
			ctx.subRows = map[*subPlan][][]value.Value{}
		}
		ctx.subRows[s] = rows
	}
	return rows, nil
}

// set returns the subquery's single column as a set, for IN.
func (s *subPlan) set(en *env) (*valueSet, error) {
	if !s.correlated {
		if set, ok := en.ctx.subSets[s]; ok {
			return set, nil
		}
	}
	rows, err := s.rows(en, 0)
	if err != nil {
		return nil, err
	}
	set := &valueSet{keys: map[string]struct{}{}}
	for _, r := range rows {
		v := r[0]
		if v.IsNull() {
			set.hasNull = true
			continue
		}
		if v.K == value.Bool {
			v = value.NewInt(v.I)
		}
		set.keys[string(value.EncodeKey(v))] = struct{}{}
	}
	if !s.correlated {
		if en.ctx.subSets == nil {
			en.ctx.subSets = map[*subPlan]*valueSet{}
		}
		en.ctx.subSets[s] = set
	}
	return set, nil
}
