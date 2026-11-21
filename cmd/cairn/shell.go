package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/integral0909/cairn"
)

type shell struct {
	db      *cairn.DB
	conn    *cairn.Conn
	in      *bufio.Scanner
	out     io.Writer
	prompt  bool
	mode    string // table or csv
	timer   bool
	maxRows int
}

func newShell(db *cairn.DB, in io.Reader, out io.Writer, interactive bool) *shell {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	return &shell{db: db, conn: db.Conn(), in: sc, out: out, prompt: interactive, mode: "table", maxRows: 1000}
}

const shellHelp = `Statements end with a semicolon and may span lines.
  .tables              list tables
  .schema [table]      show CREATE statements
  .indexes [table]     list indexes
  .explain <query>     show the query plan (same as EXPLAIN <query>)
  .mode table|csv      output format
  .timer on|off        show how long each statement takes
  .read <file>         run statements from a file
  .check               verify the database
  .stats               storage statistics
  .checkpoint          copy the log into the database file
  .help                this text
  .quit                leave (Ctrl-D works too)`

func (s *shell) run() {
	if s.prompt {
		fmt.Fprintln(s.out, "cairn shell. Type .help for help; statements end with ;")
	}
	var buf strings.Builder
	for {
		if s.prompt {
			if buf.Len() == 0 {
				fmt.Fprint(s.out, "cairn> ")
			} else {
				fmt.Fprint(s.out, "   ...> ")
			}
		}
		if !s.in.Scan() {
			if strings.TrimSpace(buf.String()) != "" {
				s.runSQL(buf.String())
			}
			if s.prompt {
				fmt.Fprintln(s.out)
			}
			return
		}
		line := s.in.Text()
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			if !s.dot(strings.TrimSpace(line)) {
				return
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if complete(buf.String()) {
			s.runSQL(buf.String())
			buf.Reset()
		}
	}
}

// complete reports whether the text ends with a semicolon outside quotes
// and comments.
func complete(src string) bool {
	inStr, inIdent, inLine, inBlock := false, false, false, false
	last := byte(0)
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
			continue
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		case inStr:
			if c == '\'' {
				inStr = false
			}
		case inIdent:
			if c == '"' {
				inIdent = false
			}
		case c == '\'':
			inStr = true
		case c == '"':
			inIdent = true
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inLine = true
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			continue
		}
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			last = c
		}
	}
	return !inStr && !inIdent && !inBlock && last == ';'
}

func (s *shell) errorf(format string, args ...any) {
	fmt.Fprintf(s.out, "error: "+format+"\n", args...)
}

// dot runs a dot command; false means quit.
func (s *shell) dot(line string) bool {
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case ".quit", ".exit", ".q":
		return false
	case ".help":
		fmt.Fprintln(s.out, shellHelp)
	case ".tables":
		for _, t := range s.db.Tables() {
			fmt.Fprintln(s.out, t)
		}
	case ".schema":
		for _, stmt := range s.db.Schema() {
			if arg == "" || strings.Contains(strings.ToLower(stmt), " "+strings.ToLower(arg)+" ") {
				fmt.Fprintln(s.out, stmt+";")
			}
		}
	case ".indexes":
		for _, stmt := range s.db.Schema() {
			if strings.HasPrefix(stmt, "CREATE INDEX") || strings.HasPrefix(stmt, "CREATE UNIQUE INDEX") {
				if arg == "" || strings.Contains(strings.ToLower(stmt), " on "+strings.ToLower(arg)+" ") {
					fmt.Fprintln(s.out, stmt)
				}
			}
		}
	case ".explain":
		s.runSQL("EXPLAIN " + strings.TrimSuffix(arg, ";"))
	case ".mode":
		if arg != "table" && arg != "csv" {
			s.errorf("modes are table and csv")
		} else {
			s.mode = arg
		}
	case ".timer":
		s.timer = arg == "on"
	case ".read":
		data, err := os.ReadFile(arg)
		if err != nil {
			s.errorf("%v", err)
			break
		}
		s.runSQL(string(data))
	case ".check":
		problems, err := s.db.Check()
		if err != nil {
			s.errorf("%v", err)
			break
		}
		if len(problems) == 0 {
			fmt.Fprintln(s.out, "ok")
		}
		for _, p := range problems {
			fmt.Fprintln(s.out, p)
		}
	case ".stats":
		st := s.db.Stats()
		fmt.Fprintf(s.out, "tables      %d\npages       %d (%d KiB)\nlog frames  %d\n", st.Tables, st.Pages, st.Pages*4, st.LogFrames)
	case ".checkpoint":
		if err := s.db.Checkpoint(); err != nil {
			s.errorf("%v", err)
		}
	default:
		s.errorf("unknown command %s (try .help)", cmd)
	}
	return true
}

// runSQL runs statements, printing results; it reports success.
func (s *shell) runSQL(src string) bool {
	stmts := splitStatements(src)
	for _, stmt := range stmts {
		start := time.Now()
		rows, err := s.conn.Query(stmt)
		if err != nil {
			s.errorf("%v", err)
			return false
		}
		cols := rows.Columns()
		var data [][]string
		truncated := false
		for rows.Next() {
			if len(data) >= s.maxRows && s.mode == "table" {
				truncated = true
				continue
			}
			data = append(data, rows.Strings())
		}
		if err := rows.Err(); err != nil {
			s.errorf("%v", err)
			return false
		}
		elapsed := time.Since(start)
		if len(cols) > 0 {
			if s.mode == "csv" {
				w := csv.NewWriter(s.out)
				w.Write(cols)
				w.WriteAll(data)
			} else if len(cols) == 1 && cols[0] == "plan" {
				for _, r := range data {
					fmt.Fprintln(s.out, r[0])
				}
			} else {
				printTable(s.out, cols, data)
				if truncated {
					fmt.Fprintf(s.out, "(showing the first %d rows)\n", s.maxRows)
				} else {
					fmt.Fprintf(s.out, "(%d row%s)\n", len(data), plural(len(data)))
				}
			}
		} else if res := rows.Result(); res != nil && res.RowsAffected > 0 && s.prompt {
			fmt.Fprintf(s.out, "%d row%s changed\n", res.RowsAffected, plural(int(res.RowsAffected)))
		}
		if s.timer {
			fmt.Fprintf(s.out, "time: %s\n", elapsed.Round(time.Microsecond))
		}
	}
	return true
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// splitStatements splits a script at top-level semicolons.
func splitStatements(src string) []string {
	var out []string
	start := 0
	inStr, inIdent, inLine, inBlock := false, false, false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			inLine = c != '\n'
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
		case inStr:
			inStr = c != '\''
		case inIdent:
			inIdent = c != '"'
		case c == '\'':
			inStr = true
		case c == '"':
			inIdent = true
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inLine = true
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
		case c == ';':
			if stmt := strings.TrimSpace(src[start:i]); stmt != "" && !onlyComments(stmt) {
				out = append(out, stmt)
			}
			start = i + 1
		}
	}
	if stmt := strings.TrimSpace(src[start:]); stmt != "" && !onlyComments(stmt) {
		out = append(out, stmt)
	}
	return out
}

func onlyComments(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "--") {
			return false
		}
	}
	return true
}

func printTable(w io.Writer, cols []string, rows [][]string) {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c)
	}
	for _, r := range rows {
		for i, v := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(v))
		}
	}
	for i := range widths {
		widths[i] = min(widths[i], 60)
	}
	cell := func(s string, width int, right bool) string {
		if utf8.RuneCountInString(s) > width {
			s = string([]rune(s)[:width-1]) + "…"
		}
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(s))
		if right {
			return pad + s
		}
		return s + pad
	}
	numeric := make([]bool, len(cols))
	for i := range cols {
		numeric[i] = len(rows) > 0
		for _, r := range rows {
			if r[i] != "NULL" && !looksNumeric(r[i]) {
				numeric[i] = false
				break
			}
		}
	}
	var line []string
	for i, c := range cols {
		line = append(line, cell(c, widths[i], false))
	}
	fmt.Fprintln(w, strings.Join(line, " │ "))
	var sep []string
	for _, wd := range widths {
		sep = append(sep, strings.Repeat("─", wd))
	}
	fmt.Fprintln(w, strings.Join(sep, "─┼─"))
	for _, r := range rows {
		line = line[:0]
		for i, v := range r {
			line = append(line, cell(v, widths[i], numeric[i]))
		}
		fmt.Fprintln(w, strings.Join(line, " │ "))
	}
}

func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c >= '0' && c <= '9' || c == '.' || c == 'e' || c == '+' || (c == '-' && (i == 0 || s[i-1] == 'e'))) {
			return false
		}
	}
	return true
}
