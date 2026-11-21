package sql

import (
	"strings"
	"testing"

	"github.com/integral0909/cairn/internal/value"
)

func one(t *testing.T, src string) Stmt {
	t.Helper()
	s, err := ParseOne(src)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return s
}

func TestCreateTable(t *testing.T) {
	ct := one(t, `CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		"Display Name" VARCHAR(80) DEFAULT 'anon',
		score REAL DEFAULT -1.5,
		active BOOLEAN DEFAULT true,
		key TEXT,
		UNIQUE (email, score)
	)`).(*CreateTable)
	if ct.Name != "users" || !ct.IfNotExists || len(ct.Columns) != 6 {
		t.Fatalf("%+v", ct)
	}
	c := ct.Columns
	if !c[0].PrimaryKey || c[0].Type != value.Int {
		t.Error("id should be an INTEGER PRIMARY KEY")
	}
	if !c[1].NotNull || !c[1].Unique || c[1].Type != value.Text {
		t.Error("email constraints")
	}
	if c[2].Name != "Display Name" || FormatExpr(c[2].Default) != "'anon'" {
		t.Errorf("quoted column: %+v", c[2])
	}
	if FormatExpr(c[3].Default) != "-1.5" {
		t.Errorf("negative default folded: %s", FormatExpr(c[3].Default))
	}
	if c[5].Name != "key" {
		t.Error("non-reserved keywords work as column names")
	}
	if len(ct.Uniques) != 1 || strings.Join(ct.Uniques[0], ",") != "email,score" {
		t.Error("table-level UNIQUE")
	}
}

func TestSelectShape(t *testing.T) {
	s := one(t, `SELECT DISTINCT u.name, count(*) AS n, o.*
		FROM users u JOIN orders AS o ON o.user_id = u.id LEFT JOIN items USING (sku)
		WHERE u.age BETWEEN 18 AND 65 AND u.name NOT LIKE 'a%' AND o.total IS NOT NULL
		GROUP BY u.name HAVING count(*) > 2
		ORDER BY n DESC NULLS LAST, 1
		LIMIT 10 OFFSET 20`).(*Select)
	if !s.Distinct || len(s.Columns) != 3 || s.Columns[1].Alias != "n" || !s.Columns[2].Star || s.Columns[2].Table != "o" {
		t.Fatalf("columns: %+v", s.Columns)
	}
	j, ok := s.From.(*Join)
	if !ok || j.Kind != LeftJoin || strings.Join(j.Using, ",") != "sku" {
		t.Fatalf("outer join: %#v", s.From)
	}
	inner := j.Left.(*Join)
	if inner.Kind != InnerJoin || inner.Right.(*TableRef).Alias != "o" {
		t.Fatal("inner join")
	}
	if FormatExpr(s.Where) != "u.age BETWEEN 18 AND 65 AND u.name NOT LIKE 'a%' AND o.total IS NOT NULL" {
		t.Errorf("where: %s", FormatExpr(s.Where))
	}
	if len(s.OrderBy) != 2 || !s.OrderBy[0].Desc || *s.OrderBy[0].NullsFirst {
		t.Error("order by")
	}
	if FormatExpr(s.Limit) != "10" || FormatExpr(s.Offset) != "20" {
		t.Error("limit/offset")
	}
}

func TestPrecedence(t *testing.T) {
	cases := map[string]string{
		"1 + 2 * 3":              "1 + 2 * 3",
		"(1 + 2) * 3":            "(1 + 2) * 3",
		"a OR b AND c":           "a OR b AND c",
		"(a OR b) AND c":         "(a OR b) AND c",
		"NOT a = b":              "NOT a = b",
		"a || b = c":             "a || b = c",
		"-x * 2":                 "-x * 2",
		"1 - (2 - 3)":            "1 - (2 - 3)",
		"x IN (1, 2) OR y IS z":  "x IN (1, 2) OR y IS z",
		"CASE WHEN a THEN 1 END": "CASE WHEN a THEN 1 END",
		"cast(x AS real)":        "CAST(x AS REAL)",
		"count(DISTINCT x)":      "count(DISTINCT x)",
		"replace(a, 'x', 'y')":   "replace(a, 'x', 'y')",
		"-9223372036854775808":   "-9223372036854775808",
		"x NOT BETWEEN 1 AND 2":  "x NOT BETWEEN 1 AND 2",
	}
	for src, want := range cases {
		e, err := ParseExpr(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got := FormatExpr(e); got != want {
			t.Errorf("%s formats as %s, want %s", src, got, want)
		}
	}
}

func TestLiteralsAndParams(t *testing.T) {
	s := one(t, `INSERT INTO t VALUES (1, 2.5e3, 'it''s', x'00ff', NULL, TRUE, ?, ?, $1)`).(*Insert)
	row := s.Rows[0]
	want := []string{"1", "2500.0", "'it''s'", "x'00ff'", "NULL", "true", "$1", "$2", "$1"}
	for i, e := range row {
		if FormatExpr(e) != want[i] {
			t.Errorf("value %d: %s, want %s", i, FormatExpr(e), want[i])
		}
	}
	if NumParams(`SELECT ?, ?, $5`) != 5 {
		t.Error("NumParams")
	}
	if e, _ := ParseExpr("99999999999999999999"); e.(*Literal).Value.K != value.Real {
		t.Error("integers too large for INTEGER become REAL")
	}
}

func TestStatementsAndScripts(t *testing.T) {
	stmts, err := Parse(`
		-- a script
		BEGIN;
		CREATE UNIQUE INDEX IF NOT EXISTS by_email ON users (email);
		UPDATE users SET score = score + 1, name = upper(name) WHERE id = 3 RETURNING id, score;
		DELETE FROM users WHERE id IN (SELECT id FROM banned);
		INSERT OR REPLACE INTO t (a) SELECT x FROM u UNION ALL SELECT 1;
		INSERT INTO t (a) VALUES (1) ON CONFLICT DO NOTHING;
		ALTER TABLE users ADD COLUMN bio TEXT;
		ALTER TABLE users RENAME TO people;
		DROP INDEX IF EXISTS by_email; DROP TABLE people;
		/* done */ COMMIT;
		EXPLAIN SELECT 1;
		SELECT * FROM (SELECT 1 AS x) AS sub WHERE EXISTS (SELECT 1) AND x = (SELECT max(y) FROM z);
		VALUES (1, 'a'), (2, 'b');
		ANALYZE;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 15 {
		t.Fatalf("parsed %d statements", len(stmts))
	}
	if ins := stmts[4].(*Insert); ins.OnConflict != "replace" || ins.Select.Next == nil {
		t.Error("INSERT OR REPLACE ... SELECT ... UNION ALL")
	}
	if ins := stmts[5].(*Insert); ins.OnConflict != "nothing" {
		t.Error("ON CONFLICT DO NOTHING")
	}
	if u := stmts[2].(*Update); len(u.Set) != 2 || len(u.Returning) != 2 {
		t.Error("UPDATE ... RETURNING")
	}
	if v := stmts[13].(*Select); v.Next == nil || v.Columns[1].Alias != "column2" {
		t.Error("VALUES as a query")
	}
}

func TestSyntaxErrorsPointAtTheProblem(t *testing.T) {
	cases := map[string]string{
		"SELECT FROM t":                    "line 1, column 8: expected an expression",
		"SELECT 1\n  FROM":                 "line 2, column 7: expected a table name",
		"CREATE TABLE t (a VARCHAR2)":      "unknown column type",
		"SELECT 'unterminated":             "unterminated string",
		"SELECT * FROM a RIGHT JOIN b":     "only INNER, LEFT and CROSS joins",
		"SELECT 1 2":                       "expected ; or end of input",
		"SELECT * FROM a FULL JOIN b ON x": "only INNER, LEFT and CROSS joins",
		"SELECT 12abc":                     "malformed number",
		"SELECT * FROM (SELECT 1)":         "needs a name",
		"SELECT x'abc'":                    "even number of hex digits",
		"SELECT 1 # 2":                     "unexpected character",
		"SELECT d\xff":                     "unexpected character",
		"SELECT 1 € 2":                     "unexpected character",
		"EXPLAIN EXPLAIN SELECT 1":         "cannot be nested",
		"INSERT INTO t":                    "expected VALUES or SELECT",
	}
	for src, want := range cases {
		_, err := Parse(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", src, err, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"SELECT a, b FROM t WHERE x > 1 ORDER BY a", "INSERT INTO t VALUES (1, 'x')",
		"CREATE TABLE t (a INTEGER PRIMARY KEY)", "SELECT CASE WHEN a THEN b ELSE c END FROM t", "x'00'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		stmts, err := Parse(src)
		if err != nil {
			return
		}
		// Every parsed select's expressions must format without panicking.
		for _, s := range stmts {
			if sel, ok := s.(*Select); ok && sel.Where != nil {
				_ = FormatExpr(sel.Where)
			}
		}
	})
}
