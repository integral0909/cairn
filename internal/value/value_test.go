package value

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func TestCompareAcrossKinds(t *testing.T) {
	ordered := []Value{
		{},
		NewReal(math.Inf(-1)),
		NewInt(math.MinInt64),
		NewInt(-3),
		NewReal(-2.5),
		NewBool(false),
		NewReal(0.5),
		NewInt(1),
		NewReal(1.5),
		NewInt(1 << 53),
		NewInt(1<<53 + 1),
		NewInt(math.MaxInt64),
		NewReal(math.Inf(1)),
		NewText(""),
		NewText("a"),
		NewText("a\x00"),
		NewText("ab"),
		NewBlob(nil),
		NewBlob([]byte{0}),
	}
	for i := range ordered {
		for j := range ordered {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(ordered[i], ordered[j]); got != want {
				t.Errorf("Compare(%v, %v) = %d, want %d", ordered[i], ordered[j], got, want)
			}
			kc := bytes.Compare(EncodeKey(ordered[i]), EncodeKey(ordered[j]))
			if kc != want {
				t.Errorf("key order of %v vs %v = %d, want %d", ordered[i], ordered[j], kc, want)
			}
		}
	}
}

func TestIntAndRealEqual(t *testing.T) {
	if Compare(NewInt(3), NewReal(3)) != 0 {
		t.Fatal("3 and 3.0 should compare equal")
	}
	if !bytes.Equal(EncodeKey(NewInt(3)), EncodeKey(NewReal(3))) {
		t.Fatal("3 and 3.0 should have the same key")
	}
	if !bytes.Equal(EncodeKey(NewReal(0)), EncodeKey(NewReal(math.Copysign(0, -1)))) {
		t.Fatal("-0 and +0 should have the same key")
	}
}

// Random numbers: key order must agree with Compare.
func TestKeyOrderMatchesCompare(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	vals := make([]Value, 3000)
	for i := range vals {
		switch r.Intn(5) {
		case 0:
			vals[i] = NewInt(r.Int63() - r.Int63())
		case 1:
			vals[i] = NewInt(int64(r.Intn(20) - 10))
		case 2:
			vals[i] = NewReal(r.NormFloat64() * 10)
		case 3:
			vals[i] = NewReal(float64(r.Intn(20) - 10))
		default:
			b := make([]byte, r.Intn(4))
			r.Read(b)
			vals[i] = NewText(string(b))
		}
	}
	byCompare := append([]Value(nil), vals...)
	sort.SliceStable(byCompare, func(i, j int) bool { return Compare(byCompare[i], byCompare[j]) < 0 })
	for i := 1; i < len(byCompare); i++ {
		a, b := EncodeKey(byCompare[i-1]), EncodeKey(byCompare[i])
		c := Compare(byCompare[i-1], byCompare[i])
		if (c < 0 && bytes.Compare(a, b) >= 0) || (c == 0 && !bytes.Equal(a, b)) {
			t.Fatalf("key order disagrees for %v and %v", byCompare[i-1], byCompare[i])
		}
	}
}

func TestTupleKeysOrderColumnByColumn(t *testing.T) {
	a := EncodeKey(NewText("ab"), NewInt(9))
	b := EncodeKey(NewText("abc"), NewInt(1))
	c := EncodeKey(NewText("ab"), NewInt(10))
	if !(bytes.Compare(a, c) < 0 && bytes.Compare(c, b) < 0) {
		t.Fatal("tuple keys must compare by the first column, then the next")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	row := []Value{{}, NewInt(-42), NewReal(3.25), NewText("héllo\x00"), NewBlob([]byte{0, 1, 2}), NewBool(true)}
	got, err := DecodeRecord(EncodeRecord(row))
	if err != nil {
		t.Fatal(err)
	}
	for i := range row {
		if Compare(got[i], row[i]) != 0 || got[i].K != row[i].K {
			t.Errorf("column %d: got %#v want %#v", i, got[i], row[i])
		}
	}
	if _, err := DecodeRecord([]byte{3, byte(Text), 50}); err == nil {
		t.Fatal("truncated record should fail")
	}
}

func TestRowidKey(t *testing.T) {
	for _, id := range []int64{math.MinInt64, -1, 0, 1, 1 << 40, math.MaxInt64} {
		if RowidFromKey(RowidKey(id)) != id {
			t.Fatalf("rowid %d does not round trip", id)
		}
	}
	if bytes.Compare(RowidKey(-1), RowidKey(1)) >= 0 {
		t.Fatal("rowid keys must be ordered")
	}
}

func TestFormatting(t *testing.T) {
	cases := map[string]Value{
		"NULL": {}, "7": NewInt(7), "2.0": NewReal(2), "0.1": NewReal(0.1), "1e+100": NewReal(1e100),
		"x'00ff'": NewBlob([]byte{0, 255}), "true": NewBool(true),
	}
	for want, v := range cases {
		if v.String() != want {
			t.Errorf("%#v prints as %q, want %q", v, v.String(), want)
		}
	}
	if NewText("it's").Literal() != "'it''s'" {
		t.Error("text literals must double quotes")
	}
}

func FuzzDecodeRecord(f *testing.F) {
	f.Add(EncodeRecord([]Value{NewInt(1), NewText("x")}))
	f.Fuzz(func(t *testing.T, b []byte) {
		row, err := DecodeRecord(b)
		if err == nil {
			// Whatever decodes must re-encode to something that decodes the same.
			again, err := DecodeRecord(EncodeRecord(row))
			if err != nil || len(again) != len(row) {
				t.Fatalf("re-encoding failed: %v", err)
			}
		}
	})
}
