package sql

import (
	"math"
	"strconv"
	"strings"

	"github.com/integral0909/cairn/internal/value"
)

// reserved keywords cannot be used as bare identifiers; other keywords
// (TEXT, KEY, REPLACE, ...) can, so columns called "key" or "text" work.
var reserved = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`ALL ALTER AND AS ASC BETWEEN BY CASE CAST CREATE CROSS DEFAULT DELETE DESC
		DISTINCT DROP ELSE END EXCEPT EXISTS EXPLAIN FALSE FROM FULL GLOB GROUP HAVING IN INDEX INNER INSERT
		INTERSECT INTO IS JOIN LEFT LIKE LIMIT NOT NULL OFFSET ON OR ORDER OUTER PRIMARY SELECT SET TABLE THEN
		TRUE UNION UNIQUE UPDATE USING VALUES WHEN WHERE RETURNING RIGHT`) {
		reserved[k] = true
	}
	keywords["USING"] = true
	keywords["RIGHT"] = true
}

type parser struct {
	src  string
	toks []token
	pos  int
}

// Parse parses one or more statements separated by semicolons.
func Parse(src string) ([]Stmt, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	var out []Stmt
	for {
		for p.isOp(";") {
			p.pos++
		}
		if p.peek().kind == tEOF {
			return out, nil
		}
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if !p.isOp(";") && p.peek().kind != tEOF {
			return nil, p.errorf("expected ; or end of input, found %s", p.describe())
		}
	}
}

// ParseOne parses exactly one statement.
func ParseOne(src string) (Stmt, error) {
	stmts, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		return nil, &Error{Msg: "expected exactly one statement", Line: 1, Col: 1}
	}
	return stmts[0], nil
}

// NumParams returns the highest parameter number used in a statement source.
func NumParams(src string) int {
	toks, err := lex(src)
	if err != nil {
		return 0
	}
	n := 0
	for _, t := range toks {
		if t.kind == tParam && t.num > n {
			n = t.num
		}
	}
	return n
}

// ParseExpr parses a standalone expression (used for stored defaults).
func ParseExpr(src string) (Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	e, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errorf("unexpected %s", p.describe())
	}
	return e, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) peekAt(n int) token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) errorf(format string, args ...any) error {
	return errorAt(p.src, p.peek().pos, format, args...)
}

func (p *parser) describe() string {
	t := p.peek()
	switch t.kind {
	case tEOF:
		return "end of input"
	case tString:
		return "'" + t.text + "'"
	case tBlob:
		return "blob literal"
	case tParam:
		return "parameter"
	}
	return strconv.Quote(p.src[t.pos:min(len(p.src), t.pos+max(1, len(t.text)))])
}

func (p *parser) isKw(words ...string) bool {
	t := p.peek()
	if t.kind != tKeyword {
		return false
	}
	for _, w := range words {
		if t.text == w {
			return true
		}
	}
	return false
}

func (p *parser) acceptKw(words ...string) bool {
	if p.isKw(words...) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectKw(word string) error {
	if !p.acceptKw(word) {
		return p.errorf("expected %s, found %s", word, p.describe())
	}
	return nil
}

func (p *parser) isOp(op string) bool {
	t := p.peek()
	return t.kind == tOp && t.text == op
}

func (p *parser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.errorf("expected %q, found %s", op, p.describe())
	}
	return nil
}

func (p *parser) isIdent() bool {
	t := p.peek()
	return t.kind == tIdent || (t.kind == tKeyword && !reserved[t.text])
}

func (p *parser) ident(what string) (string, error) {
	if !p.isIdent() {
		return "", p.errorf("expected %s, found %s", what, p.describe())
	}
	t := p.next()
	if t.kind == tKeyword {
		return strings.ToLower(t.text), nil
	}
	return t.text, nil
}

func (p *parser) identList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		name, err := p.ident("a column name")
		if err != nil {
			return nil, err
		}
		out = append(out, name)
		if !p.acceptOp(",") {
			break
		}
	}
	return out, p.expectOp(")")
}

func (p *parser) statement() (Stmt, error) {
	switch {
	case p.acceptKw("EXPLAIN"):
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		if _, nested := s.(*Explain); nested {
			return nil, p.errorf("EXPLAIN cannot be nested")
		}
		return &Explain{Stmt: s}, nil
	case p.isKw("SELECT"), p.isKw("VALUES"), p.isOp("("):
		return p.selectStmt()
	case p.acceptKw("INSERT"):
		return p.insert(false)
	case p.acceptKw("REPLACE"):
		return p.insert(true)
	case p.acceptKw("UPDATE"):
		return p.update()
	case p.acceptKw("DELETE"):
		return p.delete()
	case p.acceptKw("CREATE"):
		if p.acceptKw("TABLE") {
			return p.createTable()
		}
		unique := p.acceptKw("UNIQUE")
		if p.acceptKw("INDEX") {
			return p.createIndex(unique)
		}
		return nil, p.errorf("expected TABLE or INDEX after CREATE")
	case p.acceptKw("DROP"):
		return p.drop()
	case p.acceptKw("ALTER"):
		return p.alter()
	case p.acceptKw("BEGIN"):
		p.acceptKw("TRANSACTION")
		return &Begin{}, nil
	case p.acceptKw("COMMIT"), p.acceptKw("END"):
		p.acceptKw("TRANSACTION")
		return &Commit{}, nil
	case p.acceptKw("ROLLBACK"):
		p.acceptKw("TRANSACTION")
		return &Rollback{}, nil
	case p.acceptKw("ANALYZE"):
		a := &Analyze{}
		if p.isIdent() {
			a.Table, _ = p.ident("a table name")
		}
		return a, nil
	}
	return nil, p.errorf("expected a statement, found %s", p.describe())
}

func (p *parser) ifNotExists() (bool, error) {
	if p.acceptKw("IF") {
		if err := p.expectKw("NOT"); err != nil {
			return false, err
		}
		return true, p.expectKw("EXISTS")
	}
	return false, nil
}

func (p *parser) ifExists() (bool, error) {
	if p.acceptKw("IF") {
		return true, p.expectKw("EXISTS")
	}
	return false, nil
}

func (p *parser) colType() (Type, error) {
	t := p.peek()
	if t.kind != tKeyword && t.kind != tIdent {
		return 0, p.errorf("expected a column type, found %s", p.describe())
	}
	p.pos++
	var kind Type
	switch strings.ToUpper(t.text) {
	case "INTEGER", "INT", "BIGINT", "SMALLINT":
		kind = value.Int
	case "REAL", "FLOAT", "DOUBLE":
		kind = value.Real
		if strings.ToUpper(t.text) == "DOUBLE" && p.isIdent() && p.peek().text == "precision" {
			p.pos++
		}
	case "TEXT", "VARCHAR", "CHAR":
		kind = value.Text
	case "BLOB":
		kind = value.Blob
	case "BOOLEAN", "BOOL":
		kind = value.Bool
	default:
		p.pos--
		return 0, p.errorf("unknown column type %s (use INTEGER, REAL, TEXT, BLOB or BOOLEAN)", p.describe())
	}
	// VARCHAR(255) and friends: the length is accepted and ignored.
	if p.acceptOp("(") {
		for !p.acceptOp(")") {
			if p.peek().kind == tEOF {
				return 0, p.errorf("unterminated type arguments")
			}
			p.pos++
		}
	}
	return kind, nil
}

func (p *parser) createTable() (Stmt, error) {
	ct := &CreateTable{}
	var err error
	if ct.IfNotExists, err = p.ifNotExists(); err != nil {
		return nil, err
	}
	if ct.Name, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		switch {
		case p.acceptKw("PRIMARY"):
			if err := p.expectKw("KEY"); err != nil {
				return nil, err
			}
			if ct.PrimaryKey != nil {
				return nil, p.errorf("a table can have only one PRIMARY KEY")
			}
			if ct.PrimaryKey, err = p.identList(); err != nil {
				return nil, err
			}
		case p.acceptKw("UNIQUE"):
			cols, err := p.identList()
			if err != nil {
				return nil, err
			}
			ct.Uniques = append(ct.Uniques, cols)
		default:
			col, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			ct.Columns = append(ct.Columns, col)
		}
		if !p.acceptOp(",") {
			break
		}
	}
	return ct, p.expectOp(")")
}

func (p *parser) columnDef() (ColumnDef, error) {
	var col ColumnDef
	var err error
	if col.Name, err = p.ident("a column name"); err != nil {
		return col, err
	}
	if col.Type, err = p.colType(); err != nil {
		return col, err
	}
	for {
		switch {
		case p.acceptKw("PRIMARY"):
			if err := p.expectKw("KEY"); err != nil {
				return col, err
			}
			col.PrimaryKey = true
		case p.acceptKw("NOT"):
			if err := p.expectKw("NULL"); err != nil {
				return col, err
			}
			col.NotNull = true
		case p.acceptKw("NULL"):
		case p.acceptKw("UNIQUE"):
			col.Unique = true
		case p.acceptKw("DEFAULT"):
			if col.Default, err = p.unary(); err != nil {
				return col, err
			}
		default:
			return col, nil
		}
	}
}

func (p *parser) createIndex(unique bool) (Stmt, error) {
	ci := &CreateIndex{Unique: unique}
	var err error
	if ci.IfNotExists, err = p.ifNotExists(); err != nil {
		return nil, err
	}
	if ci.Name, err = p.ident("an index name"); err != nil {
		return nil, err
	}
	if err := p.expectKw("ON"); err != nil {
		return nil, err
	}
	if ci.Table, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	ci.Columns, err = p.identList()
	return ci, err
}

func (p *parser) drop() (Stmt, error) {
	switch {
	case p.acceptKw("TABLE"):
		d := &DropTable{}
		var err error
		if d.IfExists, err = p.ifExists(); err != nil {
			return nil, err
		}
		d.Name, err = p.ident("a table name")
		return d, err
	case p.acceptKw("INDEX"):
		d := &DropIndex{}
		var err error
		if d.IfExists, err = p.ifExists(); err != nil {
			return nil, err
		}
		d.Name, err = p.ident("an index name")
		return d, err
	}
	return nil, p.errorf("expected TABLE or INDEX after DROP")
}

func (p *parser) alter() (Stmt, error) {
	if err := p.expectKw("TABLE"); err != nil {
		return nil, err
	}
	a := &AlterTable{}
	var err error
	if a.Table, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("ADD"):
		p.acceptKw("COLUMN")
		col, err := p.columnDef()
		if err != nil {
			return nil, err
		}
		a.AddColumn = &col
	case p.acceptKw("RENAME"):
		if err := p.expectKw("TO"); err != nil {
			return nil, err
		}
		if a.RenameTo, err = p.ident("a table name"); err != nil {
			return nil, err
		}
	default:
		return nil, p.errorf("expected ADD COLUMN or RENAME TO")
	}
	return a, nil
}

func (p *parser) returning() ([]ResultCol, error) {
	if !p.acceptKw("RETURNING") {
		return nil, nil
	}
	return p.resultCols()
}

func (p *parser) insert(replace bool) (Stmt, error) {
	if !replace && p.acceptKw("OR") {
		if !p.acceptKw("REPLACE") {
			return nil, p.errorf("expected REPLACE after INSERT OR")
		}
		replace = true
	}
	if err := p.expectKw("INTO"); err != nil {
		return nil, err
	}
	ins := &Insert{}
	if replace {
		ins.OnConflict = "replace"
	}
	var err error
	if ins.Table, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	if p.isOp("(") {
		if ins.Columns, err = p.identList(); err != nil {
			return nil, err
		}
	}
	switch {
	case p.acceptKw("VALUES"):
		for {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			row, err := p.exprList()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			ins.Rows = append(ins.Rows, row)
			if !p.acceptOp(",") {
				break
			}
		}
	case p.isKw("SELECT"), p.isOp("("):
		s, err := p.selectStmt()
		if err != nil {
			return nil, err
		}
		ins.Select = s
	case p.acceptKw("DEFAULT"):
		if err := p.expectKw("VALUES"); err != nil {
			return nil, err
		}
		ins.Rows = [][]Expr{{}}
	default:
		return nil, p.errorf("expected VALUES or SELECT, found %s", p.describe())
	}
	if p.acceptKw("ON") {
		if err := p.expectKw("CONFLICT"); err != nil {
			return nil, err
		}
		if err := p.expectKw("DO"); err != nil {
			return nil, err
		}
		if err := p.expectKw("NOTHING"); err != nil {
			return nil, err
		}
		ins.OnConflict = "nothing"
	}
	ins.Returning, err = p.returning()
	return ins, err
}

func (p *parser) update() (Stmt, error) {
	u := &Update{}
	var err error
	if u.Table, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	if err := p.expectKw("SET"); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident("a column name")
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		u.Set = append(u.Set, Assignment{Column: col, Value: e})
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("WHERE") {
		if u.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	u.Returning, err = p.returning()
	return u, err
}

func (p *parser) delete() (Stmt, error) {
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	d := &Delete{}
	var err error
	if d.Table, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	if p.acceptKw("WHERE") {
		if d.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	d.Returning, err = p.returning()
	return d, err
}

// selectStmt parses a select, including compound operators and the
// ORDER BY / LIMIT that apply to the whole compound.
func (p *parser) selectStmt() (*Select, error) {
	first, err := p.selectCore()
	if err != nil {
		return nil, err
	}
	cur := first
	for {
		var op string
		switch {
		case p.acceptKw("UNION"):
			op = "UNION"
			if p.acceptKw("ALL") {
				op = "UNION ALL"
			}
		case p.acceptKw("INTERSECT"):
			op = "INTERSECT"
		case p.acceptKw("EXCEPT"):
			op = "EXCEPT"
		}
		if op == "" {
			break
		}
		next, err := p.selectCore()
		if err != nil {
			return nil, err
		}
		cur.CompoundOp, cur.Next = op, next
		cur = next
	}
	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			item := OrderItem{Expr: e}
			if p.acceptKw("DESC") {
				item.Desc = true
			} else {
				p.acceptKw("ASC")
			}
			if p.acceptKw("NULLS") {
				nullsFirst := p.acceptKw("FIRST")
				if !nullsFirst && !p.acceptKw("LAST") {
					return nil, p.errorf("expected FIRST or LAST after NULLS")
				}
				item.NullsFirst = &nullsFirst
			}
			first.OrderBy = append(first.OrderBy, item)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("LIMIT") {
		if first.Limit, err = p.expr(); err != nil {
			return nil, err
		}
		if p.acceptKw("OFFSET") {
			if first.Offset, err = p.expr(); err != nil {
				return nil, err
			}
		} else if p.acceptOp(",") {
			// LIMIT offset, count
			first.Offset = first.Limit
			if first.Limit, err = p.expr(); err != nil {
				return nil, err
			}
		}
	}
	return first, nil
}

func (p *parser) selectCore() (*Select, error) {
	if p.acceptOp("(") {
		s, err := p.selectStmt()
		if err != nil {
			return nil, err
		}
		return s, p.expectOp(")")
	}
	if p.acceptKw("VALUES") {
		// VALUES (...), (...) as a query: a union of literal rows.
		var first, cur *Select
		for {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			row, err := p.exprList()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			s := &Select{}
			for i, e := range row {
				s.Columns = append(s.Columns, ResultCol{Expr: e, Alias: "column" + strconv.Itoa(i+1)})
			}
			if first == nil {
				first = s
			} else {
				cur.CompoundOp, cur.Next = "UNION ALL", s
			}
			cur = s
			if !p.acceptOp(",") {
				return first, nil
			}
		}
	}
	if err := p.expectKw("SELECT"); err != nil {
		return nil, err
	}
	s := &Select{}
	if p.acceptKw("DISTINCT") {
		s.Distinct = true
	} else {
		p.acceptKw("ALL")
	}
	var err error
	if s.Columns, err = p.resultCols(); err != nil {
		return nil, err
	}
	if p.acceptKw("FROM") {
		if s.From, err = p.fromClause(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("WHERE") {
		if s.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("GROUP") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		if s.GroupBy, err = p.exprList(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("HAVING") {
		if s.Having, err = p.expr(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (p *parser) resultCols() ([]ResultCol, error) {
	var cols []ResultCol
	for {
		if p.acceptOp("*") {
			cols = append(cols, ResultCol{Star: true})
		} else if p.isIdent() && p.peekAt(1).kind == tOp && p.peekAt(1).text == "." &&
			p.peekAt(2).kind == tOp && p.peekAt(2).text == "*" {
			t, _ := p.ident("a table name")
			p.pos += 2
			cols = append(cols, ResultCol{Star: true, Table: t})
		} else {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			rc := ResultCol{Expr: e}
			if p.acceptKw("AS") {
				if rc.Alias, err = p.aliasName(); err != nil {
					return nil, err
				}
			} else if p.isIdent() {
				rc.Alias, _ = p.ident("an alias")
			}
			cols = append(cols, rc)
		}
		if !p.acceptOp(",") {
			return cols, nil
		}
	}
}

func (p *parser) aliasName() (string, error) {
	if p.peek().kind == tString {
		return p.next().text, nil
	}
	return p.ident("an alias")
}

func (p *parser) fromClause() (FromItem, error) {
	left, err := p.fromItem()
	if err != nil {
		return nil, err
	}
	for {
		var kind JoinKind
		switch {
		case p.acceptOp(","):
			kind = CrossJoin
		case p.acceptKw("CROSS"):
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
			kind = CrossJoin
		case p.acceptKw("LEFT"):
			p.acceptKw("OUTER")
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
			kind = LeftJoin
		case p.acceptKw("INNER"), p.isKw("JOIN"):
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
			kind = InnerJoin
		case p.isKw("FULL"), p.isKw("RIGHT"):
			return nil, p.errorf("only INNER, LEFT and CROSS joins are supported")
		default:
			return left, nil
		}
		right, err := p.fromItem()
		if err != nil {
			return nil, err
		}
		j := &Join{Kind: kind, Left: left, Right: right}
		if kind != CrossJoin {
			if p.acceptKw("ON") {
				if j.On, err = p.expr(); err != nil {
					return nil, err
				}
			} else if p.acceptKw("USING") {
				if j.Using, err = p.identList(); err != nil {
					return nil, err
				}
			} else {
				return nil, p.errorf("expected ON or USING after the joined table")
			}
		}
		left = j
	}
}

func (p *parser) fromItem() (FromItem, error) {
	if p.acceptOp("(") {
		if p.isKw("SELECT") || p.isKw("VALUES") || p.isOp("(") {
			s, err := p.selectStmt()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			ref := &SubqueryRef{Select: s}
			p.acceptKw("AS")
			if !p.isIdent() {
				return nil, p.errorf("a subquery in FROM needs a name (... ) AS name")
			}
			ref.Alias, _ = p.ident("an alias")
			return ref, nil
		}
		item, err := p.fromClause()
		if err != nil {
			return nil, err
		}
		return item, p.expectOp(")")
	}
	name, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	ref := &TableRef{Name: name}
	if p.acceptKw("AS") {
		if ref.Alias, err = p.ident("an alias"); err != nil {
			return nil, err
		}
	} else if p.isIdent() {
		ref.Alias, _ = p.ident("an alias")
	}
	return ref, nil
}

func (p *parser) exprList() ([]Expr, error) {
	var out []Expr
	if p.isOp(")") {
		return out, nil
	}
	for {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

// ---- expressions, lowest precedence first ----------------------------------

func (p *parser) expr() (Expr, error) { return p.or() }

func (p *parser) or() (Expr, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("OR") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: "OR", L: l, R: r}
	}
	return l, nil
}

func (p *parser) and() (Expr, error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("AND") {
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: "AND", L: l, R: r}
	}
	return l, nil
}

func (p *parser) not() (Expr, error) {
	if p.acceptKw("NOT") {
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return &Unary{Op: "NOT", X: x}, nil
	}
	return p.comparison()
}

func (p *parser) comparison() (Expr, error) {
	l, err := p.concat()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == tOp && (t.text == "=" || t.text == "!=" || t.text == "<" || t.text == "<=" || t.text == ">" || t.text == ">="):
			p.pos++
			r, err := p.concat()
			if err != nil {
				return nil, err
			}
			l = &Binary{Op: t.text, L: l, R: r}
		case p.acceptKw("IS"):
			not := p.acceptKw("NOT")
			if p.acceptKw("NULL") {
				l = &IsNull{X: l, Not: not}
			} else {
				r, err := p.concat()
				if err != nil {
					return nil, err
				}
				l = &Is{L: l, R: r, Not: not}
			}
		default:
			not := false
			save := p.pos
			if p.acceptKw("NOT") {
				not = true
			}
			switch {
			case p.acceptKw("IN"):
				in := &In{X: l, Not: not}
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				if p.isKw("SELECT") || p.isKw("VALUES") {
					if in.Select, err = p.selectStmt(); err != nil {
						return nil, err
					}
				} else if in.List, err = p.exprList(); err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				l = in
			case p.acceptKw("BETWEEN"):
				lo, err := p.concat()
				if err != nil {
					return nil, err
				}
				if err := p.expectKw("AND"); err != nil {
					return nil, err
				}
				hi, err := p.concat()
				if err != nil {
					return nil, err
				}
				l = &Between{X: l, Lo: lo, Hi: hi, Not: not}
			case p.isKw("LIKE"), p.isKw("GLOB"):
				glob := p.next().text == "GLOB"
				pat, err := p.concat()
				if err != nil {
					return nil, err
				}
				l = &Like{X: l, Pattern: pat, Not: not, Glob: glob}
			default:
				p.pos = save
				return l, nil
			}
		}
	}
}

func (p *parser) concat() (Expr, error) {
	l, err := p.additive()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("||") {
		r, err := p.additive()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: "||", L: l, R: r}
	}
	return l, nil
}

func (p *parser) additive() (Expr, error) {
	l, err := p.multiplicative()
	if err != nil {
		return nil, err
	}
	for p.isOp("+") || p.isOp("-") {
		op := p.next().text
		r, err := p.multiplicative()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: op, L: l, R: r}
	}
	return l, nil
}

func (p *parser) multiplicative() (Expr, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.isOp("*") || p.isOp("/") || p.isOp("%") {
		op := p.next().text
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: op, L: l, R: r}
	}
	return l, nil
}

func (p *parser) unary() (Expr, error) {
	if p.isOp("-") || p.isOp("+") {
		op := p.next().text
		// Fold negative numeric literals so -9223372036854775808 works.
		if op == "-" && p.peek().kind == tInt {
			t := p.next()
			n, err := strconv.ParseUint(t.text, 10, 64)
			if err == nil && n == 1<<63 {
				return &Literal{Value: value.NewInt(math.MinInt64)}, nil
			}
			p.pos--
		}
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		if op == "+" {
			return x, nil
		}
		if lit, ok := x.(*Literal); ok {
			switch lit.Value.K {
			case value.Int:
				if lit.Value.I != math.MinInt64 {
					return &Literal{Value: value.NewInt(-lit.Value.I)}, nil
				}
			case value.Real:
				return &Literal{Value: value.NewReal(-lit.Value.F)}, nil
			}
		}
		return &Unary{Op: "-", X: x}, nil
	}
	return p.primary()
}

func (p *parser) primary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tInt:
		p.pos++
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			// Too big for INTEGER: becomes a REAL, as in most SQL engines.
			f, _ := strconv.ParseFloat(t.text, 64)
			return &Literal{Value: value.NewReal(f)}, nil
		}
		return &Literal{Value: value.NewInt(n)}, nil
	case tFloat:
		p.pos++
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, errorAt(p.src, t.pos, "malformed number %s", t.text)
		}
		return &Literal{Value: value.NewReal(f)}, nil
	case tString:
		p.pos++
		return &Literal{Value: value.NewText(t.text)}, nil
	case tBlob:
		p.pos++
		return &Literal{Value: value.NewBlob(t.blob)}, nil
	case tParam:
		p.pos++
		return &Param{N: t.num}, nil
	case tOp:
		if t.text == "(" {
			p.pos++
			if p.isKw("SELECT") || p.isKw("VALUES") {
				s, err := p.selectStmt()
				if err != nil {
					return nil, err
				}
				return &Subquery{Select: s}, p.expectOp(")")
			}
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			return e, p.expectOp(")")
		}
	case tKeyword:
		switch t.text {
		case "NULL":
			p.pos++
			return &Literal{}, nil
		case "TRUE", "FALSE":
			p.pos++
			return &Literal{Value: value.NewBool(t.text == "TRUE")}, nil
		case "CASE":
			p.pos++
			return p.caseExpr()
		case "CAST":
			p.pos++
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("AS"); err != nil {
				return nil, err
			}
			typ, err := p.colType()
			if err != nil {
				return nil, err
			}
			return &Cast{X: x, Type: typ}, p.expectOp(")")
		case "EXISTS":
			p.pos++
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			s, err := p.selectStmt()
			if err != nil {
				return nil, err
			}
			return &Exists{Select: s}, p.expectOp(")")
		case "NOT":
			// NOT EXISTS reaches here only inside other expressions.
		case "LEFT", "REPLACE":
			if p.peekAt(1).kind == tOp && p.peekAt(1).text == "(" {
				p.pos++
				return p.funcCall(strings.ToLower(t.text))
			}
		}
	}
	if p.isIdent() {
		name, _ := p.ident("a name")
		if p.isOp("(") {
			return p.funcCall(name)
		}
		if p.acceptOp(".") {
			col, err := p.ident("a column name")
			if err != nil {
				return nil, err
			}
			return &ColumnRef{Table: name, Name: col}, nil
		}
		return &ColumnRef{Name: name}, nil
	}
	return nil, p.errorf("expected an expression, found %s", p.describe())
}

func (p *parser) funcCall(name string) (Expr, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	f := &FuncCall{Name: name}
	if p.acceptOp("*") {
		f.Star = true
		return f, p.expectOp(")")
	}
	f.Distinct = p.acceptKw("DISTINCT")
	args, err := p.exprList()
	if err != nil {
		return nil, err
	}
	f.Args = args
	return f, p.expectOp(")")
}

func (p *parser) caseExpr() (Expr, error) {
	c := &Case{}
	var err error
	if !p.isKw("WHEN") {
		if c.Operand, err = p.expr(); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("WHEN") {
		cond, err := p.expr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("THEN"); err != nil {
			return nil, err
		}
		res, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, When{Cond: cond, Result: res})
	}
	if len(c.Whens) == 0 {
		return nil, p.errorf("CASE needs at least one WHEN")
	}
	if p.acceptKw("ELSE") {
		if c.Else, err = p.expr(); err != nil {
			return nil, err
		}
	}
	return c, p.expectKw("END")
}
