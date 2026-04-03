package aggregator

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func TestReconcileOnce_IdentitySingleShard(t *testing.T) {
	plain := []byte(`{"rank":0}`)
	comp := plain // identity
	ch := Sha256Hex(comp)
	sz := strconv.Itoa(len(comp))

	cmData := map[string]string{
		"ranktable_cur_version": "v1",
		"status":                StatusCompleted,
		"protocol_version":      ProtocolV10,
		"encoding":              string(EncodingIdentity),
		"chunk_size":            "1024",
		"total_shards":          "1",
		"compressed_size":       sz,
		"original_size":         sz,
		"compressed_sha256":     ch,
		"content_sha256":        ch,
		"shards":                fmt.Sprintf(`[{"id":0,"namespace":"test","name":"rt-sh-0","size":%s,"sha256":"%s"}]`, sz, ch),
	}
	indexCM := corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "idx", Namespace: "test"},
		Data:       cmData,
	}
	indexYAML, err := yaml.Marshal(indexCM)
	if err != nil {
		t.Fatal(err)
	}

	shardCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "rt-sh-0", Namespace: "test"},
		Data: map[string]string{
			ShardDataKey: base64.StdEncoding.EncodeToString(comp),
		},
	}
	client := fake.NewSimpleClientset(shardCM)

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.yaml")
	if err := os.WriteFile(indexPath, indexYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.json")

	r := NewReconciler(client, Options{Workers: 2, RequestQPS: 100})
	if err := r.ReconcileOnce(context.Background(), indexPath, outPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("got %q want %q", got, plain)
	}

	// Same version in memory but output deleted — must rewrite file.
	if err := os.Remove(outPath); err != nil {
		t.Fatal(err)
	}
	if err := r.ReconcileOnce(context.Background(), indexPath, outPath); err != nil {
		t.Fatal(err)
	}
	got2, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != string(plain) {
		t.Fatalf("after delete, got %q want %q", got2, plain)
	}
}

func TestShouldReuseShard(t *testing.T) {
	meta := &IndexMeta{PrevVersion: "old", CurVersion: "new"}
	sh := ShardMeta{ID: 0}
	changed := map[int]struct{}{0: {}}
	cache := map[int]shardCacheEntry{
		0: {version: "old", sha: "a", data: []byte("x")},
	}
	if shouldReuseShard(meta, changed, "old", cache, sh) {
		t.Fatal("shard in changed set should not reuse")
	}
	if shouldReuseShard(meta, nil, "old", cache, ShardMeta{ID: 1}) {
		t.Fatal("missing cache entry should not reuse")
	}
	sh0 := ShardMeta{ID: 0, Namespace: "n", Name: "x", Size: 1, SHA256: "a"}
	if !shouldReuseShard(meta, nil, "old", cache, sh0) {
		t.Fatal("unchanged cached shard should reuse")
	}
}
