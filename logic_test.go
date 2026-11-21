package cairn

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestSQLLogic runs the scripts in testdata/sql. Each record is separated
// by a blank line:
//
//	statement ok              the statement must succeed
//	statement error <text>    it must fail with an error containing text
//	query [rowsort]           rows follow "----", values joined by " | "
//	                          (rowsort compares them sorted)
//
// Every file runs in a fresh in-memory database on one connection.
func TestSQLLogic(t *testing.T) {
	files, err := filepath.Glob("testdata/sql/*.test")
	if err != nil || len(files) == 0 {
		t.Fatal("no test scripts found")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) { runScript(t, file) })
	}
}

type record struct {
	line   int
	kind   string // statement or query
	arg    string // "ok", "error <text>", "rowsort"
	sql    string
	expect []string
}

func parseScript(t *testing.T, file string) []record {
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []record
	var cur *record
	inResult := false
	sc := bufio.NewScanner(f)
	n := 0
	flush := func() {
		if cur != nil {
			recs = append(recs, *cur)
			cur, inResult = nil, false
		}
	}
	for sc.Scan() {
		n++
		line := sc.Text()
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case cur == nil && strings.HasPrefix(line, "#"):
		case cur == nil:
			kind, arg, _ := strings.Cut(line, " ")
			if kind != "statement" && kind != "query" {
				t.Fatalf("%s:%d: expected statement or query, got %q", file, n, line)
			}
			cur = &record{line: n, kind: kind, arg: arg}
		case line == "----":
			inResult = true
		case inResult:
			cur.expect = append(cur.expect, line)
		default:
			if cur.sql != "" {
				cur.sql += "\n"
			}
			cur.sql += line
		}
	}
	flush()
	return recs
}

func runScript(t *testing.T, file string) {
	db, err := Open(Memory, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	defer conn.Close()
	for _, r := range parseScript(t, file) {
		where := file + ":" + strconv.Itoa(r.line)
		switch r.kind {
		case "statement":
			_, err := conn.Exec(r.sql)
			if r.arg == "ok" {
				if err != nil {
					t.Errorf("%s: %s\n  unexpected error: %v", where, r.sql, err)
				}
				continue
			}
			want := strings.TrimSpace(strings.TrimPrefix(r.arg, "error"))
			if err == nil {
				t.Errorf("%s: %s\n  expected an error containing %q", where, r.sql, want)
			} else if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %s\n  error %q does not contain %q", where, r.sql, err, want)
			}
		case "query":
			rows, err := conn.Query(r.sql)
			if err != nil {
				t.Errorf("%s: %s\n  query failed: %v", where, r.sql, err)
				continue
			}
			var got []string
			for rows.Next() {
				got = append(got, strings.Join(rows.Strings(), " | "))
			}
			if err := rows.Err(); err != nil {
				t.Errorf("%s: %s\n  error while reading rows: %v", where, r.sql, err)
				continue
			}
			want := append([]string(nil), r.expect...)
			if r.arg == "rowsort" {
				sort.Strings(got)
				sort.Strings(want)
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s: %s\n  got:\n    %s\n  want:\n    %s", where, r.sql,
					strings.Join(got, "\n    "), strings.Join(want, "\n    "))
			}
		}
	}
}
