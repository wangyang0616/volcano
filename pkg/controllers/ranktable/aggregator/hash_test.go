package aggregator

import "testing"

func TestEqualHash(t *testing.T) {
	if !EqualHash("AbCd", "abcd") {
		t.Fatal()
	}
	if EqualHash("a", "b") {
		t.Fatal()
	}
}
