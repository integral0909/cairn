package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/integral0909/cairn"
)

func TestStatementSplitting(t *testing.T) {
	got := splitStatements("SELECT 1; SELECT ';' -- a ; comment\n; /* ; */ SELECT \"a;b\";\n-- only a comment\n")
	want := []string{"SELECT 1", "SELECT ';' -- a ; comment", `/* ; */ SELECT "a;b"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
	for src, done := range map[string]bool{
		"SELECT 1;":            true,
		"SELECT 1":             false,
		"SELECT ';":            false,
		"SELECT 1; -- note":    true,
		"SELECT 1 /* ; */":     false,
		"SELECT 'a;b';  \n":    true,
		`SELECT "x;" FROM t;`:  true,
		"SELECT 1 -- ;\n FROM": false,
	} {
		if complete(src) != done {
			t.Errorf("complete(%q) = %v", src, !done)
		}
	}
}

func TestShellOutput(t *testing.T) {
	db, _ := cairn.Open(cairn.Memory, nil)
	defer db.Close()
	var out bytes.Buffer
	input := "CREATE TABLE t (n INTEGER, s TEXT);\nINSERT INTO t VALUES (1, 'one'), (22, NULL);\nSELECT * FROM t\nORDER BY n;\n.mode csv\nSELECT s FROM t WHERE n = 1;\n.bogus\nSELECT x;\n"
	newShell(db, strings.NewReader(input), &out, false).run()
	got := out.String()
	for _, want := range []string{"n  │ s   \n", "─", " 1 │ one \n", "22 │ NULL\n", "(2 rows)\n", "s\none\n", "error: unknown command .bogus", "error: no such column: x"} {
		if !strings.Contains(got, want) {
			t.Errorf("shell output lacks %q:\n%s", want, got)
		}
	}
}

func TestHTTPAPI(t *testing.T) {
	db, _ := cairn.Open(cairn.Memory, nil)
	defer db.Close()
	srv := httptest.NewServer(newHandler(db))
	defer srv.Close()

	post := func(body string, header map[string]string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/query", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}
	_, out := post(`{"sql": "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t (v) VALUES ('a'), ('b'); SELECT id, v FROM t WHERE id > ?", "args": [1]}`, nil)
	results := out["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results: %v", out)
	}
	last := results[2].(map[string]any)
	rows := last["rows"].([]any)
	if len(rows) != 1 || rows[0].([]any)[1] != "b" {
		t.Fatalf("rows: %v", last)
	}
	if results[1].(map[string]any)["rowsAffected"].(float64) != 2 {
		t.Fatalf("rowsAffected: %v", results[1])
	}

	_, out = post(`{"sql": "SELECT 1; SELECT nope; SELECT 2"}`, nil)
	results = out["results"].([]any)
	if len(results) != 2 || !strings.Contains(results[1].(map[string]any)["error"].(string), "no such column") {
		t.Fatalf("an error should stop the batch: %v", out)
	}

	res, _ := post(`{"sql": "DROP TABLE t"}`, map[string]string{"Origin": "https://evil.example"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin write allowed: %d", res.StatusCode)
	}
	res, _ = post(`not json`, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body: %d", res.StatusCode)
	}

	sres, err := http.Get(srv.URL + "/api/schema")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	json.NewDecoder(sres.Body).Decode(&schema)
	sres.Body.Close()
	tables := schema["tables"].([]any)
	if len(tables) != 1 || tables[0].(map[string]any)["name"] != "t" {
		t.Fatalf("schema: %v", schema)
	}

	page, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	if page.StatusCode != 200 || !strings.Contains(page.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("console page: %d %v", page.StatusCode, page.Header)
	}
}
