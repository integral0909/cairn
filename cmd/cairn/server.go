package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/integral0909/cairn"
)

//go:embed web
var webFiles embed.FS

const (
	maxBody      = 1 << 20 // 1 MiB of SQL
	maxRows      = 10000
	queryTimeout = 30 * time.Second
)

func runServe(args []string) int {
	fset := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fset.String("addr", "127.0.0.1:8080", "address to listen on")
	fset.Parse(args)
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cairn serve [-addr host:port] <file>")
		return 2
	}
	db, err := cairn.Open(fset.Arg(0), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn:", err)
		return 1
	}
	defer db.Close()
	host, _, _ := net.SplitHostPort(*addr)
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		log.Printf("warning: the console has no authentication and is listening on %s", *addr)
	}
	srv := &http.Server{Addr: *addr, Handler: newHandler(db), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("cairn console on http://%s (database %s)", *addr, fset.Arg(0))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		return 1
	}
	return 0
}

type queryRequest struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args"`
}

type statementResult struct {
	SQL          string   `json:"sql"`
	Columns      []string `json:"columns,omitempty"`
	Rows         [][]any  `json:"rows,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
	RowsAffected int64    `json:"rowsAffected"`
	LastInsertID int64    `json:"lastInsertId,omitempty"`
	ElapsedMs    float64  `json:"elapsedMs"`
	Error        string   `json:"error,omitempty"`
}

func newHandler(db *cairn.DB) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))

	mux.HandleFunc("POST /api/query", func(w http.ResponseWriter, r *http.Request) {
		var req queryRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send JSON like {\"sql\": \"SELECT 1\"}"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		// Each request gets its own session; statements in one request run
		// in order, so a request may hold BEGIN ... COMMIT.
		conn := db.Conn().WithContext(ctx)
		defer conn.Close()
		var results []statementResult
		stmts := splitStatements(req.SQL)
		for i, stmt := range stmts {
			var args []any
			if i == len(stmts)-1 {
				args = req.Args
			}
			res := runStatement(conn, stmt, args)
			results = append(results, res)
			if res.Error != "" {
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	})

	mux.HandleFunc("GET /api/schema", func(w http.ResponseWriter, r *http.Request) {
		st := db.Stats()
		tables := db.TableInfo()
		if tables == nil {
			tables = []cairn.TableInfo{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"tables": tables, "pages": st.Pages, "logFrames": st.LogFrames})
	})
	return securityHeaders(mux)
}

func runStatement(conn *cairn.Conn, stmt string, args []any) statementResult {
	res := statementResult{SQL: stmt}
	start := time.Now()
	defer func() { res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000 }()
	for i, a := range args {
		// JSON numbers arrive as float64; whole ones are meant as integers.
		if f, ok := a.(float64); ok && f == float64(int64(f)) {
			args[i] = int64(f)
		}
	}
	rows, err := conn.Query(stmt, args...)
	if err != nil {
		res.Error = err.Error()
		res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
		return res
	}
	res.Columns = rows.Columns()
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			continue
		}
		vals := rows.Values()
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = fmt.Sprintf("x'%x'", b)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		res.Error = err.Error()
	}
	if r := rows.Result(); r != nil {
		res.RowsAffected, res.LastInsertID = r.RowsAffected, r.LastInsertID
	}
	res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
	return res
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// Writes must come from the console itself, not another site.
		if r.Method == http.MethodPost {
			if origin := r.Header.Get("Origin"); origin != "" && !strings.HasSuffix(origin, "://"+r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
