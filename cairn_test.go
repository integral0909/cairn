package cairn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustExec(t *testing.T, db interface {
	Exec(string, ...any) (Result, error)
}, q string, args ...any) Result {
	t.Helper()
	res, err := db.Exec(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res
}

func queryStrings(t *testing.T, db *DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		out = append(out, strings.Join(rows.Strings(), "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkClean(t *testing.T, db *DB) {
	t.Helper()
	problems, err := db.Check()
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("integrity check failed:\n%s", strings.Join(problems, "\n"))
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL, tag TEXT DEFAULT ('t' || 'x'), UNIQUE (body))`)
	mustExec(t, db, `CREATE INDEX notes_tag ON notes (tag)`)
	for i := 0; i < 500; i++ {
		mustExec(t, db, `INSERT INTO notes (body) VALUES (?)`, fmt.Sprintf("note %d %s", i, strings.Repeat("x", i%300)))
	}
	mustExec(t, db, `ALTER TABLE notes ADD COLUMN stars INTEGER DEFAULT 3`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := queryStrings(t, db, `SELECT count(*), min(tag), max(stars) FROM notes WHERE tag = 'tx'`)
	if got[0] != "500|tx|3" {
		t.Fatalf("after reopen: %v", got)
	}
	if _, err := db.Exec(`INSERT INTO notes (body) VALUES ('note 7 xxxxxxx')`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("unique constraint lost after reopen: %v", err)
	}
	checkClean(t, db)
	schema := strings.Join(db.Schema(), "\n")
	if !strings.Contains(schema, "CREATE INDEX notes_tag ON notes (tag)") || !strings.Contains(schema, "DEFAULT ('t' || 'x')") {
		t.Fatalf("schema: %s", schema)
	}
}

func TestTransactions(t *testing.T) {
	db, _ := Open(Memory, nil)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE acct (name TEXT PRIMARY KEY, balance INTEGER NOT NULL)`)
	mustExec(t, db, `INSERT INTO acct VALUES ('a', 100), ('b', 0)`)

	c := db.Conn()
	defer c.Close()
	mustExec(t, c, `BEGIN`)
	mustExec(t, c, `UPDATE acct SET balance = balance - 30 WHERE name = 'a'`)
	// A failing statement is undone, but the transaction carries on.
	if _, err := c.Exec(`INSERT INTO acct VALUES ('a', 1)`); err == nil {
		t.Fatal("expected a constraint error")
	}
	mustExec(t, c, `UPDATE acct SET balance = balance + 30 WHERE name = 'b'`)
	// Other sessions do not see uncommitted changes.
	if got := queryStrings(t, db, `SELECT balance FROM acct ORDER BY name`); strings.Join(got, ",") != "100,0" {
		t.Fatalf("uncommitted changes leaked: %v", got)
	}
	mustExec(t, c, `COMMIT`)
	if got := queryStrings(t, db, `SELECT balance FROM acct ORDER BY name`); strings.Join(got, ",") != "70,30" {
		t.Fatalf("after commit: %v", got)
	}

	mustExec(t, c, `BEGIN`)
	mustExec(t, c, `DELETE FROM acct`)
	mustExec(t, c, `CREATE TABLE scratch (x INTEGER)`)
	mustExec(t, c, `ROLLBACK`)
	if got := queryStrings(t, db, `SELECT count(*) FROM acct`); got[0] != "2" {
		t.Fatalf("rollback lost: %v", got)
	}
	if _, err := db.Exec(`SELECT * FROM scratch`); err == nil {
		t.Fatal("a rolled back CREATE TABLE must disappear")
	}
	if _, err := c.Exec(`COMMIT`); err == nil {
		t.Fatal("COMMIT without BEGIN should fail")
	}
}

// A reader's snapshot is unaffected by commits made while it reads.
func TestSnapshotReads(t *testing.T) {
	db, _ := Open(Memory, nil)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE n (v INTEGER)`)
	for i := 0; i < 100; i++ {
		mustExec(t, db, `INSERT INTO n VALUES (?)`, i)
	}
	rows, err := db.Query(`SELECT v FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	mustExec(t, db, `DELETE FROM n WHERE v > 10`)
	mustExec(t, db, `INSERT INTO n VALUES (1000)`)
	count := 1
	for rows.Next() {
		count++
	}
	if count != 100 {
		t.Fatalf("snapshot saw %d rows, want 100", count)
	}
	if got := queryStrings(t, db, `SELECT count(*) FROM n`); got[0] != "12" {
		t.Fatalf("new readers see the change: %v", got)
	}
}

func TestDatabaseSQLDriver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "driver.db")
	db, err := sql.Open("cairn", path+"?nosync=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score REAL, ok BOOLEAN, data BLOB)`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO t (name, score, ok, data) VALUES (?, ?, ?, ?)`, "ada", 9.5, true, []byte{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 1 {
		t.Fatalf("LastInsertId = %d", id)
	}
	var (
		name  string
		score float64
		ok    bool
		data  []byte
		null  sql.NullString
	)
	if err := db.QueryRow(`SELECT name, score, ok, data, NULL FROM t WHERE id = $1`, 1).Scan(&name, &score, &ok, &data, &null); err != nil {
		t.Fatal(err)
	}
	if name != "ada" || score != 9.5 || !ok || len(data) != 2 || null.Valid {
		t.Fatalf("scanned %q %v %v %v %v", name, score, ok, data, null)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO t (name) VALUES (?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := stmt.Exec(fmt.Sprint("n", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM t`).Scan(&n)
	if n != 1 {
		t.Fatalf("rolled back rows are visible: %d", n)
	}
	if _, err := db.Exec(`SELECT * FROM t WHERE id = :id`, sql.Named("id", 1)); err == nil {
		t.Fatal("named parameters should be refused")
	}
	if _, err := db.Exec(`SELECT ?`); err == nil {
		t.Fatal("missing parameter should fail")
	}
}

func TestContextCancellation(t *testing.T) {
	db, _ := Open(Memory, nil)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE a (x INTEGER)`)
	for i := 0; i < 200; i++ {
		mustExec(t, db, `INSERT INTO a VALUES (?)`, i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := db.Conn().WithContext(ctx)
	defer c.Close()
	// A 200^3-row cross join stops promptly once the context is done.
	rows, err := c.Query(`SELECT count(*) FROM a, a AS b, a AS c`)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentSessions(t *testing.T) {
	db, _ := Open(Memory, &Options{NoSync: true})
	defer db.Close()
	mustExec(t, db, `CREATE TABLE counter (id INTEGER PRIMARY KEY, n INTEGER)`)
	mustExec(t, db, `INSERT INTO counter VALUES (1, 0)`)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := db.Conn()
			defer c.Close()
			for i := 0; i < 50; i++ {
				if _, err := c.Exec(`UPDATE counter SET n = n + 1 WHERE id = 1`); err != nil {
					errs <- err
					return
				}
				rows, err := c.Query(`SELECT n FROM counter`)
				if err != nil {
					errs <- err
					return
				}
				for rows.Next() {
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := queryStrings(t, db, `SELECT n FROM counter`); got[0] != "400" {
		t.Fatalf("lost updates: %v", got)
	}
}

// Random changes to an indexed table: every query must give the same
// answer through the index as through a full scan (x + 0 cannot use the
// index), and the indexes must match the table afterwards.
func TestIndexesAgreeWithScans(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	db, _ := Open(Memory, &Options{NoSync: true})
	defer db.Close()
	mustExec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, x INTEGER, y TEXT, z REAL)`)
	mustExec(t, db, `CREATE INDEX t_x ON t (x)`)
	mustExec(t, db, `CREATE INDEX t_yx ON t (y, x)`)
	words := []string{"a", "b", "c", "dd", ""}
	for step := 0; step < 1500; step++ {
		switch r.Intn(4) {
		case 0, 1:
			var y any = words[r.Intn(len(words))]
			if r.Intn(8) == 0 {
				y = nil
			}
			mustExec(t, db, `INSERT INTO t (x, y, z) VALUES (?, ?, ?)`, r.Intn(50)-10, y, r.Float64()*10)
		case 2:
			mustExec(t, db, `UPDATE t SET x = ?, y = ? WHERE id = ?`, r.Intn(50)-10, words[r.Intn(len(words))], r.Intn(step+1)+1)
		case 3:
			mustExec(t, db, `DELETE FROM t WHERE x = ?`, r.Intn(50)-10)
		}
		if step%100 == 0 {
			for k := 0; k < 20; k++ {
				v := r.Intn(50) - 10
				w := words[r.Intn(len(words))]
				pairs := [][2]string{
					{`SELECT count(*), sum(id) FROM t WHERE x = ?`, `SELECT count(*), sum(id) FROM t WHERE x + 0 = ?`},
					{`SELECT count(*), sum(id) FROM t WHERE x > ?`, `SELECT count(*), sum(id) FROM t WHERE x + 0 > ?`},
					{`SELECT count(*), sum(id) FROM t WHERE x <= ?`, `SELECT count(*), sum(id) FROM t WHERE x + 0 <= ?`},
				}
				for _, p := range pairs {
					a := queryStrings(t, db, p[0], v)
					b := queryStrings(t, db, p[1], v)
					if a[0] != b[0] {
						t.Fatalf("step %d: %s with %d: index says %s, scan says %s", step, p[0], v, a[0], b[0])
					}
				}
				a := queryStrings(t, db, `SELECT count(*) FROM t WHERE y = ? AND x >= ?`, w, v)
				b := queryStrings(t, db, `SELECT count(*) FROM t WHERE y || '' = ? AND x + 0 >= ?`, w, v)
				if a[0] != b[0] {
					t.Fatalf("composite index disagrees: %s vs %s", a[0], b[0])
				}
			}
		}
	}
	checkClean(t, db)
}

func TestScanTypes(t *testing.T) {
	db, _ := Open(Memory, nil)
	defer db.Close()
	rows, err := db.Query(`SELECT 1, 2.5, 'x', x'00', true, NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	rows.Next()
	var i int64
	var f float64
	var s string
	var b []byte
	var ok bool
	var n any
	if err := rows.Scan(&i, &f, &s, &b, &ok, &n); err != nil {
		t.Fatal(err)
	}
	if i != 1 || f != 2.5 || s != "x" || len(b) != 1 || !ok || n != nil {
		t.Fatal("scan mismatch")
	}
	if err := rows.Scan(&i, &f, &s, &b, &ok, &s); err == nil {
		t.Fatal("scanning NULL into a string should fail")
	}
}

func TestLargeValuesAndManyRows(t *testing.T) {
	db, _ := Open(Memory, &Options{NoSync: true})
	defer db.Close()
	mustExec(t, db, `CREATE TABLE docs (id INTEGER PRIMARY KEY, body TEXT)`)
	big := strings.Repeat("lorem ipsum ", 20000) // 240 KB
	mustExec(t, db, `INSERT INTO docs (body) VALUES (?)`, big)
	c := db.Conn()
	mustExec(t, c, `BEGIN`)
	for i := 0; i < 20000; i++ {
		mustExec(t, c, `INSERT INTO docs (body) VALUES (?)`, fmt.Sprint(i))
	}
	mustExec(t, c, `COMMIT`)
	c.Close()
	if got := queryStrings(t, db, `SELECT length(body) FROM docs WHERE id = 1`); got[0] != fmt.Sprint(len(big)) {
		t.Fatalf("big value: %v", got)
	}
	if got := queryStrings(t, db, `SELECT count(*) FROM docs`); got[0] != "20001" {
		t.Fatalf("count: %v", got)
	}
	start := time.Now()
	queryStrings(t, db, `SELECT body FROM docs WHERE id = 15000`)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("rowid lookup took %v", time.Since(start))
	}
	checkClean(t, db)
}
