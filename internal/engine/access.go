package engine

import (
	"strings"

	"github.com/integral0909/cairn/internal/sql"
)

// A candidate bound on one column: column op expr, where expr does not
// mention this table.
type candidate struct {
	op    string // = < <= > >=
	expr  sql.Expr
	outer bool // expr mentions earlier tables of the join
}

// accessPlan chooses how to read unit i: a rowid lookup or range, an index
// lookup or range, or a full scan, given the conditions that apply to it.
// earlier is the scope of tables already joined (nil when none are visible
// to this side). Every condition is also kept as a filter, so the access
// path only has to narrow the rows down, never decide them exactly.
func (c *compiler) accessPlan(u *unit, i int, conds []*conjunct, earlier *scope, parent *scope, refs func(sql.Expr) uint64) (plan, error) {
	lateral := parent
	if earlier != nil {
		lateral = earlier
	}
	own := &scope{sources: shifted(u.sources, -u.offset), parent: lateral}
	pred, text, err := c.compileConjuncts(conds, own)
	if err != nil {
		return nil, err
	}
	if u.table == nil {
		var p plan = u.plan
		if earlier != nil {
			// The subquery was planned against the enclosing query; skip the
			// outer-row level a nested loop adds.
			p = &liftPlan{input: p}
		}
		if pred != nil {
			p = &filterPlan{input: p, pred: pred, text: text}
		}
		return p, nil
	}
	t := u.table
	mine := uint64(1) << uint(i)

	// Collect bounds per column.
	bounds := map[int][]candidate{}
	add := func(col int, op string, other sql.Expr) {
		r := refs(other)
		if r&mine != 0 { // mentions this table (subqueries mention all)
			return
		}
		bounds[col] = append(bounds[col], candidate{op: op, expr: other, outer: r != 0})
	}
	colOf := func(e sql.Expr) int {
		ref, ok := e.(*sql.ColumnRef)
		if !ok {
			return -1
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, u.alias) {
			return -1
		}
		if refs(ref) != mine {
			return -1
		}
		return t.column(ref.Name)
	}
	flip := map[string]string{"=": "=", "<": ">", "<=": ">=", ">": "<", ">=": "<="}
	for _, cj := range conds {
		switch x := cj.expr.(type) {
		case *sql.Binary:
			if _, ok := flip[x.Op]; !ok {
				continue
			}
			if col := colOf(x.L); col >= 0 {
				add(col, x.Op, x.R)
			} else if col := colOf(x.R); col >= 0 {
				add(col, flip[x.Op], x.L)
			}
		case *sql.Between:
			if col := colOf(x.X); col >= 0 && !x.Not {
				add(col, ">=", x.Lo)
				add(col, "<=", x.Hi)
			}
		}
	}

	boundScope := &scope{parent: lateral}
	compileBound := func(cd candidate) (*bound, error) {
		f, err := c.compileExpr(cd.expr, boundScope)
		if err != nil {
			return nil, err
		}
		return &bound{fn: f, inclusive: cd.op == "=" || cd.op == "<=" || cd.op == ">=", text: sql.FormatExpr(cd.expr)}, nil
	}
	pick := func(col int) (eq *candidate, lo, hi *candidate) {
		for k := range bounds[col] {
			cd := &bounds[col][k]
			switch cd.op {
			case "=":
				if eq == nil {
					eq = cd
				}
			case ">", ">=":
				if lo == nil {
					lo = cd
				}
			case "<", "<=":
				if hi == nil {
					hi = cd
				}
			}
		}
		return
	}

	// Score each way in. Higher is better.
	bestScore := 0
	var best plan
	if t.RowidCol >= 0 {
		eq, lo, hi := pick(t.RowidCol)
		sp := &scanPlan{table: t, alias: u.alias, filter: pred, filterText: text}
		score := 0
		switch {
		case eq != nil:
			b, err := compileBound(*eq)
			if err != nil {
				return nil, err
			}
			sp.eq, sp.eqText, sp.outerKeys = b.fn, b.text, eq.outer
			score = 1000
		case lo != nil || hi != nil:
			score = 40
			for _, cd := range []*candidate{lo, hi} {
				if cd == nil {
					continue
				}
				b, err := compileBound(*cd)
				if err != nil {
					return nil, err
				}
				b.inclusive = cd.op == ">=" || cd.op == "<="
				if cd.op == ">" || cd.op == ">=" {
					sp.lo = b
				} else {
					sp.hi = b
				}
				sp.outerKeys = sp.outerKeys || cd.outer
				score += 10
			}
		}
		if score > bestScore {
			bestScore, best = score, sp
		}
	}
	for _, ix := range t.Indexes {
		ip := &indexPlan{table: t, alias: u.alias, index: ix, filter: pred, filterText: text}
		score := 0
		for _, col := range ix.Columns {
			eq, lo, hi := pick(col)
			if eq != nil {
				b, err := compileBound(*eq)
				if err != nil {
					return nil, err
				}
				ip.eq = append(ip.eq, b.fn)
				ip.eqText = append(ip.eqText, t.Columns[col].Name+" = "+b.text)
				ip.outerKeys = ip.outerKeys || eq.outer
				score += 100
				continue
			}
			for _, cd := range []*candidate{lo, hi} {
				if cd == nil {
					continue
				}
				b, err := compileBound(*cd)
				if err != nil {
					return nil, err
				}
				b.inclusive = cd.op == ">=" || cd.op == "<="
				b.text = t.Columns[col].Name + " " + cd.op + " " + b.text
				if cd.op == ">" || cd.op == ">=" {
					ip.lo = b
				} else {
					ip.hi = b
				}
				ip.outerKeys = ip.outerKeys || cd.outer
				score += 15
			}
			break
		}
		if ix.Unique && len(ip.eq) == len(ix.Columns) {
			score += 500
		}
		if score > bestScore {
			bestScore, best = score, ip
		}
	}
	if best != nil {
		return best, nil
	}
	return &scanPlan{table: t, alias: u.alias, filter: pred, filterText: text}, nil
}

// liftPlan opens its input one environment level up.
type liftPlan struct{ input plan }

func (p *liftPlan) columns() []string { return p.input.columns() }

func (p *liftPlan) open(ctx *execCtx, outer *env) (iter, error) {
	if outer != nil {
		outer = outer.outer
	}
	return p.input.open(ctx, outer)
}
