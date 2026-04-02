package aggregator

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

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

func TestDecodeAndDecompressLimited_Identity(t *testing.T) {
	in := []byte("hello")
	out, err := DecodeAndDecompressLimited(EncodingIdentity, in, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello" {
		t.Fatal(string(out))
	}
	if _, err := DecodeAndDecompressLimited(EncodingIdentity, in, 4); err == nil {
		t.Fatal("expected limit error")
	}
}

func TestDecodeAndDecompressLimited_GzipUnderLimit(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := DecodeAndDecompressLimited(EncodingGzip, buf.Bytes(), 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "payload" {
		t.Fatal(string(out))
	}
}

func TestDecodeAndDecompressLimited_GzipOverLimit(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAndDecompressLimited(EncodingGzip, buf.Bytes(), 10); err == nil {
		t.Fatal("expected expansion limit error")
	}
}

func TestWriteFileAtomically(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "out.dat")
	if err := WriteFileAtomically(p, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "ok" {
		t.Fatal(string(b))
	}
}
