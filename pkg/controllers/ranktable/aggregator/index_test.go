package aggregator

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestParseIndexBytes_ConfigMap(t *testing.T) {
	cm := corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "idx", Namespace: "ns"},
		Data: map[string]string{
			"ranktable_cur_version":  "1",
			"ranktable_prev_version": "",
			"status":                 StatusCompleted,
			"protocol_version":       ProtocolV10,
			"encoding":               string(EncodingIdentity),
			"chunk_size":             "100",
			"total_shards":           "1",
			"compressed_size":        "5",
			"original_size":          "5",
			"compressed_sha256":      "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
			"content_sha256":         "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
			"shards":                 `[{"id":0,"namespace":"ns","name":"sh0","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"}]`,
		},
	}
	b, err := yaml.Marshal(cm)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ParseIndexBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if meta.TotalShards != 1 || len(meta.Shards) != 1 {
		t.Fatalf("shards: %+v", meta)
	}
}

func TestParseIndexBytes_InvalidNumeric(t *testing.T) {
	cm := corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "idx", Namespace: "ns"},
		Data: map[string]string{
			"ranktable_cur_version": "1",
			"status":                StatusCompleted,
			"protocol_version":      ProtocolV10,
			"encoding":              string(EncodingIdentity),
			"total_shards":          "bogus",
			"compressed_size":       "1",
			"original_size":         "1",
			"compressed_sha256":     "a",
			"content_sha256":        "a",
			"shards":                `[]`,
		},
	}
	b, err := yaml.Marshal(cm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseIndexBytes(b); err == nil {
		t.Fatal("expected error for bogus total_shards")
	}
}

func TestParseChangedShards(t *testing.T) {
	m, err := ParseChangedShards("")
	if err != nil || m != nil {
		t.Fatalf("empty: %v %v", m, err)
	}
	m, err = ParseChangedShards(`[1, 2]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatal(m)
	}
	if _, err := ParseChangedShards(`[1,`); err == nil {
		t.Fatal("expected error")
	}
}

func TestIndexMetaValidate_DuplicateShardID(t *testing.T) {
	meta := &IndexMeta{
		CurVersion:       "1",
		Status:           StatusCompleted,
		ProtocolVersion:  ProtocolV10,
		Encoding:         string(EncodingIdentity),
		TotalShards:      2,
		CompressedSize:   1,
		OriginalSize:     1,
		CompressedSHA256: "a",
		ContentSHA256:    "a",
		Shards: []ShardMeta{
			{ID: 0, Namespace: "n", Name: "a", Size: 1, SHA256: "x"},
			{ID: 0, Namespace: "n", Name: "b", Size: 1, SHA256: "y"},
		},
	}
	if err := meta.Validate(); err == nil {
		t.Fatal("expected duplicate id error")
	}
}
