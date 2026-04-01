package aggregator

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// DecodeAndDecompressLimited decodes at most maxDecompressed bytes of output. If the
// codec would expand beyond that, an error is returned (mitigates zip bombs). For
// identity encoding, len(in) must not exceed maxDecompressed.
func DecodeAndDecompressLimited(enc Encoding, in []byte, maxDecompressed int64) ([]byte, error) {
	if maxDecompressed < 0 {
		return nil, fmt.Errorf("maxDecompressed must be non-negative")
	}
	switch enc {
	case EncodingIdentity:
		if int64(len(in)) > maxDecompressed {
			return nil, fmt.Errorf("identity payload size %d exceeds limit %d", len(in), maxDecompressed)
		}
		return bytes.Clone(in), nil
	case EncodingGzip:
		gr, err := gzip.NewReader(bytes.NewReader(in))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gr.Close()
		lr := io.LimitReader(gr, maxDecompressed+1)
		out, err := io.ReadAll(lr)
		if err != nil {
			return nil, fmt.Errorf("gzip read: %w", err)
		}
		if int64(len(out)) > maxDecompressed {
			return nil, fmt.Errorf("gzip decompressed size exceeds limit %d", maxDecompressed)
		}
		return out, nil
	case EncodingZstd:
		dec, err := zstd.NewReader(bytes.NewReader(in))
		if err != nil {
			return nil, fmt.Errorf("zstd reader: %w", err)
		}
		defer dec.Close()
		lr := io.LimitReader(dec, maxDecompressed+1)
		out, err := io.ReadAll(lr)
		if err != nil {
			return nil, fmt.Errorf("zstd read: %w", err)
		}
		if int64(len(out)) > maxDecompressed {
			return nil, fmt.Errorf("zstd decompressed size exceeds limit %d", maxDecompressed)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported encoding: %q", enc)
	}
}

// WriteFileAtomically writes data via a temp file in the same directory, fsyncs,
// then renames into place so readers never see a partial file.
func WriteFileAtomically(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ranktable-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}
