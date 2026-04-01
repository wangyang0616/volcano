package aggregator

import "fmt"

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
