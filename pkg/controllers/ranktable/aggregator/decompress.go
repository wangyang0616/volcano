package aggregator

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// EffectiveDecompressedLimit returns the smallest positive bound applied while decoding:
// min(original_size, max_original_size if set, runtime MaxOriginalSize if set).
// Used with DecodeAndDecompressLimited to cap expansion before full output is materialized.
func EffectiveDecompressedLimit(meta *IndexMeta, runtimeMax int64) (int64, error) {
	if meta.OriginalSize < 0 {
		return 0, fmt.Errorf("original_size must be non-negative")
	}
	limit := meta.OriginalSize
	if meta.MaxOriginalSize > 0 && meta.MaxOriginalSize < limit {
		limit = meta.MaxOriginalSize
	}
	if runtimeMax > 0 && runtimeMax < limit {
		limit = runtimeMax
	}
	return limit, nil
}

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
