package engine

import (
	"fmt"
	"strings"

	"github.com/integral0909/cairn/internal/sql"
	"github.com/integral0909/cairn/internal/value"
)

// compiler plans statements against a catalog.
type compiler struct {
	cat      *Catalog
	maxParam int
	// holders are the subqueries being planned, innermost last.
	holders []*scope
}

// subquery plans a subquery nested in scope s.
func (c *compiler) subquery(sel *sql.Select, s *scope) (*subPlan, error) {
	root := &scope{parent: s}
	c.holders = append(c.holders, root)
	p, err := c.planSelectIn(sel, root)
	c.holders = c.holders[:len(c.holders)-1]
	if err != nil {
		return nil, err
	}
	return &subPlan{plan: p, correlated: root.correlated}, nil
}

// planSelect plans a top-level query.
func (c *compiler) planSelect(sel *sql.Select) (plan, error) {
	return c.planSelectIn(sel, &scope{})
}

// planSelectIn plans a query whose correlation is recorded on holder: the
// query's own scope gets holder.parent as its parent, and holder.correlated
// is set if the query refers to enclosing queries.
func (c *compiler) planSelectIn(sel *sql.Select, holder *scope) (plan, error) {
	if sel.Next == nil {
		return c.planCore(sel, holder, true)
	}
	// A compound: plan each part without the trailing ORDER BY / LIMIT,
	// which apply to the combined result.
	first := *sel
	first.OrderBy, first.Limit, first.Offset = nil, nil, nil
	first.CompoundOp, first.Next = "", nil
	result, err := c.planCore(&first, holder, false)
	if err != nil {
		return nil, err
	}
	op := sel.CompoundOp
	for part := sel.Next; part != nil; part = part.Next {
		core := *part
		core.CompoundOp, core.Next = "", nil
		if core.OrderBy != nil || core.Limit != nil {
			return nil, fmt.Errorf("ORDER BY and LIMIT go after the last part of a %s", op)
		}
		right, err := c.planCore(&core, holder, false)
		if err != nil {
			return nil, err
		}
		if len(right.columns()) != len(result.columns()) {
			return nil, fmt.Errorf("each part of a %s must have the same number of columns (%d and %d)", op, len(result.columns()), len(right.columns()))
		}
		result = &compoundPlan{op: op, left: result, right: right}
		op = part.CompoundOp
	}
	// ORDER BY on a compound can only name output columns.
	names := result.columns()
	if len(sel.OrderBy) > 0 {
		var keys []sortKey
		var texts []string
		for _, item := range sel.OrderBy {
			col := -1
			switch x := item.Expr.(type) {
			case *sql.Literal:
				if x.Value.K == value.Int && x.Value.I >= 1 && int(x.Value.I) <= len(names) {
					col = int(x.Value.I) - 1
				}
			case *sql.ColumnRef:
				for i, n := range names {
					if x.Table == "" && strings.EqualFold(n, x.Name) {
						col = i
						break
					}
				}
			}
			if col < 0 {
				return nil, fmt.Errorf("ORDER BY on a %s must name an output column or its position", sel.CompoundOp)
			}
			keys = append(keys, orderKey(item, col))
			texts = append(texts, orderText(item))
		}
		result = &sortPlan{input: result, keys: keys, texts: texts}
	}
	return c.limit(result, sel, holder)
}

func orderKey(item sql.OrderItem, col int) sortKey {
	k := sortKey{col: col, desc: item.Desc, nullsFirst: !item.Desc}
	if item.NullsFirst != nil {
		k.nullsFirst = *item.NullsFirst
	}
	return k
}

func orderText(item sql.OrderItem) string {
	t := sql.FormatExpr(item.Expr)
	if item.Desc {
		t += " DESC"
	}
	return t
}

func (c *compiler) limit(p plan, sel *sql.Select, holder *scope) (plan, error) {
	if sel.Limit == nil && sel.Offset == nil {
		return p, nil
	}
	empty := &scope{parent: holder.parent}
	lp := &limitPlan{input: p}
	var err error
	if sel.Limit != nil {
		if lp.limit, err = c.compileExpr(sel.Limit, empty); err != nil {
			return nil, err
		}
		lp.limitText = sql.FormatExpr(sel.Limit)
	}
	if sel.Offset != nil {
		if lp.offset, err = c.compileExpr(sel.Offset, empty); err != nil {
			return nil, err
		}
		lp.offsetText = sql.FormatExpr(sel.Offset)
	}
	return lp, nil
}

// planCore plans one SELECT (no compound parts).
func (c *compiler) planCore(sel *sql.Select, holder *scope, withOrder bool) (plan, error) {
	if sel.Where != nil && hasAggregate(sel.Where) {
		return nil, fmt.Errorf("aggregates are not allowed in WHERE; use HAVING")
	}
	input, sc, err := c.planFrom(sel, holder)
	if err != nil {
		return nil, err
	}
	// Expand the select list.
	var items []sql.ResultCol
	for _, rc := range sel.Columns {
		if !rc.Star {
			items = append(items, rc)
			continue
		}
		matched := false
		for _, src := range sc.sources {
			if rc.Table != "" && !strings.EqualFold(src.alias, rc.Table) {
				continue
			}
			matched = true
			for _, col := range src.columns {
				items = append(items, sql.ResultCol{Expr: &sql.ColumnRef{Table: src.alias, Name: col}, Alias: col})
			}
		}
		if rc.Table != "" && !matched {
			return nil, fmt.Errorf("no table called %s in this query", rc.Table)
		}
		if rc.Table == "" && len(sc.sources) == 0 {
			return nil, fmt.Errorf("SELECT * needs a FROM clause")
		}
	}

	aggregated := len(sel.GroupBy) > 0 || sel.Having != nil
	for _, it := range items {
		if hasAggregate(it.Expr) {
			aggregated = true
		}
	}
	if withOrder {
		for _, o := range sel.OrderBy {
			if hasAggregate(o.Expr) {
				aggregated = true
			}
		}
	}

	outScope := sc
	if aggregated {
		ap, as, err := c.planAggregate(input, sel, sc)
		if err != nil {
			return nil, err
		}
		input, outScope = ap, as
		if sel.Having != nil {
			h, err := c.compileExpr(sel.Having, outScope)
			if err != nil {
				return nil, fmt.Errorf("HAVING: %w", err)
			}
			input = &filterPlan{input: input, pred: h, text: sql.FormatExpr(sel.Having)}
		}
	}

	proj := &projectPlan{input: input}
	for _, it := range items {
		f, err := c.compileExpr(it.Expr, outScope)
		if err != nil {
			return nil, err
		}
		proj.exprs = append(proj.exprs, f)
		proj.names = append(proj.names, resultName(it))
		proj.texts = append(proj.texts, sql.FormatExpr(it.Expr))
		outScope.outputs = append(outScope.outputs, outputCol{name: resultName(it), expr: it.Expr})
	}
	visible := len(proj.exprs)

	// ORDER BY: output columns by position or alias, otherwise any
	// expression over the input, carried as a hidden column.
	var keys []sortKey
	var texts []string
	if withOrder {
		for _, item := range sel.OrderBy {
			col := -1
			switch x := item.Expr.(type) {
			case *sql.Literal:
				if x.Value.K == value.Int {
					if x.Value.I < 1 || int(x.Value.I) > visible {
						return nil, fmt.Errorf("ORDER BY position %d is out of range (1 to %d)", x.Value.I, visible)
					}
					col = int(x.Value.I) - 1
				}
			case *sql.ColumnRef:
				if x.Table == "" {
					for i := 0; i < visible; i++ {
						if strings.EqualFold(proj.names[i], x.Name) && items[i].Alias != "" {
							col = i
							break
						}
					}
				}
			}
			if col < 0 {
				// The same expression as an output column?
				text := sql.FormatExpr(item.Expr)
				for i := 0; i < visible; i++ {
					if proj.texts[i] == text {
						col = i
						break
					}
				}
			}
			if col < 0 {
				if sel.Distinct {
					return nil, fmt.Errorf("with SELECT DISTINCT, ORDER BY expressions must appear in the select list")
				}
				f, err := c.compileExpr(item.Expr, outScope)
				if err != nil {
					return nil, fmt.Errorf("ORDER BY: %w", err)
				}
				proj.exprs = append(proj.exprs, f)
				proj.names = append(proj.names, "")
				proj.texts = append(proj.texts, sql.FormatExpr(item.Expr))
				col = len(proj.exprs) - 1
			}
			keys = append(keys, orderKey(item, col))
			texts = append(texts, orderText(item))
		}
	}

	var result plan = proj
	if sel.Distinct {
		result = &distinctPlan{input: result}
	}
	if len(keys) > 0 {
		result = &sortPlan{input: result, keys: keys, texts: texts}
	}
	if len(proj.exprs) > visible {
		result = &trimPlan{input: result, width: visible, names: proj.names[:visible]}
	}
	if withOrder {
		return c.limit(result, sel, holder)
	}
	return result, nil
}

// trimPlan drops hidden ORDER BY columns.
type trimPlan struct {
	input plan
	width int
	names []string
}

func (p *trimPlan) columns() []string { return p.names }

func (p *trimPlan) open(ctx *execCtx, outer *env) (iter, error) {
	in, err := p.input.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &trimIter{in: in, width: p.width}, nil
}

type trimIter struct {
	in    iter
	width int
}

func (it *trimIter) next() ([]value.Value, error) {
	row, err := it.in.next()
	if row == nil || err != nil {
		return nil, err
	}
	return row[:it.width], nil
}

func (it *trimIter) close() { it.in.close() }

func resultName(rc sql.ResultCol) string {
	if rc.Alias != "" {
		return rc.Alias
	}
	if ref, ok := rc.Expr.(*sql.ColumnRef); ok {
		return ref.Name
	}
	return sql.FormatExpr(rc.Expr)
}

// ---- FROM and joins --------------------------------------------------------------

// unit is one item joined in a FROM clause.
type unit struct {
	plan    plan // for subqueries and nested joins; nil for base tables
	table   *Table
	alias   string
	sources []*source
	width   int
	offset  int
	kind    sql.JoinKind // how it joins to the units before it
	on      []sql.Expr   // its ON conditions
	using   []string
}

// conjunct is one AND-ed condition and the units it refers to.
type conjunct struct {
	expr      sql.Expr
	refs      uint64 // bit i: refers to unit i
	fromWhere bool
	join      int // for ON conditions: the unit whose join it belongs to
	used      bool
}

func splitAnd(e sql.Expr, out []sql.Expr) []sql.Expr {
	if b, ok := e.(*sql.Binary); ok && b.Op == "AND" {
		return splitAnd(b.R, splitAnd(b.L, out))
	}
	if e != nil {
		out = append(out, e)
	}
	return out
}

func (c *compiler) planFrom(sel *sql.Select, holder *scope) (plan, *scope, error) {
	sc := &scope{parent: holder.parent}
	if sel.From == nil {
		if sel.Where != nil {
			var conds []*conjunct
			for _, e := range splitAnd(sel.Where, nil) {
				conds = append(conds, &conjunct{expr: e})
			}
			w, text, err := c.compileConjuncts(conds, sc)
			if err != nil {
				return nil, nil, err
			}
			return &filterPlan{input: oneRowPlan{}, pred: w, text: text}, sc, nil
		}
		return oneRowPlan{}, sc, nil
	}
	var units []*unit
	if err := c.flatten(sel.From, sql.InnerJoin, &units, holder); err != nil {
		return nil, nil, err
	}
	if len(units) > 64 {
		return nil, nil, fmt.Errorf("a query can join at most 64 tables")
	}
	offset := 0
	for _, u := range units {
		u.offset = offset
		for _, s := range u.sources {
			s.offset += offset
			for _, other := range sc.sources {
				if strings.EqualFold(other.alias, s.alias) {
					return nil, nil, fmt.Errorf("table name %s is used twice in FROM; give one an alias", s.alias)
				}
			}
			sc.sources = append(sc.sources, s)
		}
		offset += u.width
	}

	// Gather conditions.
	var conds []*conjunct
	for _, e := range splitAnd(sel.Where, nil) {
		conds = append(conds, &conjunct{expr: e, fromWhere: true})
	}
	for i, u := range units {
		for _, name := range u.using {
			if i == 0 {
				return nil, nil, fmt.Errorf("USING needs a table on each side")
			}
			left := ""
			for _, prev := range units[:i] {
				for _, s := range prev.sources {
					for _, col := range s.columns {
						if strings.EqualFold(col, name) {
							if left != "" {
								return nil, nil, fmt.Errorf("USING column %s is ambiguous on the left side", name)
							}
							left = s.alias
						}
					}
				}
			}
			if left == "" {
				return nil, nil, fmt.Errorf("USING column %s is not in the tables on the left", name)
			}
			u.on = append(u.on, &sql.Binary{Op: "=", L: &sql.ColumnRef{Table: left, Name: name}, R: &sql.ColumnRef{Table: u.alias, Name: name}})
		}
		for _, e := range u.on {
			for _, part := range splitAnd(e, nil) {
				if hasAggregate(part) {
					return nil, nil, fmt.Errorf("aggregates are not allowed in ON")
				}
				conds = append(conds, &conjunct{expr: part, join: i})
			}
		}
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
		if refErr != nil {
			return nil, nil, refErr
		}
		if !cj.fromWhere && cj.refs>>(uint(cj.join)+1) != 0 {
			return nil, nil, fmt.Errorf("ON condition %s refers to a table joined later", sql.FormatExpr(cj.expr))
		}
	}

	// Build left to right.
	var result plan
	joined := &scope{parent: sc.parent}
	for i, u := range units {
		upto := uint64(1)<<uint(i+1) - 1
		mine := uint64(1) << uint(i)
		if i == 0 {
			var here []*conjunct
			for _, cj := range conds {
				if cj.fromWhere && cj.refs&^upto == 0 {
					here = append(here, cj)
					cj.used = true
				}
			}
			p, err := c.accessPlan(u, 0, here, nil, sc.parent, refs)
			if err != nil {
				return nil, nil, err
			}
			result = p
			joined.sources = append(joined.sources, u.sources...)
			continue
		}
		left := u.kind == sql.LeftJoin
		// Conditions that decide this join step.
		var step, after []*conjunct
		for _, cj := range conds {
			if cj.used {
				continue
			}
			switch {
			case !cj.fromWhere && cj.join == i:
				step = append(step, cj)
				cj.used = true
			case cj.fromWhere && cj.refs&^upto == 0 && (cj.refs&mine != 0 || cj.refs == 0):
				if left {
					after = append(after, cj) // must see the NULL-extended rows
				} else {
					step = append(step, cj)
				}
				cj.used = true
			}
		}
		earlier := joined // scope of everything joined so far
		p, err := c.joinStep(result, earlier, u, i, step, left, sc, refs)
		if err != nil {
			return nil, nil, err
		}
		result = p
		joined = &scope{parent: sc.parent, sources: append(append([]*source(nil), earlier.sources...), u.sources...)}
		if len(after) > 0 {
			pred, text, err := c.compileConjuncts(after, joined)
			if err != nil {
				return nil, nil, err
			}
			result = &filterPlan{input: result, pred: pred, text: text}
		}
	}
	for _, cj := range conds {
		if !cj.used {
			return nil, nil, fmt.Errorf("internal error: condition %s was not placed", sql.FormatExpr(cj.expr))
		}
	}
	return result, sc, nil
}

// flatten turns a FROM tree into left-to-right units.
func (c *compiler) flatten(item sql.FromItem, kind sql.JoinKind, units *[]*unit, holder *scope) error {
	switch x := item.(type) {
	case *sql.Join:
		if err := c.flatten(x.Left, kind, units, holder); err != nil {
			return err
		}
		start := len(*units)
		if err := c.flatten(x.Right, x.Kind, units, holder); err != nil {
			return err
		}
		if len(*units)-start != 1 {
			return fmt.Errorf("parenthesised joins on the right of a JOIN are not supported; reorder the tables")
		}
		u := (*units)[start]
		if x.On != nil {
			u.on = append(u.on, x.On)
		}
		u.using = x.Using
		return nil
	case *sql.TableRef:
		t := c.cat.Table(x.Name)
		if t == nil {
			return fmt.Errorf("no such table: %s", x.Name)
		}
		alias := x.Alias
		if alias == "" {
			alias = t.Name
		}
		*units = append(*units, &unit{table: t, alias: alias, kind: kind, width: len(t.Columns),
			sources: []*source{{alias: alias, table: t, columns: columnNames(t)}}})
		return nil
	case *sql.SubqueryRef:
		// A subquery in FROM cannot see sibling tables, only enclosing
		// queries (whose references reach the holders already).
		inner := &scope{parent: holder.parent}
		p, err := c.planSelectIn(x.Select, inner)
		if err != nil {
			return err
		}
		names := p.columns()
		*units = append(*units, &unit{plan: &derivedPlan{input: p, alias: x.Alias, names: names}, alias: x.Alias, kind: kind,
			width: len(names), sources: []*source{{alias: x.Alias, columns: names}}})
		return nil
	}
	return fmt.Errorf("unsupported FROM item %T", item)
}

// refsOf finds which units an expression refers to. Subqueries count as
// referring to everything, which only delays where they are evaluated.
func (c *compiler) refsOf(e sql.Expr, sc *scope, units []*unit) (uint64, error) {
	var refs uint64
	var err error
	walkExpr(e, func(x sql.Expr) {
		switch r := x.(type) {
		case *sql.ColumnRef:
			level := &scope{sources: sc.sources}
			_, idx, _, rerr := level.resolve(r)
			if rerr != nil {
				if strings.Contains(rerr.Error(), "ambiguous") {
					err = rerr
				}
				return // a column of an enclosing query
			}
			for i, u := range units {
				if idx >= u.offset && idx < u.offset+u.width {
					refs |= 1 << uint(i)
				}
			}
		case *sql.Subquery, *sql.Exists:
			refs = ^uint64(0) >> (64 - uint(len(units)))
		case *sql.In:
			if r.Select != nil {
				refs = ^uint64(0) >> (64 - uint(len(units)))
			}
		}
	})
	return refs, err
}

func (c *compiler) compileConjuncts(conds []*conjunct, s *scope) (evalFn, string, error) {
	var fns []evalFn
	var texts []string
	for _, cj := range conds {
		f, err := c.compileExpr(cj.expr, s)
		if err != nil {
			return nil, "", err
		}
		fns = append(fns, f)
		texts = append(texts, sql.FormatExpr(cj.expr))
	}
	if len(fns) == 0 {
		return nil, "", nil
	}
	return andAll(fns), strings.Join(texts, " AND "), nil
}

func andAll(fns []evalFn) evalFn {
	if len(fns) == 1 {
		f := fns[0]
		return func(en *env) (value.Value, error) {
			v, err := f(en)
			if err != nil {
				return v, err
			}
			return v, checkBoolean(v, "WHERE")
		}
	}
	return func(en *env) (value.Value, error) {
		result := value.NewBool(true)
		for _, f := range fns {
			v, err := f(en)
			if err != nil {
				return v, err
			}
			if err := checkBoolean(v, "WHERE"); err != nil {
				return v, err
			}
			if t, k := truth(v); k && !t {
				return value.NewBool(false), nil
			}
			result = and3(result, v)
		}
		return result, nil
	}
}

// joinStep joins unit i onto the plan of the units before it. refs maps an
// expression to the units it mentions.
func (c *compiler) joinStep(left plan, earlier *scope, u *unit, i int, conds []*conjunct, leftJoin bool, sc *scope, refs func(sql.Expr) uint64) (plan, error) {
	leftWidth := earlier.width()
	mine := uint64(1) << uint(i)
	before := mine - 1

	// First choice: an index (or rowid) lookup on this table keyed by
	// columns of the outer row: an index nested loop join.
	inner, err := c.accessPlan(u, i, conds, earlier, sc.parent, refs)
	if err != nil {
		return nil, err
	}
	if usesOuter(inner) {
		return &nestedLoopPlan{left: left, right: inner, leftWidth: leftWidth, rightWidth: u.width, leftJoin: leftJoin, lookup: true}, nil
	}

	// Second: a hash join on equalities between earlier units and this one.
	var lkeys, rkeys []sql.Expr
	var own, residual []*conjunct
	for _, cj := range conds {
		if b, ok := cj.expr.(*sql.Binary); ok && b.Op == "=" {
			lr, rr := refs(b.L), refs(b.R)
			switch {
			case lr != 0 && lr&^before == 0 && rr == mine:
				lkeys, rkeys = append(lkeys, b.L), append(rkeys, b.R)
				continue
			case rr != 0 && rr&^before == 0 && lr == mine:
				lkeys, rkeys = append(lkeys, b.R), append(rkeys, b.L)
				continue
			}
		}
		if cj.refs&^mine == 0 {
			own = append(own, cj)
		} else {
			residual = append(residual, cj)
		}
	}
	if len(lkeys) > 0 {
		right, err := c.accessPlan(u, i, own, nil, sc.parent, refs)
		if err != nil {
			return nil, err
		}
		hj := &hashJoinPlan{left: left, right: right, leftWidth: leftWidth, rightWidth: u.width, leftJoin: leftJoin}
		rightScope := &scope{sources: shifted(u.sources, -u.offset), parent: sc.parent}
		for k := range lkeys {
			lf, err := c.compileExpr(lkeys[k], earlier)
			if err != nil {
				return nil, err
			}
			rf, err := c.compileExpr(rkeys[k], rightScope)
			if err != nil {
				return nil, err
			}
			hj.leftKeys, hj.rightKeys = append(hj.leftKeys, lf), append(hj.rightKeys, rf)
			hj.keyText = append(hj.keyText, sql.FormatExpr(lkeys[k])+" = "+sql.FormatExpr(rkeys[k]))
		}
		if len(residual) > 0 {
			full := &scope{parent: sc.parent, sources: append(append([]*source(nil), earlier.sources...), u.sources...)}
			if hj.residual, hj.residText, err = c.compileConjuncts(residual, full); err != nil {
				return nil, err
			}
		}
		return hj, nil
	}

	// Otherwise a plain nested loop, checking every condition on the inner
	// side, which sees the outer row.
	return &nestedLoopPlan{left: left, right: inner, leftWidth: leftWidth, rightWidth: u.width, leftJoin: leftJoin}, nil
}

func shifted(srcs []*source, by int) []*source {
	out := make([]*source, len(srcs))
	for i, s := range srcs {
		c := *s
		c.offset += by
		out[i] = &c
	}
	return out
}

// usesOuter reports whether an access plan's lookup keys depend on the
// outer row.
func usesOuter(p plan) bool {
	switch x := p.(type) {
	case *scanPlan:
		return x.outerKeys
	case *indexPlan:
		return x.outerKeys
	}
	return false
}
