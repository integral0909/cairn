// Package sql parses Cairn's SQL dialect into an abstract syntax tree.
package sql

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tKeyword
	tInt
	tFloat
	tString
	tBlob
	tParam
	tOp
)

type token struct {
	kind tokKind
	text string // identifiers lowercased unless quoted; keywords uppercased
	pos  int
	num  int // parameter number for tParam (1-based, 0 for plain ?)
	blob []byte
}

var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`ADD ALL ALTER AND AS ASC BEGIN BETWEEN BLOB BOOLEAN BY CASE CAST COLUMN COMMIT
		CREATE CROSS DEFAULT DELETE DESC DISTINCT DROP ELSE END EXCEPT EXISTS EXPLAIN FALSE FROM FULL GLOB GROUP
		HAVING IF IN INDEX INNER INSERT INTEGER INTERSECT INTO IS JOIN KEY LEFT LIKE LIMIT NOT NULL OFFSET ON OR
		ORDER OUTER PRIMARY REAL RENAME ROLLBACK SELECT SET TABLE TEXT THEN TO TRANSACTION TRUE UNION UNIQUE
		UPDATE VALUES WHEN WHERE ANALYZE INT BIGINT SMALLINT FLOAT DOUBLE VARCHAR CHAR BOOL RETURNING CONFLICT
		DO NOTHING REPLACE NULLS FIRST LAST`) {
		keywords[k] = true
	}
}

// Error is a syntax error with its position in the source.
type Error struct {
	Msg       string
	Line, Col int
}

func (e *Error) Error() string {
	return fmt.Sprintf("syntax error at line %d, column %d: %s", e.Line, e.Col, e.Msg)
}

func errorAt(src string, pos int, format string, args ...any) *Error {
	line, col := 1, 1
	for i, r := range src {
		if i >= pos {
			break
		}
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return &Error{Msg: fmt.Sprintf(format, args...), Line: line, Col: col}
}

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	nextParam := 0
	for {
		// Skip spaces and comments.
		for i < len(src) {
			r, w := utf8.DecodeRuneInString(src[i:])
			if unicode.IsSpace(r) {
				i += w
			} else if strings.HasPrefix(src[i:], "--") {
				for i < len(src) && src[i] != '\n' {
					i++
				}
			} else if strings.HasPrefix(src[i:], "/*") {
				end := strings.Index(src[i+2:], "*/")
				if end < 0 {
					return nil, errorAt(src, i, "unterminated comment")
				}
				i += end + 4
			} else {
				break
			}
		}
		if i >= len(src) {
			return append(toks, token{kind: tEOF, pos: i}), nil
		}
		start := i
		c := src[i]
		switch {
		case (c == 'x' || c == 'X') && i+1 < len(src) && src[i+1] == '\'':
			end := strings.IndexByte(src[i+2:], '\'')
			if end < 0 {
				return nil, errorAt(src, i, "unterminated blob literal")
			}
			b, err := hex.DecodeString(src[i+2 : i+2+end])
			if err != nil {
				return nil, errorAt(src, i, "blob literals need an even number of hex digits")
			}
			i += end + 3
			toks = append(toks, token{kind: tBlob, pos: start, blob: b})
		case isIdentStart(c) || c >= 0x80:
			for i < len(src) {
				r, w := utf8.DecodeRuneInString(src[i:])
				if !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
					break
				}
				i += w
			}
			if i == start {
				// A byte that starts no token: invalid UTF-8, or a symbol.
				r, _ := utf8.DecodeRuneInString(src[i:])
				return nil, errorAt(src, start, "unexpected character %q", r)
			}
			word := src[start:i]
			if up := strings.ToUpper(word); keywords[up] {
				toks = append(toks, token{kind: tKeyword, text: up, pos: start})
			} else {
				toks = append(toks, token{kind: tIdent, text: strings.ToLower(word), pos: start})
			}
		case c == '"' || c == '`':
			// Quoted identifiers keep their case and may be keywords.
			var sb strings.Builder
			i++
			for {
				if i >= len(src) {
					return nil, errorAt(src, start, "unterminated quoted identifier")
				}
				if src[i] == c {
					if i+1 < len(src) && src[i+1] == c {
						sb.WriteByte(c)
						i += 2
						continue
					}
					i++
					break
				}
				sb.WriteByte(src[i])
				i++
			}
			toks = append(toks, token{kind: tIdent, text: sb.String(), pos: start})
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			kind := tInt
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			if i < len(src) && src[i] == '.' {
				kind = tFloat
				i++
				for i < len(src) && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && src[j] >= '0' && src[j] <= '9' {
					kind = tFloat
					i = j
					for i < len(src) && src[i] >= '0' && src[i] <= '9' {
						i++
					}
				}
			}
			if i < len(src) && isIdentStart(src[i]) {
				return nil, errorAt(src, start, "malformed number %q", src[start:i+1])
			}
			toks = append(toks, token{kind: kind, text: src[start:i], pos: start})
		case c == '\'':
			var sb strings.Builder
			i++
			for {
				if i >= len(src) {
					return nil, errorAt(src, start, "unterminated string")
				}
				if src[i] == '\'' {
					if i+1 < len(src) && src[i+1] == '\'' {
						sb.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				sb.WriteByte(src[i])
				i++
			}
			toks = append(toks, token{kind: tString, text: sb.String(), pos: start})
		case c == '?':
			i++
			nextParam++
			toks = append(toks, token{kind: tParam, pos: start, num: nextParam})
		case c == '$':
			i++
			n := 0
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				n = n*10 + int(src[i]-'0')
				i++
			}
			if n == 0 {
				return nil, errorAt(src, start, "parameters are written $1, $2, ...")
			}
			toks = append(toks, token{kind: tParam, pos: start, num: n})
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "<>", "!=", "==", "||":
				i += 2
				if two == "==" {
					two = "="
				} else if two == "<>" {
					two = "!="
				}
				toks = append(toks, token{kind: tOp, text: two, pos: start})
				continue
			}
			if strings.IndexByte("=<>+-*/%(),.;", c) < 0 {
				return nil, errorAt(src, start, "unexpected character %q", c)
			}
			i++
			toks = append(toks, token{kind: tOp, text: string(c), pos: start})
		}
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
