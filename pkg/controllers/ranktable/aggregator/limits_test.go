package aggregator

import "testing"

func TestEffectiveDecompressedLimit(t *testing.T) {
	meta := &IndexMeta{OriginalSize: 100, MaxOriginalSize: 50}
	l, err := EffectiveDecompressedLimit(meta, 200)
	if err != nil {
		t.Fatal(err)
	}
	if l != 50 {
		t.Fatal(l)
	}
	l, err = EffectiveDecompressedLimit(meta, 30)
	if err != nil {
		t.Fatal(err)
	}
	if l != 30 {
		t.Fatal(l)
	}
}
