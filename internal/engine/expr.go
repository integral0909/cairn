package engine

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"unicode/utf8"

	"github.com/integral0909/cairn/internal/sql"
	"github.com/integral0909/cairn/internal/value"
)

// env is what an expression is evaluated against: the current row, the
// rows of enclosing queries (for correlated subqueries) and the statement's
// context.
type env struct {
	row   []value.Value
	outer *env
	ctx   *execCtx
}

type evalFn func(*env) (value.Value, error)

// source is one item of a FROM clause as seen by name resolution.
type source struct {
	alias   string // what the query calls it (alias or table name)
	table   *Table // nil for subqueries
	columns []string
	offset  int // where its columns start in the joined row
}

// scope resolves names for one query level.
type scope struct {
	sources []*source
	parent  *scope
	// correlated is set on a subquery's holder scope when the subquery
	// refers to columns of enclosing queries.
	correlated bool
	// agg is set while compiling expressions evaluated per group.
	agg *aggScope
	// outputs lets ORDER BY refer to select-list aliases.
	outputs []outputCol
}

type outputCol struct {
	name string
	expr sql.Expr
}

func (s *scope) width() int {
	n := 0
	for _, src := range s.sources {
		n += len(src.columns)
	}
	return n
}

// resolve finds a column: how many scopes up, its index in that scope's
// row, and the scope it was found in.
func (s *scope) resolve(ref *sql.ColumnRef) (depth, idx int, found *scope, err error) {
	for sc, d := s, 0; sc != nil; sc, d = sc.parent, d+1 {
		at := -1
		for _, src := range sc.sources {
			if ref.Table != "" && !strings.EqualFold(src.alias, ref.Table) {
				continue
			}
			for i, c := range src.columns {
				if strings.EqualFold(c, ref.Name) {
					if at >= 0 {
						return 0, 0, nil, fmt.Errorf("column %s is ambiguous; qualify it with a table name", ref.Name)
					}
					at = src.offset + i
				}
			}
		}
		if at >= 0 {
			return d, at, sc, nil
		}
		if ref.Table != "" && sc.hasSource(ref.Table) {
			return 0, 0, nil, fmt.Errorf("table %s has no column %s", ref.Table, ref.Name)
		}
	}
	if ref.Table != "" {
		return 0, 0, nil, fmt.Errorf("no table called %s in this query", ref.Table)
	}
	return 0, 0, nil, fmt.Errorf("no such column: %s", ref.Name)
}

// noteResolved marks every subquery being planned that the reference
// reaches out of: those whose enclosing scope is the found scope or below
// it. Such subqueries must run again for each outer row.
func (c *compiler) noteResolved(found *scope) {
	for _, h := range c.holders {
		for x := h.parent; x != nil; x = x.parent {
			if x == found {
				h.correlated = true
				break
			}
		}
	}
}

func (s *scope) hasSource(alias string) bool {
	for _, src := range s.sources {
		if strings.EqualFold(src.alias, alias) {
			return true
		}
	}
	return false
}

var aggregateFuncs = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true, "total": true,
	"group_concat": true, "string_agg": true, "bool_and": true, "bool_or": true,
}

func isAggregate(f *sql.FuncCall) bool {
	if !aggregateFuncs[f.Name] {
		return false
	}
	// min(a, b) and max(a, b) with several arguments are scalar.
	return !((f.Name == "min" || f.Name == "max") && len(f.Args) > 1)
}

// hasAggregate reports whether e contains an aggregate call (outside
// subqueries).
func hasAggregate(e sql.Expr) bool {
	found := false
	walkExpr(e, func(x sql.Expr) {
		if f, ok := x.(*sql.FuncCall); ok && isAggregate(f) {
			found = true
		}
	})
	return found
}

// compileExpr turns an expression into an evaluator for rows of scope s.
func (c *compiler) compileExpr(e sql.Expr, s *scope) (evalFn, error) {
	if s.agg != nil {
		if fn, ok, err := s.agg.lookup(c, e); ok || err != nil {
			return fn, err
		}
	}
	switch x := e.(type) {
	case *sql.Literal:
		v := x.Value
		return func(*env) (value.Value, error) { return v, nil }, nil
	case *sql.Param:
		n := x.N
		if n > c.maxParam {
			c.maxParam = n
		}
		return func(en *env) (value.Value, error) {
			if n > len(en.ctx.params) {
				return value.Value{}, fmt.Errorf("parameter $%d has no value (%d given)", n, len(en.ctx.params))
			}
			return en.ctx.params[n-1], nil
		}, nil
	case *sql.ColumnRef:
		if s.agg != nil {
			return nil, fmt.Errorf("column %s must appear in GROUP BY or be used in an aggregate", sql.FormatExpr(x))
		}
		depth, idx, found, err := s.resolve(x)
		if err != nil {
			return nil, err
		}
		c.noteResolved(found)
		if depth == 0 {
			return func(en *env) (value.Value, error) { return en.row[idx], nil }, nil
		}
		return func(en *env) (value.Value, error) {
			for i := 0; i < depth; i++ {
				en = en.outer
			}
			return en.row[idx], nil
		}, nil
	case *sql.Unary:
		return c.compileUnary(x, s)
	case *sql.Binary:
		return c.compileBinary(x, s)
	case *sql.FuncCall:
		if isAggregate(x) {
			return nil, fmt.Errorf("aggregate %s() is not allowed here", x.Name)
		}
		return c.compileFunc(x, s)
	case *sql.Case:
		return c.compileCase(x, s)
	case *sql.In:
		return c.compileIn(x, s)
	case *sql.Between:
		xf, err := c.compileExpr(x.X, s)
		if err != nil {
			return nil, err
		}
		lo, err := c.compileExpr(x.Lo, s)
		if err != nil {
			return nil, err
		}
		hi, err := c.compileExpr(x.Hi, s)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(en *env) (value.Value, error) {
			v, err := xf(en)
			if err != nil {
				return v, err
			}
			l, err := lo(en)
			if err != nil {
				return l, err
			}
			h, err := hi(en)
			if err != nil {
				return h, err
			}
			ge, err := cmpOp(">=", v, l)
			if err != nil {
				return ge, err
			}
			le, err := cmpOp("<=", v, h)
			if err != nil {
				return le, err
			}
			r := and3(ge, le)
			if not {
				r = not3(r)
			}
			return r, nil
		}, nil
	case *sql.Like:
		xf, err := c.compileExpr(x.X, s)
		if err != nil {
			return nil, err
		}
		pf, err := c.compileExpr(x.Pattern, s)
		if err != nil {
			return nil, err
		}
		not, isGlob := x.Not, x.Glob
		return func(en *env) (value.Value, error) {
			v, err := xf(en)
			if err != nil || v.IsNull() {
				return value.Value{}, err
			}
			p, err := pf(en)
			if err != nil || p.IsNull() {
				return value.Value{}, err
			}
			var m bool
			if isGlob {
				m = glob(textOf(v), textOf(p))
			} else {
				m = like(textOf(v), textOf(p))
			}
			return value.NewBool(m != not), nil
		}, nil
	case *sql.IsNull:
		xf, err := c.compileExpr(x.X, s)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(en *env) (value.Value, error) {
			v, err := xf(en)
			if err != nil {
				return v, err
			}
			return value.NewBool(v.IsNull() != not), nil
		}, nil
	case *sql.Is:
		lf, err := c.compileExpr(x.L, s)
		if err != nil {
			return nil, err
		}
		rf, err := c.compileExpr(x.R, s)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(en *env) (value.Value, error) {
			l, err := lf(en)
			if err != nil {
				return l, err
			}
			r, err := rf(en)
			if err != nil {
				return r, err
			}
			var same bool
			switch {
			case l.IsNull() || r.IsNull():
				same = l.IsNull() && r.IsNull()
			default:
				cmp, err := compareSQL(l, r)
				if err != nil {
					return value.Value{}, err
				}
				same = cmp == 0
			}
			return value.NewBool(same != not), nil
		}, nil
	case *sql.Cast:
		xf, err := c.compileExpr(x.X, s)
		if err != nil {
			return nil, err
		}
		typ := x.Type
		return func(en *env) (value.Value, error) {
			v, err := xf(en)
			if err != nil {
				return v, err
			}
			return castTo(v, typ)
		}, nil
	case *sql.Exists:
		sub, err := c.subquery(x.Select, s)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(en *env) (value.Value, error) {
			rows, err := sub.rows(en, 1)
			if err != nil {
				return value.Value{}, err
			}
			return value.NewBool((len(rows) > 0) != not), nil
		}, nil
	case *sql.Subquery:
		sub, err := c.subquery(x.Select, s)
		if err != nil {
			return nil, err
		}
		if len(sub.plan.columns()) != 1 {
			return nil, fmt.Errorf("a scalar subquery must return one column, not %d", len(sub.plan.columns()))
		}
		return func(en *env) (value.Value, error) {
			rows, err := sub.rows(en, 2)
			if err != nil {
				return value.Value{}, err
			}
			switch len(rows) {
			case 0:
				return value.Value{}, nil
			case 1:
				return rows[0][0], nil
			}
			return value.Value{}, fmt.Errorf("a scalar subquery returned more than one row")
		}, nil
	}
	return nil, fmt.Errorf("unsupported expression %T", e)
}

// Three-valued logic: NULL is "unknown".
func truth(v value.Value) (bool, bool) { return v.Truth() }

func and3(a, b value.Value) value.Value {
	at, ak := truth(a)
	bt, bk := truth(b)
	if (ak && !at) || (bk && !bt) {
		return value.NewBool(false)
	}
	if ak && bk {
		return value.NewBool(true)
	}
	return value.Value{}
}

func or3(a, b value.Value) value.Value {
	at, ak := truth(a)
	bt, bk := truth(b)
	if (ak && at) || (bk && bt) {
		return value.NewBool(true)
	}
	if ak && bk {
		return value.NewBool(false)
	}
	return value.Value{}
}

func not3(a value.Value) value.Value {
	t, k := truth(a)
	if !k {
		return value.Value{}
	}
	return value.NewBool(!t)
}

func checkBoolean(v value.Value, what string) error {
	if v.IsNull() || v.IsNumeric() {
		return nil
	}
	return fmt.Errorf("%s needs a boolean, got %s %s", what, v.K, v.Literal())
}

func cmpOp(op string, l, r value.Value) (value.Value, error) {
	if l.IsNull() || r.IsNull() {
		return value.Value{}, nil
	}
	c, err := compareSQL(l, r)
	if err != nil {
		return value.Value{}, err
	}
	var ok bool
	switch op {
	case "=":
		ok = c == 0
	case "!=":
		ok = c != 0
	case "<":
		ok = c < 0
	case "<=":
		ok = c <= 0
	case ">":
		ok = c > 0
	case ">=":
		ok = c >= 0
	}
	return value.NewBool(ok), nil
}

func (c *compiler) compileUnary(x *sql.Unary, s *scope) (evalFn, error) {
	f, err := c.compileExpr(x.X, s)
	if err != nil {
		return nil, err
	}
	if x.Op == "NOT" {
		return func(en *env) (value.Value, error) {
			v, err := f(en)
			if err != nil {
				return v, err
			}
			if err := checkBoolean(v, "NOT"); err != nil {
				return v, err
			}
			return not3(v), nil
		}, nil
	}
	return func(en *env) (value.Value, error) {
		v, err := f(en)
		if err != nil {
			return v, err
		}
		return arith("-", value.NewInt(0), v)
	}, nil
}

func (c *compiler) compileBinary(x *sql.Binary, s *scope) (evalFn, error) {
	lf, err := c.compileExpr(x.L, s)
	if err != nil {
		return nil, err
	}
	rf, err := c.compileExpr(x.R, s)
	if err != nil {
		return nil, err
	}
	op := x.Op
	switch op {
	case "AND", "OR":
		return func(en *env) (value.Value, error) {
			l, err := lf(en)
			if err != nil {
				return l, err
			}
			if err := checkBoolean(l, op); err != nil {
				return l, err
			}
			// Short-circuit when the left side decides.
			if t, k := truth(l); k && t == (op == "OR") {
				return value.NewBool(t), nil
			}
			r, err := rf(en)
			if err != nil {
				return r, err
			}
			if err := checkBoolean(r, op); err != nil {
				return r, err
			}
			if op == "AND" {
				return and3(l, r), nil
			}
			return or3(l, r), nil
		}, nil
	case "=", "!=", "<", "<=", ">", ">=":
		return func(en *env) (value.Value, error) {
			l, err := lf(en)
			if err != nil {
				return l, err
			}
			r, err := rf(en)
			if err != nil {
				return r, err
			}
			return cmpOp(op, l, r)
		}, nil
	case "||":
		return func(en *env) (value.Value, error) {
			l, err := lf(en)
			if err != nil || l.IsNull() {
				return value.Value{}, err
			}
			r, err := rf(en)
			if err != nil || r.IsNull() {
				return value.Value{}, err
			}
			return value.NewText(textOf(l) + textOf(r)), nil
		}, nil
	}
	return func(en *env) (value.Value, error) {
		l, err := lf(en)
		if err != nil {
			return l, err
		}
		r, err := rf(en)
		if err != nil {
			return r, err
		}
		return arith(op, l, r)
	}, nil
}

func (c *compiler) compileCase(x *sql.Case, s *scope) (evalFn, error) {
	var operand evalFn
	var err error
	if x.Operand != nil {
		if operand, err = c.compileExpr(x.Operand, s); err != nil {
			return nil, err
		}
	}
	conds := make([]evalFn, len(x.Whens))
	results := make([]evalFn, len(x.Whens))
	for i, w := range x.Whens {
		if conds[i], err = c.compileExpr(w.Cond, s); err != nil {
			return nil, err
		}
		if results[i], err = c.compileExpr(w.Result, s); err != nil {
			return nil, err
		}
	}
	var elseFn evalFn
	if x.Else != nil {
		if elseFn, err = c.compileExpr(x.Else, s); err != nil {
			return nil, err
		}
	}
	return func(en *env) (value.Value, error) {
		var subject value.Value
		if operand != nil {
			v, err := operand(en)
			if err != nil {
				return v, err
			}
			subject = v
		}
		for i, cond := range conds {
			cv, err := cond(en)
			if err != nil {
				return cv, err
			}
			if operand != nil {
				if cv, err = cmpOp("=", subject, cv); err != nil {
					return cv, err
				}
			}
			if t, k := truth(cv); k && t {
				return results[i](en)
			}
		}
		if elseFn != nil {
			return elseFn(en)
		}
		return value.Value{}, nil
	}, nil
}

func (c *compiler) compileIn(x *sql.In, s *scope) (evalFn, error) {
	xf, err := c.compileExpr(x.X, s)
	if err != nil {
		return nil, err
	}
	not := x.Not
	finish := func(found, sawNull bool) value.Value {
		switch {
		case found:
			return value.NewBool(!not)
		case sawNull:
			return value.Value{} // unknown: NULL might have matched
		}
		return value.NewBool(not)
	}
	if x.Select != nil {
		sub, err := c.subquery(x.Select, s)
		if err != nil {
			return nil, err
		}
		if len(sub.plan.columns()) != 1 {
			return nil, fmt.Errorf("IN (SELECT ...) needs exactly one column")
		}
		return func(en *env) (value.Value, error) {
			v, err := xf(en)
			if err != nil || v.IsNull() {
				return value.Value{}, err
			}
			set, err := sub.set(en)
			if err != nil {
				return value.Value{}, err
			}
			_, found := set.keys[string(value.EncodeKey(v))]
			return finish(found, set.hasNull), nil
		}, nil
	}
	items := make([]evalFn, len(x.List))
	for i, e := range x.List {
		if items[i], err = c.compileExpr(e, s); err != nil {
			return nil, err
		}
	}
	return func(en *env) (value.Value, error) {
		v, err := xf(en)
		if err != nil || v.IsNull() {
			return value.Value{}, err
		}
		sawNull := false
		for _, item := range items {
			iv, err := item(en)
			if err != nil {
				return iv, err
			}
			if iv.IsNull() {
				sawNull = true
				continue
			}
			cmp, err := compareSQL(v, iv)
			if err != nil {
				return value.Value{}, err
			}
			if cmp == 0 {
				return finish(true, false), nil
			}
		}
		return finish(false, sawNull), nil
	}, nil
}

// ---- scalar functions ------------------------------------------------------

type scalarFunc struct {
	minArgs, maxArgs int // maxArgs -1 for any
	fn               func(args []value.Value) (value.Value, error)
	// nullIn: return NULL as soon as any argument is NULL.
	nullIn bool
}

var scalarFuncs map[string]scalarFunc

func init() {
	text1 := func(f func(string) string) scalarFunc {
		return scalarFunc{1, 1, func(a []value.Value) (value.Value, error) { return value.NewText(f(textOf(a[0]))), nil }, true}
	}
	scalarFuncs = map[string]scalarFunc{
		"abs": {1, 1, func(a []value.Value) (value.Value, error) {
			switch a[0].K {
			case value.Int:
				if a[0].I == math.MinInt64 {
					return value.Value{}, errOverflow
				}
				if a[0].I < 0 {
					return value.NewInt(-a[0].I), nil
				}
				return a[0], nil
			case value.Real:
				return value.NewReal(math.Abs(a[0].F)), nil
			}
			return value.Value{}, fmt.Errorf("abs() needs a number")
		}, true},
		"coalesce": {1, -1, func(a []value.Value) (value.Value, error) {
			for _, v := range a {
				if !v.IsNull() {
					return v, nil
				}
			}
			return value.Value{}, nil
		}, false},
		"ifnull": {2, 2, func(a []value.Value) (value.Value, error) {
			if a[0].IsNull() {
				return a[1], nil
			}
			return a[0], nil
		}, false},
		"nullif": {2, 2, func(a []value.Value) (value.Value, error) {
			if !a[0].IsNull() && !a[1].IsNull() && categoryOf(a[0]) == categoryOf(a[1]) && value.Compare(a[0], a[1]) == 0 {
				return value.Value{}, nil
			}
			return a[0], nil
		}, false},
		"iif": {3, 3, func(a []value.Value) (value.Value, error) {
			if t, k := truth(a[0]); k && t {
				return a[1], nil
			}
			return a[2], nil
		}, false},
		"length": {1, 1, func(a []value.Value) (value.Value, error) {
			if a[0].K == value.Blob {
				return value.NewInt(int64(len(a[0].B))), nil
			}
			return value.NewInt(int64(utf8.RuneCountInString(textOf(a[0])))), nil
		}, true},
		"lower": text1(strings.ToLower),
		"upper": text1(strings.ToUpper),
		"trim":  {1, 2, trimFunc(strings.Trim), true},
		"ltrim": {1, 2, trimFunc(strings.TrimLeft), true},
		"rtrim": {1, 2, trimFunc(strings.TrimRight), true},
		"replace": {3, 3, func(a []value.Value) (value.Value, error) {
			if textOf(a[1]) == "" {
				return value.NewText(textOf(a[0])), nil
			}
			return value.NewText(strings.ReplaceAll(textOf(a[0]), textOf(a[1]), textOf(a[2]))), nil
		}, true},
		"substr":    {2, 3, substr, true},
		"substring": {2, 3, substr, true},
		"instr": {2, 2, func(a []value.Value) (value.Value, error) {
			s, sub := textOf(a[0]), textOf(a[1])
			i := strings.Index(s, sub)
			if i < 0 {
				return value.NewInt(0), nil
			}
			return value.NewInt(int64(utf8.RuneCountInString(s[:i]) + 1)), nil
		}, true},
		"round": {1, 2, func(a []value.Value) (value.Value, error) {
			if !a[0].IsNumeric() {
				return value.Value{}, fmt.Errorf("round() needs a number")
			}
			digits := 0
			if len(a) == 2 {
				if a[1].K != value.Int {
					return value.Value{}, fmt.Errorf("round() digits must be an integer")
				}
				digits = int(a[1].I)
			}
			p := math.Pow(10, float64(digits))
			return value.NewReal(math.Round(a[0].Float()*p) / p), nil
		}, true},
		"typeof": {1, 1, func(a []value.Value) (value.Value, error) {
			return value.NewText(strings.ToLower(a[0].K.String())), nil
		}, false},
		"hex": {1, 1, func(a []value.Value) (value.Value, error) { return value.NewText(hexOf(a[0])), nil }, true},
		"random": {0, 0, func([]value.Value) (value.Value, error) {
			return value.NewInt(rand.Int63() - rand.Int63()), nil
		}, false},
		"min": {2, -1, func(a []value.Value) (value.Value, error) { return extreme(a, -1) }, true},
		"max": {2, -1, func(a []value.Value) (value.Value, error) { return extreme(a, 1) }, true},
		"concat": {1, -1, func(a []value.Value) (value.Value, error) {
			var b strings.Builder
			for _, v := range a {
				if !v.IsNull() {
					b.WriteString(textOf(v))
				}
			}
			return value.NewText(b.String()), nil
		}, false},
		"sign": {1, 1, func(a []value.Value) (value.Value, error) {
			if !a[0].IsNumeric() {
				return value.Value{}, fmt.Errorf("sign() needs a number")
			}
			f := a[0].Float()
			switch {
			case f > 0:
				return value.NewInt(1), nil
			case f < 0:
				return value.NewInt(-1), nil
			}
			return value.NewInt(0), nil
		}, true},
	}
}

func trimFunc(f func(string, string) string) func([]value.Value) (value.Value, error) {
	return func(a []value.Value) (value.Value, error) {
		cut := " "
		if len(a) == 2 {
			cut = textOf(a[1])
		}
		return value.NewText(f(textOf(a[0]), cut)), nil
	}
}

func substr(a []value.Value) (value.Value, error) {
	runes := []rune(textOf(a[0]))
	if a[1].K != value.Int || (len(a) == 3 && a[2].K != value.Int) {
		return value.Value{}, fmt.Errorf("substr() positions must be integers")
	}
	start := a[1].I - 1 // 1-based
	end := int64(len(runes))
	if len(a) == 3 {
		if a[2].I < 0 {
			return value.Value{}, fmt.Errorf("substr() length must not be negative")
		}
		end = start + a[2].I
	}
	start = max(start, 0)
	end = min(end, int64(len(runes)))
	if start >= end {
		return value.NewText(""), nil
	}
	return value.NewText(string(runes[start:end])), nil
}

func extreme(a []value.Value, dir int) (value.Value, error) {
	best := a[0]
	for _, v := range a[1:] {
		c, err := compareSQL(v, best)
		if err != nil {
			return value.Value{}, err
		}
		if c*dir > 0 {
			best = v
		}
	}
	return best, nil
}

func (c *compiler) compileFunc(x *sql.FuncCall, s *scope) (evalFn, error) {
	f, ok := scalarFuncs[x.Name]
	if !ok {
		return nil, fmt.Errorf("no such function: %s", x.Name)
	}
	if x.Star || x.Distinct {
		return nil, fmt.Errorf("%s() does not take * or DISTINCT", x.Name)
	}
	if len(x.Args) < f.minArgs || (f.maxArgs >= 0 && len(x.Args) > f.maxArgs) {
		return nil, fmt.Errorf("wrong number of arguments to %s()", x.Name)
	}
	args := make([]evalFn, len(x.Args))
	for i, a := range x.Args {
		var err error
		if args[i], err = c.compileExpr(a, s); err != nil {
			return nil, err
		}
	}
	return func(en *env) (value.Value, error) {
		vals := make([]value.Value, len(args))
		for i, a := range args {
			v, err := a(en)
			if err != nil {
				return v, err
			}
			if f.nullIn && v.IsNull() {
				return value.Value{}, nil
			}
			vals[i] = v
		}
		return f.fn(vals)
	}, nil
}
