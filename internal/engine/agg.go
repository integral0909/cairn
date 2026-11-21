package engine

import (
	"fmt"
	"math"
	"strings"

	"github.com/integral0909/cairn/internal/sql"
	"github.com/integral0909/cairn/internal/value"
)

// aggScope compiles expressions evaluated once per group. Their row is the
// group's key values followed by the aggregate results.
type aggScope struct {
	input      *scope
	groupTexts []string
	groupCols  map[int]int // input column -> group slot, for plain column groups
	aggs       []*aggCall
}

type aggCall struct {
	name     string
	args     []evalFn
	star     bool
	distinct bool
	text     string
}

func (a *aggScope) slot(i int) evalFn {
	return func(en *env) (value.Value, error) { return en.row[i], nil }
}

// lookup handles the expressions that mean something per group: group-by
// expressions, aggregate calls and columns of enclosing queries.
func (a *aggScope) lookup(c *compiler, e sql.Expr) (evalFn, bool, error) {
	text := sql.FormatExpr(e)
	for i, g := range a.groupTexts {
		if g == text {
			return a.slot(i), true, nil
		}
	}
	if ref, ok := e.(*sql.ColumnRef); ok {
		depth, idx, found, err := a.input.resolve(ref)
		if err != nil {
			return nil, true, err
		}
		c.noteResolved(found)
		if depth > 0 {
			return func(en *env) (value.Value, error) {
				for i := 0; i < depth; i++ {
					en = en.outer
				}
				return en.row[idx], nil
			}, true, nil
		}
		if slot, ok := a.groupCols[idx]; ok {
			return a.slot(slot), true, nil
		}
		return nil, true, fmt.Errorf("column %s must appear in GROUP BY or be used in an aggregate", text)
	}
	f, ok := e.(*sql.FuncCall)
	if !ok || !isAggregate(f) {
		return nil, false, nil
	}
	for j, ag := range a.aggs {
		if ag.text == text {
			return a.slot(len(a.groupTexts) + j), true, nil
		}
	}
	call := &aggCall{name: f.Name, star: f.Star, distinct: f.Distinct, text: text}
	switch {
	case f.Star && f.Name != "count":
		return nil, true, fmt.Errorf("only count() takes *")
	case f.Name == "group_concat" && (len(f.Args) < 1 || len(f.Args) > 2):
		return nil, true, fmt.Errorf("group_concat() takes one or two arguments")
	case f.Name == "string_agg" && len(f.Args) != 2:
		return nil, true, fmt.Errorf("string_agg() takes two arguments")
	case !f.Star && f.Name != "group_concat" && f.Name != "string_agg" && len(f.Args) != 1:
		return nil, true, fmt.Errorf("%s() takes one argument", f.Name)
	}
	for _, arg := range f.Args {
		if hasAggregate(arg) {
			return nil, true, fmt.Errorf("aggregate calls cannot be nested")
		}
		fn, err := c.compileExpr(arg, a.input)
		if err != nil {
			return nil, true, err
		}
		call.args = append(call.args, fn)
	}
	a.aggs = append(a.aggs, call)
	return a.slot(len(a.groupTexts) + len(a.aggs) - 1), true, nil
}

// planAggregate groups input rows. The aggregates themselves are collected
// later, as the select list, HAVING and ORDER BY are compiled.
func (c *compiler) planAggregate(input plan, sel *sql.Select, in *scope) (*aggPlan, *scope, error) {
	as := &aggScope{input: in, groupCols: map[int]int{}}
	ap := &aggPlan{input: input, as: as}
	for i, g := range sel.GroupBy {
		if hasAggregate(g) {
			return nil, nil, fmt.Errorf("aggregates are not allowed in GROUP BY")
		}
		// GROUP BY 2 means the second select-list item.
		if lit, ok := g.(*sql.Literal); ok && lit.Value.K == value.Int {
			n := int(lit.Value.I)
			if n < 1 || n > len(sel.Columns) || sel.Columns[n-1].Star {
				return nil, nil, fmt.Errorf("GROUP BY position %d is out of range", n)
			}
			g = sel.Columns[n-1].Expr
		}
		f, err := c.compileExpr(g, in)
		if err != nil {
			return nil, nil, fmt.Errorf("GROUP BY: %w", err)
		}
		ap.groups = append(ap.groups, f)
		as.groupTexts = append(as.groupTexts, sql.FormatExpr(g))
		if ref, ok := g.(*sql.ColumnRef); ok {
			if d, idx, _, err := in.resolve(ref); err == nil && d == 0 {
				as.groupCols[idx] = i
			}
		}
	}
	return ap, &scope{agg: as, parent: in.parent}, nil
}

type aggPlan struct {
	input  plan
	groups []evalFn
	as     *aggScope
}

func (p *aggPlan) columns() []string {
	out := append([]string(nil), p.as.groupTexts...)
	for _, a := range p.as.aggs {
		out = append(out, a.text)
	}
	return out
}

func (p *aggPlan) open(ctx *execCtx, outer *env) (iter, error) {
	in, err := p.input.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	defer in.close()
	type group struct {
		key  []value.Value
		accs []accumulator
	}
	groups := map[string]*group{}
	var order []*group
	newGroup := func(key []value.Value) *group {
		g := &group{key: key}
		for _, a := range p.as.aggs {
			g.accs = append(g.accs, newAccumulator(a))
		}
		return g
	}
	for {
		row, err := in.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		en := &env{row: row, outer: outer, ctx: ctx}
		key := make([]value.Value, len(p.groups))
		for i, f := range p.groups {
			if key[i], err = f(en); err != nil {
				return nil, err
			}
		}
		k := rowKey(key)
		g := groups[k]
		if g == nil {
			g = newGroup(key)
			groups[k] = g
			order = append(order, g)
		}
		for i, a := range p.as.aggs {
			var args []value.Value
			for _, f := range a.args {
				v, err := f(en)
				if err != nil {
					return nil, err
				}
				args = append(args, v)
			}
			if err := g.accs[i].add(args); err != nil {
				return nil, fmt.Errorf("%s: %w", a.text, err)
			}
		}
	}
	// Without GROUP BY, an empty input still gives one row (count 0).
	if len(p.groups) == 0 && len(order) == 0 {
		order = append(order, newGroup(nil))
	}
	rows := make([][]value.Value, len(order))
	for i, g := range order {
		row := append([]value.Value(nil), g.key...)
		for _, acc := range g.accs {
			row = append(row, acc.result())
		}
		rows[i] = row
	}
	return &sliceIter{rows: rows}, nil
}

type accumulator interface {
	add(args []value.Value) error
	result() value.Value
}

func newAccumulator(a *aggCall) accumulator {
	var acc accumulator
	switch a.name {
	case "count":
		acc = &countAcc{star: a.star}
	case "sum":
		acc = &sumAcc{}
	case "total":
		acc = &sumAcc{total: true}
	case "avg":
		acc = &avgAcc{}
	case "min":
		acc = &extremeAcc{dir: -1}
	case "max":
		acc = &extremeAcc{dir: 1}
	case "group_concat", "string_agg":
		acc = &concatAcc{}
	case "bool_and":
		acc = &boolAcc{and: true}
	case "bool_or":
		acc = &boolAcc{}
	}
	if a.distinct {
		acc = &distinctAcc{inner: acc, seen: map[string]bool{}}
	}
	return acc
}

type countAcc struct {
	star bool
	n    int64
}

func (a *countAcc) add(args []value.Value) error {
	if a.star || !args[0].IsNull() {
		a.n++
	}
	return nil
}
func (a *countAcc) result() value.Value { return value.NewInt(a.n) }

type sumAcc struct {
	total   bool
	seen    bool
	isFloat bool
	i       int64
	f       float64
}

func (a *sumAcc) add(args []value.Value) error {
	v := args[0]
	if v.IsNull() {
		return nil
	}
	if !v.IsNumeric() {
		return fmt.Errorf("cannot sum %s %s", v.K, v.Literal())
	}
	a.seen = true
	if v.K == value.Real || a.isFloat || a.total {
		if !a.isFloat {
			a.isFloat, a.f = true, float64(a.i)
		}
		a.f += v.Float()
		return nil
	}
	s := a.i + v.I
	if (s > a.i) != (v.I > 0) {
		return errOverflow
	}
	a.i = s
	return nil
}

func (a *sumAcc) result() value.Value {
	switch {
	case a.total && !a.seen:
		return value.NewReal(0)
	case !a.seen:
		return value.Value{}
	case a.isFloat:
		return value.NewReal(a.f)
	}
	return value.NewInt(a.i)
}

type avgAcc struct {
	n   int64
	sum float64
	// Kahan compensation keeps long averages accurate.
	comp float64
}

func (a *avgAcc) add(args []value.Value) error {
	v := args[0]
	if v.IsNull() {
		return nil
	}
	if !v.IsNumeric() {
		return fmt.Errorf("cannot average %s %s", v.K, v.Literal())
	}
	y := v.Float() - a.comp
	t := a.sum + y
	a.comp = (t - a.sum) - y
	a.sum = t
	a.n++
	return nil
}

func (a *avgAcc) result() value.Value {
	if a.n == 0 {
		return value.Value{}
	}
	r := a.sum / float64(a.n)
	if math.IsInf(r, 0) {
		return value.NewReal(r)
	}
	return value.NewReal(r)
}

type extremeAcc struct {
	dir  int
	best value.Value
}

func (a *extremeAcc) add(args []value.Value) error {
	v := args[0]
	if v.IsNull() {
		return nil
	}
	if a.best.IsNull() {
		a.best = v
		return nil
	}
	c, err := compareSQL(v, a.best)
	if err != nil {
		return err
	}
	if c*a.dir > 0 {
		a.best = v
	}
	return nil
}
func (a *extremeAcc) result() value.Value { return a.best }

type concatAcc struct {
	parts []string
	any   bool
}

func (a *concatAcc) add(args []value.Value) error {
	if args[0].IsNull() {
		return nil
	}
	sep := ","
	if len(args) > 1 {
		sep = textOf(args[1])
	}
	if a.any {
		a.parts = append(a.parts, sep)
	}
	a.parts = append(a.parts, textOf(args[0]))
	a.any = true
	return nil
}

func (a *concatAcc) result() value.Value {
	if !a.any {
		return value.Value{}
	}
	return value.NewText(strings.Join(a.parts, ""))
}

type boolAcc struct {
	and  bool
	seen bool
	v    bool
}

func (a *boolAcc) add(args []value.Value) error {
	t, k := truth(args[0])
	if !k {
		return checkBoolean(args[0], "bool aggregate")
	}
	if !a.seen {
		a.seen, a.v = true, t
	} else if a.and {
		a.v = a.v && t
	} else {
		a.v = a.v || t
	}
	return nil
}

func (a *boolAcc) result() value.Value {
	if !a.seen {
		return value.Value{}
	}
	return value.NewBool(a.v)
}

type distinctAcc struct {
	inner accumulator
	seen  map[string]bool
}

func (a *distinctAcc) add(args []value.Value) error {
	k := rowKey(args[:1])
	if a.seen[k] {
		return nil
	}
	a.seen[k] = true
	return a.inner.add(args)
}
func (a *distinctAcc) result() value.Value { return a.inner.result() }
