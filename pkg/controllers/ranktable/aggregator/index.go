package aggregator

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// LoadIndexFromFile reads the mounted index path and parses it with ParseIndexBytes.
func LoadIndexFromFile(path string) (*IndexMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read index file: %w", err)
	}
	return ParseIndexBytes(b)
}

// ParseIndexBytes accepts either a serialized corev1.ConfigMap (typical volume mount)
// or raw YAML/JSON matching IndexMeta. It populates Shards from the shards data key
// and runs Validate.
func ParseIndexBytes(b []byte) (*IndexMeta, error) {
	// Case 1: full ConfigMap YAML/JSON (require Kind to avoid misclassification).
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(b, &cm); err == nil && cm.Kind == "ConfigMap" && len(cm.Data) > 0 {
		meta, err := metaFromConfigMapData(cm.Data)
		if err != nil {
			return nil, err
		}
		if err := parseShardList(meta); err != nil {
			return nil, err
		}
		if err := meta.Validate(); err != nil {
			return nil, err
		}
		return meta, nil
	}

	// Case 2: raw JSON/YAML IndexMeta
	meta := &IndexMeta{}
	if err := yaml.Unmarshal(b, meta); err != nil {
		return nil, fmt.Errorf("parse index meta: %w", err)
	}
	if err := parseShardList(meta); err != nil {
		return nil, err
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	return meta, nil
}

func metaFromConfigMapData(data map[string]string) (*IndexMeta, error) {
	get := func(key string) string { return strings.TrimSpace(data[key]) }
	meta := &IndexMeta{
		CurVersion:       get("ranktable_cur_version"),
		PrevVersion:      get("ranktable_prev_version"),
		Status:           get("status"),
		ProtocolVersion:  get("protocol_version"),
		Encoding:         get("encoding"),
		CompressedSHA256: get("compressed_sha256"),
		ContentSHA256:    get("content_sha256"),
		Selector:         get("selector"),
		ChangedShards:    get("changed_shards"),
		ShardsRaw:        get("shards"),
	}
	var err error
	if meta.ChunkSize, err = parseInt64Field(data, "chunk_size"); err != nil {
		return nil, err
	}
	if meta.TotalShards, err = parseIntField(data, "total_shards"); err != nil {
		return nil, err
	}
	if meta.CompressedSize, err = parseInt64Field(data, "compressed_size"); err != nil {
		return nil, err
	}
	if meta.OriginalSize, err = parseInt64Field(data, "original_size"); err != nil {
		return nil, err
	}
	if v, e := parseInt64Field(data, "max_original_size"); e != nil {
		return nil, e
	} else if get("max_original_size") != "" {
		meta.MaxOriginalSize = v
	}
	return meta, nil
}

func parseInt64Field(data map[string]string, key string) (int64, error) {
	s := strings.TrimSpace(data[key])
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("index data key %q: %w", key, err)
	}
	return v, nil
}

func parseIntField(data map[string]string, key string) (int, error) {
	s := strings.TrimSpace(data[key])
	if s == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("index data key %q: %w", key, err)
	}
	return v, nil
}

// parseShardList unmarshals ShardsRaw JSON into meta.Shards when present.
func parseShardList(meta *IndexMeta) error {
	if meta.ShardsRaw == "" && len(meta.Shards) > 0 {
		return nil
	}
	if meta.ShardsRaw == "" {
		meta.Shards = nil
		return nil
	}
	if err := json.Unmarshal([]byte(meta.ShardsRaw), &meta.Shards); err != nil {
		return fmt.Errorf("parse shards manifest: %w", err)
	}
	return nil
}

// ParseChangedShards parses the changed_shards JSON array (shard ids) from the index.
// An empty string means no list. Non-empty invalid JSON is an error so incremental
// hints are not silently ignored.
func ParseChangedShards(raw string) (map[int]struct{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var ids []int
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("parse changed_shards: %w", err)
	}
	out := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}
