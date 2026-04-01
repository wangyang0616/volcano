package aggregator

import (
	"context"
	"encoding/base64"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestFetchShard_StrictBase64(t *testing.T) {
	payload := []byte{0xde, 0xad}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sh", Namespace: "ns"},
		Data:       map[string]string{ShardDataKey: base64.StdEncoding.EncodeToString(payload)},
	}
	client := fake.NewSimpleClientset(cm)
	got, err := FetchShard(context.Background(), client, ShardMeta{ID: 0, Namespace: "ns", Name: "sh"}, FetchShardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal(got)
	}
}

func TestFetchShard_InvalidBase64WithoutPlain(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sh", Namespace: "ns"},
		Data:       map[string]string{ShardDataKey: "@@@not-base64@@@"},
	}
	client := fake.NewSimpleClientset(cm)
	_, err := FetchShard(context.Background(), client, ShardMeta{ID: 0, Namespace: "ns", Name: "sh"}, FetchShardOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchShard_PlainAllowed(t *testing.T) {
	raw := []byte("plain")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sh", Namespace: "ns"},
		Data:       map[string]string{ShardDataKey: string(raw)},
	}
	client := fake.NewSimpleClientset(cm)
	got, err := FetchShard(context.Background(), client, ShardMeta{ID: 0, Namespace: "ns", Name: "sh"}, FetchShardOptions{AllowPlainShard: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatal(got)
	}
}
