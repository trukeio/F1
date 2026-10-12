package hash

import (
	"encoding/json"
	"testing"
)

// BLAKE3 known answer: BLAKE3("") from the BLAKE3 specification test vectors.
func TestBLAKE3KnownAnswer(t *testing.T) {
	if got := Plain(nil).String(); got != "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262" {
		t.Fatalf("BLAKE3(\"\") = %s", got)
	}
}

func TestSumIsTagged(t *testing.T) {
	if Sum(1, []byte("x")) == Plain([]byte("x")) {
		t.Fatal("tagged hash equals untagged hash")
	}
	if Sum(1, []byte("x")) != Plain([]byte("\x01x")) {
		t.Fatal("Sum(tag, p) != BLAKE3(tag ‖ p)")
	}
}

func TestParse(t *testing.T) {
	h := Sum(2, []byte("abc"))
	p, err := Parse(h.String())
	if err != nil || p != h {
		t.Fatalf("Parse(String()) = %v, %v", p, err)
	}
	for _, bad := range []string{"", "00", h.String() + "00", "zz" + h.String()[2:]} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestJSON(t *testing.T) {
	h := Sum(3, nil)
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"`+h.String()+`"` {
		t.Fatalf("JSON = %s", b)
	}
	var back Hash
	if err := json.Unmarshal(b, &back); err != nil || back != h {
		t.Fatalf("round trip = %v, %v", back, err)
	}
}

func TestCompare(t *testing.T) {
	a, b := Hash{1}, Hash{2}
	if Compare(a, b) != -1 || Compare(b, a) != 1 || Compare(a, a) != 0 {
		t.Fatal("Compare is wrong")
	}
	if !Zero.IsZero() || a.IsZero() {
		t.Fatal("IsZero is wrong")
	}
}
