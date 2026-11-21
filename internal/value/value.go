// Package value defines the values a row can hold and how they compare,
// convert and print.
package value

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is the dynamic type of a Value.
type Kind uint8

const (
	Null Kind = iota
	Int
	Real
	Text
	Blob
	Bool
)

func (k Kind) String() string {
	switch k {
	case Null:
		return "NULL"
	case Int:
		return "INTEGER"
	case Real:
		return "REAL"
	case Text:
		return "TEXT"
	case Blob:
		return "BLOB"
	case Bool:
		return "BOOLEAN"
	}
	return fmt.Sprintf("Kind(%d)", k)
}

// Value is a single SQL value. The zero Value is NULL.
type Value struct {
	K Kind
	I int64 // Int, and Bool as 0 or 1
	F float64
	S string // Text
	B []byte // Blob
}

func NewInt(i int64) Value   { return Value{K: Int, I: i} }
func NewText(s string) Value { return Value{K: Text, S: s} }
func NewBlob(b []byte) Value { return Value{K: Blob, B: b} }
func NewBool(b bool) Value {
	v := Value{K: Bool}
	if b {
		v.I = 1
	}
	return v
}
func NewReal(f float64) Value {
	if math.IsNaN(f) {
		return Value{}
	}
	return Value{K: Real, F: f}
}

func (v Value) IsNull() bool { return v.K == Null }

// IsNumeric reports whether v is an INTEGER, REAL or BOOLEAN.
func (v Value) IsNumeric() bool { return v.K == Int || v.K == Real || v.K == Bool }

// Float returns the numeric value as a float64.
func (v Value) Float() float64 {
	if v.K == Real {
		return v.F
	}
	return float64(v.I)
}

// Truth is the value's meaning in a WHERE clause: true, false or unknown
// (NULL). Non-zero numbers are true; text and blobs are errors elsewhere,
// so here they count as unknown.
func (v Value) Truth() (truth bool, known bool) {
	switch v.K {
	case Bool, Int:
		return v.I != 0, true
	case Real:
		return v.F != 0, true
	}
	return false, false
}

// rank orders kinds for comparison: NULL < numbers < text < blobs.
func rank(k Kind) int {
	switch k {
	case Null:
		return 0
	case Int, Real, Bool:
		return 1
	case Text:
		return 2
	}
	return 3
}

// Compare orders two values: -1, 0 or 1. NULLs sort first and equal each
// other here; SQL's "NULL is not equal to anything" is handled by callers.
// Integers and reals compare by numeric value, exactly even past 2^53.
func Compare(a, b Value) int {
	ra, rb := rank(a.K), rank(b.K)
	if ra != rb {
		return cmpInt(int64(ra), int64(rb))
	}
	switch ra {
	case 0:
		return 0
	case 1:
		if a.K != Real && b.K != Real {
			return cmpInt(a.I, b.I)
		}
		if a.K == Real && b.K == Real {
			return cmpFloat(a.F, b.F)
		}
		if a.K == Real {
			return -compareIntReal(b.I, a.F)
		}
		return compareIntReal(a.I, b.F)
	case 2:
		return strings.Compare(a.S, b.S)
	}
	return bytes.Compare(a.B, b.B)
}

// compareIntReal compares an integer with a float without losing precision.
func compareIntReal(i int64, f float64) int {
	if f >= 9.223372036854775807e18 {
		return -1
	}
	if f < -9.223372036854775808e18 {
		return 1
	}
	t := math.Trunc(f)
	if c := cmpInt(i, int64(t)); c != 0 {
		return c
	}
	// Equal integer parts: the fraction decides.
	return cmpFloat(0, f-t)
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Equal reports SQL equality ignoring NULL semantics (used for grouping and
// DISTINCT, where NULLs fall into one group).
func Equal(a, b Value) bool { return Compare(a, b) == 0 }

// String renders the value the way the shell prints it.
func (v Value) String() string {
	switch v.K {
	case Null:
		return "NULL"
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Real:
		return FormatReal(v.F)
	case Text:
		return v.S
	case Blob:
		return "x'" + hex.EncodeToString(v.B) + "'"
	case Bool:
		if v.I != 0 {
			return "true"
		}
		return "false"
	}
	return "?"
}

// FormatReal prints a float with the fewest digits that round-trip, and
// always with a decimal point or exponent so it reads as a REAL.
func FormatReal(f float64) string {
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}

// Literal renders the value as SQL source.
func (v Value) Literal() string {
	switch v.K {
	case Text:
		return "'" + strings.ReplaceAll(v.S, "'", "''") + "'"
	}
	return v.String()
}

// Go converts the value to the natural Go type (nil, int64, float64,
// string, []byte or bool).
func (v Value) Go() any {
	switch v.K {
	case Int:
		return v.I
	case Real:
		return v.F
	case Text:
		return v.S
	case Blob:
		return v.B
	case Bool:
		return v.I != 0
	}
	return nil
}

// FromGo converts a Go value into a Value.
func FromGo(x any) (Value, error) {
	switch t := x.(type) {
	case nil:
		return Value{}, nil
	case Value:
		return t, nil
	case int:
		return NewInt(int64(t)), nil
	case int8:
		return NewInt(int64(t)), nil
	case int16:
		return NewInt(int64(t)), nil
	case int32:
		return NewInt(int64(t)), nil
	case int64:
		return NewInt(t), nil
	case uint8:
		return NewInt(int64(t)), nil
	case uint16:
		return NewInt(int64(t)), nil
	case uint32:
		return NewInt(int64(t)), nil
	case uint:
		if uint64(t) > math.MaxInt64 {
			return Value{}, fmt.Errorf("value %d overflows INTEGER", t)
		}
		return NewInt(int64(t)), nil
	case uint64:
		if t > math.MaxInt64 {
			return Value{}, fmt.Errorf("value %d overflows INTEGER", t)
		}
		return NewInt(int64(t)), nil
	case float32:
		return NewReal(float64(t)), nil
	case float64:
		return NewReal(t), nil
	case string:
		return NewText(t), nil
	case []byte:
		return NewBlob(append([]byte(nil), t...)), nil
	case bool:
		return NewBool(t), nil
	}
	return Value{}, fmt.Errorf("unsupported parameter type %T", x)
}
