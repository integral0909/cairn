package engine

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/integral0909/cairn/internal/value"
)

// Columns are strictly typed: a value is stored only if it fits the column
// type without losing information. 3.0 fits an INTEGER column, 3.5 does
// not; text never silently becomes a number.
func coerce(v value.Value, typ value.Kind, col string) (value.Value, error) {
	if v.IsNull() || v.K == typ {
		return v, nil
	}
	bad := func() (value.Value, error) {
		return value.Value{}, fmt.Errorf("cannot store %s %s in %s column %s", v.K, v.Literal(), typ, col)
	}
	switch typ {
	case value.Int:
		switch v.K {
		case value.Bool:
			return value.NewInt(v.I), nil
		case value.Real:
			if v.F == math.Trunc(v.F) && v.F >= -9.223372036854775808e18 && v.F < 9.223372036854775807e18 {
				return value.NewInt(int64(v.F)), nil
			}
		}
	case value.Real:
		if v.K == value.Int || v.K == value.Bool {
			return value.NewReal(float64(v.I)), nil
		}
	case value.Bool:
		if (v.K == value.Int && (v.I == 0 || v.I == 1)) || (v.K == value.Real && (v.F == 0 || v.F == 1)) {
			return value.NewBool(v.Float() == 1), nil
		}
	}
	return bad()
}

// castTo implements CAST, which converts more freely than storing does.
func castTo(v value.Value, typ value.Kind) (value.Value, error) {
	if v.IsNull() || v.K == typ {
		return v, nil
	}
	fail := func() (value.Value, error) {
		return value.Value{}, fmt.Errorf("cannot cast %s %s to %s", v.K, v.Literal(), typ)
	}
	switch typ {
	case value.Int:
		switch v.K {
		case value.Bool:
			return value.NewInt(v.I), nil
		case value.Real:
			if math.IsInf(v.F, 0) || v.F >= 9.223372036854775807e18 || v.F < -9.223372036854775808e18 {
				return fail()
			}
			return value.NewInt(int64(v.F)), nil
		case value.Text:
			s := strings.TrimSpace(v.S)
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return value.NewInt(n), nil
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) && math.Abs(f) < 9.2e18 {
				return value.NewInt(int64(f)), nil
			}
		}
	case value.Real:
		switch v.K {
		case value.Int, value.Bool:
			return value.NewReal(float64(v.I)), nil
		case value.Text:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64); err == nil {
				return value.NewReal(f), nil
			}
		}
	case value.Text:
		switch v.K {
		case value.Blob:
			return value.NewText(string(v.B)), nil
		default:
			return value.NewText(v.String()), nil
		}
	case value.Blob:
		switch v.K {
		case value.Text:
			return value.NewBlob([]byte(v.S)), nil
		}
	case value.Bool:
		switch v.K {
		case value.Int:
			return value.NewBool(v.I != 0), nil
		case value.Real:
			return value.NewBool(v.F != 0), nil
		case value.Text:
			switch strings.ToLower(strings.TrimSpace(v.S)) {
			case "true", "t", "yes", "1":
				return value.NewBool(true), nil
			case "false", "f", "no", "0":
				return value.NewBool(false), nil
			}
		}
	}
	return fail()
}

type category int

const (
	catNull category = iota
	catNumber
	catText
	catBlob
)

func categoryOf(v value.Value) category {
	switch v.K {
	case value.Null:
		return catNull
	case value.Int, value.Real, value.Bool:
		return catNumber
	case value.Text:
		return catText
	}
	return catBlob
}

// compareSQL compares two non-null values of compatible kinds. Comparing
// text with numbers is an error rather than a silent guess.
func compareSQL(a, b value.Value) (int, error) {
	if categoryOf(a) != categoryOf(b) {
		return 0, fmt.Errorf("cannot compare %s with %s", a.K, b.K)
	}
	return value.Compare(a, b), nil
}

var errDivZero = fmt.Errorf("division by zero")

// arith applies + - * / % with SQL NULL rules and integer overflow checks.
func arith(op string, a, b value.Value) (value.Value, error) {
	if a.IsNull() || b.IsNull() {
		return value.Value{}, nil
	}
	if !a.IsNumeric() || !b.IsNumeric() {
		bad := a
		if a.IsNumeric() {
			bad = b
		}
		return value.Value{}, fmt.Errorf("cannot apply %s to %s %s", op, bad.K, bad.Literal())
	}
	if a.K != value.Real && b.K != value.Real {
		x, y := a.I, b.I
		switch op {
		case "+":
			r := x + y
			if (r > x) != (y > 0) {
				return value.Value{}, errOverflow
			}
			return value.NewInt(r), nil
		case "-":
			r := x - y
			if (r < x) != (y > 0) {
				return value.Value{}, errOverflow
			}
			return value.NewInt(r), nil
		case "*":
			if x == 0 || y == 0 {
				return value.NewInt(0), nil
			}
			r := x * y
			if r/y != x || (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64) {
				return value.Value{}, errOverflow
			}
			return value.NewInt(r), nil
		case "/":
			if y == 0 {
				return value.Value{}, errDivZero
			}
			if x == math.MinInt64 && y == -1 {
				return value.Value{}, errOverflow
			}
			return value.NewInt(x / y), nil
		case "%":
			if y == 0 {
				return value.Value{}, errDivZero
			}
			if y == -1 {
				return value.NewInt(0), nil
			}
			return value.NewInt(x % y), nil
		}
	}
	x, y := a.Float(), b.Float()
	switch op {
	case "+":
		return value.NewReal(x + y), nil
	case "-":
		return value.NewReal(x - y), nil
	case "*":
		return value.NewReal(x * y), nil
	case "/":
		if y == 0 {
			return value.Value{}, errDivZero
		}
		return value.NewReal(x / y), nil
	case "%":
		if y == 0 {
			return value.Value{}, errDivZero
		}
		return value.NewReal(math.Mod(x, y)), nil
	}
	return value.Value{}, fmt.Errorf("unknown operator %s", op)
}

var errOverflow = fmt.Errorf("integer overflow")

// textOf renders a value for || and text functions.
func textOf(v value.Value) string {
	if v.K == value.Blob {
		return string(v.B)
	}
	return v.String()
}

// like matches SQL LIKE: % any run, _ one character, case-insensitive for
// ASCII letters.
func like(s, pattern string) bool {
	return matchPattern([]rune(s), []rune(pattern), true)
}

func matchPattern(s, p []rune, fold bool) bool {
	// Iterative matching with backtracking to the last %.
	si, pi := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '%':
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi] == '_' || runeEq(p[pi], s[si], fold)):
			si++
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

func runeEq(a, b rune, fold bool) bool {
	if a == b {
		return true
	}
	if !fold {
		return false
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	return lower(a) == lower(b)
}

// glob matches GLOB: * any run, ? one character, [abc] and [a-z] classes,
// case-sensitive.
func glob(s, pattern string) bool {
	return globMatch([]rune(s), []rune(pattern))
}

func globMatch(s, p []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(s[i:], p) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			s, p = s[1:], p[1:]
		case '[':
			end := 1
			for end < len(p) && p[end] != ']' {
				end++
			}
			if end >= len(p) || len(s) == 0 {
				return false
			}
			class := p[1:end]
			neg := len(class) > 0 && class[0] == '^'
			if neg {
				class = class[1:]
			}
			hit := false
			for i := 0; i < len(class); i++ {
				if i+2 < len(class) && class[i+1] == '-' {
					if s[0] >= class[i] && s[0] <= class[i+2] {
						hit = true
					}
					i += 2
				} else if class[i] == s[0] {
					hit = true
				}
			}
			if hit == neg {
				return false
			}
			s, p = s[1:], p[end+1:]
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
			s, p = s[1:], p[1:]
		}
	}
	return len(s) == 0
}

func hexOf(v value.Value) string {
	if v.K == value.Blob {
		return strings.ToUpper(hex.EncodeToString(v.B))
	}
	return strings.ToUpper(hex.EncodeToString([]byte(textOf(v))))
}
