package aggregator

import (
	"fmt"
)

// Index status values in the ranktable index ConfigMap data map.
const (
	StatusInitializing = "initializing"
	StatusCompleted    = "completed"
	// ProtocolV10 is the only supported value for protocol_version in the index.
	ProtocolV10 = "v1.0"

	// DefaultMaxOriginalSize is the default --max-original-size (bytes) and matches the
	// example index max_original_size. Larger payloads can be allowed by raising both.
	DefaultMaxOriginalSize int64 = 200 * 1024 * 1024 // 200 MiB
)

// Encoding names the compression applied to the full RankTable before sharding.
type Encoding string

const (
	EncodingIdentity Encoding = "identity"
	EncodingGzip     Encoding = "gzip"
	EncodingZstd     Encoding = "zstd"
)

// IndexMeta mirrors the ranktable index ConfigMap: versioning, integrity fields,
// shard manifest, and optional incremental hints (prev_version, changed_shards).
// Shards is filled after parsing the JSON shards list from ShardsRaw.
type IndexMeta struct {
	CurVersion      string `json:"ranktable_cur_version" yaml:"ranktable_cur_version"`
	PrevVersion     string `json:"ranktable_prev_version" yaml:"ranktable_prev_version"`
	Status          string `json:"status" yaml:"status"`
	ProtocolVersion string `json:"protocol_version" yaml:"protocol_version"`
	Encoding        string `json:"encoding" yaml:"encoding"`

	ChunkSize      int64 `json:"chunk_size,string" yaml:"chunk_size"`
	TotalShards    int   `json:"total_shards,string" yaml:"total_shards"`
	CompressedSize int64 `json:"compressed_size,string" yaml:"compressed_size"`
	OriginalSize   int64 `json:"original_size,string" yaml:"original_size"`

	CompressedSHA256 string `json:"compressed_sha256" yaml:"compressed_sha256"`
	ContentSHA256    string `json:"content_sha256" yaml:"content_sha256"`
	MaxOriginalSize  int64  `json:"max_original_size,string" yaml:"max_original_size"`

	Selector      string `json:"selector" yaml:"selector"`
	ChangedShards string `json:"changed_shards" yaml:"changed_shards"`
	ShardsRaw     string `json:"shards" yaml:"shards"`

	Shards []ShardMeta `json:"-" yaml:"-"`
}

// ShardMeta identifies one shard ConfigMap and its expected compressed-byte size and hash.
type ShardMeta struct {
	ID        int    `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// Validate checks protocol_version, encoding, shard count, and required hashes.
func (m *IndexMeta) Validate() error {
	if m.CurVersion == "" {
		return fmt.Errorf("ranktable_cur_version is empty")
	}
	if m.Status != StatusInitializing && m.Status != StatusCompleted {
		return fmt.Errorf("invalid status %q", m.Status)
	}
	if m.ProtocolVersion != ProtocolV10 {
		return fmt.Errorf("unsupported protocol_version %q", m.ProtocolVersion)
	}
	switch Encoding(m.Encoding) {
	case EncodingIdentity, EncodingGzip, EncodingZstd:
	default:
		return fmt.Errorf("unsupported encoding %q", m.Encoding)
	}
	if m.TotalShards <= 0 {
		return fmt.Errorf("invalid total_shards %d", m.TotalShards)
	}
	if m.CompressedSHA256 == "" || m.ContentSHA256 == "" {
		return fmt.Errorf("compressed_sha256 and content_sha256 are required")
	}
	if m.CompressedSize < 0 || m.OriginalSize < 0 {
		return fmt.Errorf("compressed_size and original_size must be non-negative")
	}
	if m.MaxOriginalSize > 0 && m.OriginalSize > m.MaxOriginalSize {
		return fmt.Errorf("original_size %d exceeds max_original_size %d", m.OriginalSize, m.MaxOriginalSize)
	}
	if len(m.Shards) != m.TotalShards {
		return fmt.Errorf("shards count mismatch: len(shards)=%d total_shards=%d", len(m.Shards), m.TotalShards)
	}
	seen := make(map[int]struct{}, len(m.Shards))
	for _, sh := range m.Shards {
		if _, ok := seen[sh.ID]; ok {
			return fmt.Errorf("duplicate shard id %d in manifest", sh.ID)
		}
		seen[sh.ID] = struct{}{}
		if sh.Namespace == "" || sh.Name == "" {
			return fmt.Errorf("shard id %d: namespace and name are required", sh.ID)
		}
	}
	return nil
}
