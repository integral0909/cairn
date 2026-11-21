// Package engine plans and runs SQL statements against the storage layers.
package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/integral0909/cairn/internal/btree"
	"github.com/integral0909/cairn/internal/sql"
	"github.com/integral0909/cairn/internal/value"
)

// The schema lives in a B-tree whose root is page 1. Each entry is keyed by
// the object's name and holds a record: kind ("table" or "index"), name,
// table, root page and the CREATE statement that rebuilds it.
const schemaRoot = 1

// Column is one column of a table.
type Column struct {
	Name    string
	Type    value.Kind
	NotNull bool
	Default sql.Expr // nil for no default
}

// Table is a table: a B-tree keyed by rowid holding encoded rows.
type Table struct {
	Name    string
	Columns []Column
	Root    uint32
	// RowidCol is the INTEGER PRIMARY KEY column that aliases the rowid,
	// or -1.
	RowidCol int
	// PrimaryKey lists the primary key columns (also enforced by an
	// automatic unique index unless the key is the rowid).
	PrimaryKey []int
	Indexes    []*Index
}

// Index is a secondary index: a B-tree keyed by the indexed values followed
// by the rowid, with empty values.
type Index struct {
	Name    string
	Table   string
	Columns []int
	Unique  bool
	Root    uint32
	// Auto indexes back PRIMARY KEY and UNIQUE constraints and go away with
	// their table.
	Auto bool
}

func (t *Table) column(name string) int {
	for i, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

func (t *Table) clone() *Table {
	c := *t
	c.Columns = append([]Column(nil), t.Columns...)
	c.Indexes = append([]*Index(nil), t.Indexes...)
	return &c
}

// Catalog is an immutable snapshot of the schema. Changes produce a new
// catalog, which becomes visible to others when its transaction commits.
type Catalog struct {
	tables  map[string]*Table // by lowercase name
	indexes map[string]*Index
}

func newCatalog() *Catalog {
	return &Catalog{tables: map[string]*Table{}, indexes: map[string]*Index{}}
}

func (c *Catalog) clone() *Catalog {
	n := newCatalog()
	for k, v := range c.tables {
		n.tables[k] = v
	}
	for k, v := range c.indexes {
		n.indexes[k] = v
	}
	return n
}

func (c *Catalog) Table(name string) *Table { return c.tables[strings.ToLower(name)] }

func (c *Catalog) Index(name string) *Index { return c.indexes[strings.ToLower(name)] }

// Tables returns the tables sorted by name.
func (c *Catalog) Tables() []*Table {
	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TableSQL is the canonical CREATE TABLE statement for a table.
func TableSQL(t *Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (", quoteIdent(t.Name))
	for i, c := range t.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s", quoteIdent(c.Name), c.Type)
		if i == t.RowidCol {
			b.WriteString(" PRIMARY KEY")
		}
		if c.NotNull && i != t.RowidCol {
			b.WriteString(" NOT NULL")
		}
		if c.Default != nil {
			// Anything but a plain literal needs parentheses to parse back.
			if _, ok := c.Default.(*sql.Literal); ok {
				b.WriteString(" DEFAULT " + sql.FormatExpr(c.Default))
			} else {
				b.WriteString(" DEFAULT (" + sql.FormatExpr(c.Default) + ")")
			}
		}
	}
	if t.RowidCol < 0 && len(t.PrimaryKey) > 0 {
		b.WriteString(", PRIMARY KEY (" + t.columnList(t.PrimaryKey) + ")")
	}
	b.WriteString(")")
	return b.String()
}

// IndexSQL is the canonical CREATE INDEX statement for an index.
func IndexSQL(t *Table, ix *Index) string {
	u := ""
	if ix.Unique {
		u = "UNIQUE "
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", u, quoteIdent(ix.Name), quoteIdent(t.Name), t.columnList(ix.Columns))
}

func (t *Table) columnList(cols []int) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(t.Columns[c].Name)
	}
	return strings.Join(names, ", ")
}

func quoteIdent(s string) string {
	plain := s != ""
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			plain = false
		}
	}
	if plain && !isReservedWord(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func isReservedWord(s string) bool {
	_, err := sql.ParseExpr(s)
	if err != nil {
		return true
	}
	e, _ := sql.ParseExpr(s)
	ref, ok := e.(*sql.ColumnRef)
	return !ok || ref.Name != s
}

// schemaRecord is how an object is stored in the schema tree.
func schemaRecord(kind, name, table string, root uint32, def string) []byte {
	return value.EncodeRecord([]value.Value{
		value.NewText(kind), value.NewText(name), value.NewText(table), value.NewInt(int64(root)), value.NewText(def),
	})
}

func schemaKey(name string) []byte { return value.EncodeKey(value.NewText(strings.ToLower(name))) }

// loadCatalog reads the schema tree.
func loadCatalog(r btree.Reader) (*Catalog, error) {
	cat := newCatalog()
	type pendingIndex struct {
		def  string
		root uint32
		auto bool
	}
	var indexes []pendingIndex
	c := btree.NewCursor(r, schemaRoot)
	for c.First(); c.Valid(); c.Next() {
		raw, err := c.Value()
		if err != nil {
			return nil, err
		}
		rec, err := value.DecodeRecord(raw)
		if err != nil || len(rec) != 5 {
			return nil, fmt.Errorf("corrupt schema entry %q", c.Key())
		}
		kind, def, root := rec[0].S, rec[4].S, uint32(rec[3].I)
		switch kind {
		case "table":
			stmt, err := sql.ParseOne(def)
			if err != nil {
				return nil, fmt.Errorf("schema: %w", err)
			}
			t, _, err := tableFromAST(stmt.(*sql.CreateTable))
			if err != nil {
				return nil, fmt.Errorf("schema: %w", err)
			}
			t.Root = root
			cat.tables[strings.ToLower(t.Name)] = t
		case "index", "autoindex":
			indexes = append(indexes, pendingIndex{def, root, kind == "autoindex"})
		default:
			return nil, fmt.Errorf("schema: unknown object kind %q", kind)
		}
	}
	if err := c.Err(); err != nil {
		return nil, err
	}
	for _, pi := range indexes {
		stmt, err := sql.ParseOne(pi.def)
		if err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
		ci := stmt.(*sql.CreateIndex)
		t := cat.Table(ci.Table)
		if t == nil {
			return nil, fmt.Errorf("schema: index %s on missing table %s", ci.Name, ci.Table)
		}
		ix := &Index{Name: ci.Name, Table: t.Name, Unique: ci.Unique, Root: pi.root, Auto: pi.auto}
		for _, name := range ci.Columns {
			col := t.column(name)
			if col < 0 {
				return nil, fmt.Errorf("schema: index %s names missing column %s", ci.Name, name)
			}
			ix.Columns = append(ix.Columns, col)
		}
		t.Indexes = append(t.Indexes, ix)
		cat.indexes[strings.ToLower(ix.Name)] = ix
	}
	for _, t := range cat.tables {
		sort.Slice(t.Indexes, func(i, j int) bool { return t.Indexes[i].Name < t.Indexes[j].Name })
	}
	return cat, nil
}

// tableFromAST validates a CREATE TABLE and builds the table. It also
// returns the unique constraints (column lists) that need automatic
// indexes.
func tableFromAST(ct *sql.CreateTable) (*Table, [][]int, error) {
	t := &Table{Name: ct.Name, RowidCol: -1}
	if len(ct.Columns) == 0 {
		return nil, nil, fmt.Errorf("table %s needs at least one column", ct.Name)
	}
	var uniques [][]int
	var pkCols []int
	for i, cd := range ct.Columns {
		if t.column(cd.Name) >= 0 {
			return nil, nil, fmt.Errorf("duplicate column %s in table %s", cd.Name, ct.Name)
		}
		if cd.Default != nil {
			if err := checkConstant(cd.Default); err != nil {
				return nil, nil, fmt.Errorf("default for %s: %w", cd.Name, err)
			}
		}
		t.Columns = append(t.Columns, Column{Name: cd.Name, Type: cd.Type, NotNull: cd.NotNull, Default: cd.Default})
		if cd.PrimaryKey {
			pkCols = append(pkCols, i)
		}
		if cd.Unique {
			uniques = append(uniques, []int{i})
		}
	}
	if len(pkCols) > 1 {
		return nil, nil, fmt.Errorf("table %s has more than one PRIMARY KEY column; use PRIMARY KEY (a, b)", ct.Name)
	}
	if ct.PrimaryKey != nil {
		if len(pkCols) > 0 {
			return nil, nil, fmt.Errorf("table %s has two primary keys", ct.Name)
		}
		for _, name := range ct.PrimaryKey {
			c := t.column(name)
			if c < 0 {
				return nil, nil, fmt.Errorf("PRIMARY KEY names unknown column %s", name)
			}
			pkCols = append(pkCols, c)
		}
	}
	for _, u := range ct.Uniques {
		var cols []int
		for _, name := range u {
			c := t.column(name)
			if c < 0 {
				return nil, nil, fmt.Errorf("UNIQUE names unknown column %s", name)
			}
			cols = append(cols, c)
		}
		uniques = append(uniques, cols)
	}
	t.PrimaryKey = pkCols
	if len(pkCols) == 1 && t.Columns[pkCols[0]].Type == value.Int {
		// A single INTEGER PRIMARY KEY is the rowid itself.
		t.RowidCol = pkCols[0]
	} else if len(pkCols) > 0 {
		for _, c := range pkCols {
			t.Columns[c].NotNull = true
		}
		uniques = append([][]int{pkCols}, uniques...)
	}
	return t, uniques, nil
}

// checkConstant rejects defaults that depend on columns or parameters.
func checkConstant(e sql.Expr) error {
	var bad error
	walkExpr(e, func(x sql.Expr) {
		switch x.(type) {
		case *sql.ColumnRef:
			bad = fmt.Errorf("must not refer to columns")
		case *sql.Param:
			bad = fmt.Errorf("must not use parameters")
		case *sql.Subquery, *sql.Exists, *sql.In:
			if in, ok := x.(*sql.In); !ok || in.Select != nil {
				bad = fmt.Errorf("must not use subqueries")
			}
		}
	})
	return bad
}

// walkExpr calls fn for e and every expression inside it (not descending
// into subqueries).
func walkExpr(e sql.Expr, fn func(sql.Expr)) {
	if e == nil {
		return
	}
	fn(e)
	switch x := e.(type) {
	case *sql.Unary:
		walkExpr(x.X, fn)
	case *sql.Binary:
		walkExpr(x.L, fn)
		walkExpr(x.R, fn)
	case *sql.FuncCall:
		for _, a := range x.Args {
			walkExpr(a, fn)
		}
	case *sql.Case:
		walkExpr(x.Operand, fn)
		for _, w := range x.Whens {
			walkExpr(w.Cond, fn)
			walkExpr(w.Result, fn)
		}
		walkExpr(x.Else, fn)
	case *sql.In:
		walkExpr(x.X, fn)
		for _, a := range x.List {
			walkExpr(a, fn)
		}
	case *sql.Between:
		walkExpr(x.X, fn)
		walkExpr(x.Lo, fn)
		walkExpr(x.Hi, fn)
	case *sql.Like:
		walkExpr(x.X, fn)
		walkExpr(x.Pattern, fn)
	case *sql.IsNull:
		walkExpr(x.X, fn)
	case *sql.Is:
		walkExpr(x.L, fn)
		walkExpr(x.R, fn)
	case *sql.Cast:
		walkExpr(x.X, fn)
	}
}
