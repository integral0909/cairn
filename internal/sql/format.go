package sql

import (
	"strconv"
	"strings"
)

// FormatExpr renders an expression back as SQL. It names result columns
// (SELECT count(*) gives a column called "count(*)") and prints plans.
func FormatExpr(e Expr) string {
	var b strings.Builder
	writeExpr(&b, e, 0)
	return b.String()
}

// Binding strength, used to add parentheses only where needed.
func prec(e Expr) int {
	switch x := e.(type) {
	case *Binary:
		switch x.Op {
		case "OR":
			return 1
		case "AND":
			return 2
		case "=", "!=", "<", "<=", ">", ">=":
			return 4
		case "||":
			return 5
		case "+", "-":
			return 6
		default:
			return 7
		}
	case *Unary:
		if x.Op == "NOT" {
			return 3
		}
		return 8
	case *In, *Between, *Like, *IsNull, *Is:
		return 4
	}
	return 9
}

func writeExpr(b *strings.Builder, e Expr, outer int) {
	p := prec(e)
	if p < outer {
		b.WriteByte('(')
		defer b.WriteByte(')')
	}
	switch x := e.(type) {
	case *Literal:
		b.WriteString(x.Value.Literal())
	case *Param:
		b.WriteString("$" + strconv.Itoa(x.N))
	case *ColumnRef:
		if x.Table != "" {
			b.WriteString(x.Table + ".")
		}
		b.WriteString(x.Name)
	case *Unary:
		if x.Op == "NOT" {
			b.WriteString("NOT ")
		} else {
			b.WriteString(x.Op)
		}
		writeExpr(b, x.X, p)
	case *Binary:
		writeExpr(b, x.L, p)
		b.WriteString(" " + x.Op + " ")
		writeExpr(b, x.R, p+1)
	case *FuncCall:
		b.WriteString(x.Name + "(")
		if x.Star {
			b.WriteByte('*')
		}
		if x.Distinct {
			b.WriteString("DISTINCT ")
		}
		for i, a := range x.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, a, 0)
		}
		b.WriteByte(')')
	case *Case:
		b.WriteString("CASE")
		if x.Operand != nil {
			b.WriteByte(' ')
			writeExpr(b, x.Operand, 0)
		}
		for _, w := range x.Whens {
			b.WriteString(" WHEN ")
			writeExpr(b, w.Cond, 0)
			b.WriteString(" THEN ")
			writeExpr(b, w.Result, 0)
		}
		if x.Else != nil {
			b.WriteString(" ELSE ")
			writeExpr(b, x.Else, 0)
		}
		b.WriteString(" END")
	case *In:
		writeExpr(b, x.X, p+1)
		if x.Not {
			b.WriteString(" NOT")
		}
		b.WriteString(" IN (")
		if x.Select != nil {
			b.WriteString("SELECT ...")
		}
		for i, a := range x.List {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, a, 0)
		}
		b.WriteByte(')')
	case *Between:
		writeExpr(b, x.X, p+1)
		if x.Not {
			b.WriteString(" NOT")
		}
		b.WriteString(" BETWEEN ")
		writeExpr(b, x.Lo, p+1)
		b.WriteString(" AND ")
		writeExpr(b, x.Hi, p+1)
	case *Like:
		writeExpr(b, x.X, p+1)
		if x.Not {
			b.WriteString(" NOT")
		}
		if x.Glob {
			b.WriteString(" GLOB ")
		} else {
			b.WriteString(" LIKE ")
		}
		writeExpr(b, x.Pattern, p+1)
	case *IsNull:
		writeExpr(b, x.X, p+1)
		if x.Not {
			b.WriteString(" IS NOT NULL")
		} else {
			b.WriteString(" IS NULL")
		}
	case *Is:
		writeExpr(b, x.L, p+1)
		if x.Not {
			b.WriteString(" IS NOT ")
		} else {
			b.WriteString(" IS ")
		}
		writeExpr(b, x.R, p+1)
	case *Cast:
		b.WriteString("CAST(")
		writeExpr(b, x.X, 0)
		b.WriteString(" AS " + x.Type.String() + ")")
	case *Exists:
		if x.Not {
			b.WriteString("NOT ")
		}
		b.WriteString("EXISTS (SELECT ...)")
	case *Subquery:
		b.WriteString("(SELECT ...)")
	default:
		b.WriteString("?")
	}
}
