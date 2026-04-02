package aggregator

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
)

// ShardDataKey is the ConfigMap data key holding base64-encoded shard bytes.
const ShardDataKey = "ranktable_shard_info"

// FetchShardOptions controls shard fetch behavior.
type FetchShardOptions struct {
	// AllowPlainShard permits non-base64 shard payloads (tests/debug only).
	AllowPlainShard bool
}

// FetchShard GETs the shard ConfigMap, decodes payload, and retries transient errors.
func FetchShard(ctx context.Context, client kubernetes.Interface, shard ShardMeta, opt FetchShardOptions) ([]byte, error) {
	var lastErr error
	baseDelay := 200 * time.Millisecond
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			d := baseDelay * time.Duration(1<<uint(attempt-1))
			if d > 10*time.Second {
				d = 10 * time.Second
			}
			d += time.Duration(rand.Int63n(int64(d / 5)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d):
			}
		}
		b, err := fetchShardOnce(ctx, client, shard, opt.AllowPlainShard)
		if err == nil {
			return b, nil
		}
		lastErr = err
		if !isRetriableShardGetErr(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("shard %d GET after retries: %w", shard.ID, lastErr)
}

func fetchShardOnce(ctx context.Context, client kubernetes.Interface, shard ShardMeta, allowPlain bool) ([]byte, error) {
	cm, err := client.CoreV1().ConfigMaps(shard.Namespace).Get(ctx, shard.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get shard configmap %s/%s: %w", shard.Namespace, shard.Name, err)
	}
	raw, ok := cm.Data[ShardDataKey]
	if !ok {
		return nil, fmt.Errorf("missing shard key %q in %s/%s", ShardDataKey, shard.Namespace, shard.Name)
	}
	raw = strings.TrimSpace(raw)
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(raw, "\n", ""))
	if err != nil {
		if !allowPlain {
			return nil, fmt.Errorf("shard %s/%s: decode ranktable_shard_info (base64): %w", shard.Namespace, shard.Name, err)
		}
		return []byte(raw), nil
	}
	return decoded, nil
}

func isRetriableShardGetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if apierrors.IsTooManyRequests(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		return true
	}
	if apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err) {
		return true
	}
	return false
}
