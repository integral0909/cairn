package sql

import "github.com/integral0909/cairn/internal/value"

// Stmt is a parsed statement.
type Stmt interface{ stmt() }

// Type is a column type.
type Type = value.Kind

type ColumnDef struct {
	Name       string
	Type       Type
	NotNull    bool
	PrimaryKey bool
	Unique     bool
	Default    Expr
}

type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	PrimaryKey  []string   // table-level PRIMARY KEY (...)
	Uniques     [][]string // table-level UNIQUE (...)
}

type DropTable struct {
	Name     string
	IfExists bool
}

type CreateIndex struct {
	Name        string
	Table       string
	Columns     []string
	Unique      bool
	IfNotExists bool
}

type DropIndex struct {
	Name     string
	IfExists bool
}

type AlterTable struct {
	Table     string
	AddColumn *ColumnDef
	RenameTo  string
}

type Insert struct {
	Table      string
	Columns    []string
	Rows       [][]Expr
	Select     *Select
	OnConflict string // "", "nothing" or "replace"
	Returning  []ResultCol
}

type Assignment struct {
	Column string
	Value  Expr
}

type Update struct {
	Table     string
	Set       []Assignment
	Where     Expr
	Returning []ResultCol
}

type Delete struct {
	Table     string
	Where     Expr
	Returning []ResultCol
}

// Select is a query, possibly a compound (UNION and friends) chain.
type Select struct {
	Distinct bool
	Columns  []ResultCol
	From     FromItem
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr
	// Compound queries: this select, then op, then Next.
	CompoundOp string // "UNION", "UNION ALL", "INTERSECT", "EXCEPT"
	Next       *Select
}

// ResultCol is one item of a select list: an expression, * or table.*.
type ResultCol struct {
	Expr  Expr
	Alias string
	Star  bool
	Table string // for table.*
}

type OrderItem struct {
	Expr       Expr
	Desc       bool
	NullsFirst *bool
}

type Begin struct{}
type Commit struct{}
type Rollback struct{}
type Analyze struct{ Table string }

type Explain struct{ Stmt Stmt }

func (*CreateTable) stmt() {}
func (*DropTable) stmt()   {}
func (*CreateIndex) stmt() {}
func (*DropIndex) stmt()   {}
func (*AlterTable) stmt()  {}
func (*Insert) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
func (*Select) stmt()      {}
func (*Begin) stmt()       {}
func (*Commit) stmt()      {}
func (*Rollback) stmt()    {}
func (*Explain) stmt()     {}
func (*Analyze) stmt()     {}

// FromItem is a table, a join or a subquery in FROM.
type FromItem interface{ from() }

type TableRef struct {
	Name  string
	Alias string
}

type JoinKind int

const (
	InnerJoin JoinKind = iota
	LeftJoin
	CrossJoin
)

type Join struct {
	Kind        JoinKind
	Left, Right FromItem
	On          Expr
	Using       []string
}

type SubqueryRef struct {
	Select *Select
	Alias  string
}

func (*TableRef) from()    {}
func (*Join) from()        {}
func (*SubqueryRef) from() {}

// Expr is an expression.
type Expr interface{ expr() }

type Literal struct{ Value value.Value }

type Param struct{ N int } // 1-based

type ColumnRef struct {
	Table string // may be empty
	Name  string
}

type Unary struct {
	Op string // "-", "+", "NOT"
	X  Expr
}

type Binary struct {
	Op   string // + - * / % || = != < <= > >= AND OR
	L, R Expr
}

type FuncCall struct {
	Name     string // lowercase
	Args     []Expr
	Star     bool // count(*)
	Distinct bool
}

type When struct{ Cond, Result Expr }

type Case struct {
	Operand Expr // CASE x WHEN ... (may be nil)
	Whens   []When
	Else    Expr
}

type In struct {
	X      Expr
	List   []Expr
	Select *Select
	Not    bool
}

type Between struct {
	X, Lo, Hi Expr
	Not       bool
}

type Like struct {
	X, Pattern Expr
	Not        bool
	Glob       bool
}

type IsNull struct {
	X   Expr
	Not bool
}

// Is is "x IS y" / "x IS NOT y": equality where NULL equals NULL.
type Is struct {
	L, R Expr
	Not  bool
}

type Cast struct {
	X    Expr
	Type Type
}

type Exists struct {
	Select *Select
	Not    bool
}

// Subquery is a scalar subquery: its single value.
type Subquery struct{ Select *Select }

func (*Literal) expr()   {}
func (*Param) expr()     {}
func (*ColumnRef) expr() {}
func (*Unary) expr()     {}
func (*Binary) expr()    {}
func (*FuncCall) expr()  {}
func (*Case) expr()      {}
func (*In) expr()        {}
func (*Between) expr()   {}
func (*Like) expr()      {}
func (*IsNull) expr()    {}
func (*Is) expr()        {}
func (*Cast) expr()      {}
func (*Exists) expr()    {}
func (*Subquery) expr()  {}
