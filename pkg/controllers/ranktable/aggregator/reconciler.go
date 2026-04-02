package aggregator

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Options configures concurrency, client-side rate limiting, decompressed size cap,
// and optional permissive shard decoding for tests.
type Options struct {
	Workers         int
	RequestQPS      float64
	MaxOriginalSize int64
	AllowPlainShard bool
}

type shardCacheEntry struct {
	version string
	sha     string
	data    []byte
}

// Reconciler fetches shards, assembles the compressed blob, verifies hashes and sizes,
// decompresses, and writes the output. It keeps a small in-memory shard cache to
// skip refetches when the index reports an incremental update compatible with
// prev_version and changed_shards.
type Reconciler struct {
	client kubernetes.Interface
	opts   Options

	mu             sync.Mutex
	currentVersion string
	lastShardByID  map[int]shardCacheEntry

	startOnce sync.Once
	triggerCh chan struct{}
}

// NewReconciler returns a reconciler with defaulted Workers and RequestQPS if unset.
func NewReconciler(client kubernetes.Interface, opts Options) *Reconciler {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.RequestQPS <= 0 {
		opts.RequestQPS = 3
	}
	return &Reconciler{
		client:        client,
		opts:          opts,
		lastShardByID: map[int]shardCacheEntry{},
	}
}

// CurrentVersion returns the last successfully reconciled ranktable_cur_version.
func (r *Reconciler) CurrentVersion() string {
	return r.getCurrentVersion()
}

func (r *Reconciler) getCurrentVersion() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentVersion
}

func (r *Reconciler) setCurrentVersionAndCache(version string, cache map[int]shardCacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.currentVersion = version
	r.lastShardByID = cache
}

func (r *Reconciler) snapshotVersionAndCache() (string, map[int]shardCacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.currentVersion
	snap := make(map[int]shardCacheEntry, len(r.lastShardByID))
	for id, e := range r.lastShardByID {
		snap[id] = e
	}
	return cur, snap
}

// Start runs a single reconcile loop in the background. Trigger coalesces bursts via a
// buffered channel so at most one extra reconcile is queued while a run is active.
//
// Start is safe to call multiple times; only the first call starts the loop.
func (r *Reconciler) Start(ctx context.Context, indexPath, outputPath string) {
	r.startOnce.Do(func() {
		r.triggerCh = make(chan struct{}, 1)
		go r.runLoop(ctx, indexPath, outputPath)
	})
}

// Trigger requests a reconcile. Calls coalesce: if a request is already pending,
// Trigger returns without blocking.
func (r *Reconciler) Trigger() {
	ch := r.triggerCh
	if ch == nil {
		// Start was not called; ignore to keep call sites simple.
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// ReconcileOnce loads the index, fetches missing shards, validates, decompresses, and
// atomically writes outputPath. No-op if status is not completed or version unchanged.
func (r *Reconciler) ReconcileOnce(ctx context.Context, indexPath, outputPath string) (err error) {
	start := time.Now()
	skipped := false
	defer func() { observeReconcileOutcome(start, err, skipped) }()

	meta, err := LoadIndexFromFile(indexPath)
	if err != nil {
		return err
	}
	if meta.Status != StatusCompleted {
		return fmt.Errorf("index not completed, status=%s", meta.Status)
	}
	if meta.CurVersion == r.CurrentVersion() {
		skipped = true
		klog.V(4).InfoS("Skip reconcile for unchanged version", "version", meta.CurVersion)
		return nil
	}

	payload, cache, err := r.fetchAssemble(ctx, meta)
	if err != nil {
		return err
	}

	if int64(len(payload)) != meta.CompressedSize {
		return fmt.Errorf("compressed size mismatch: got=%d expected=%d", len(payload), meta.CompressedSize)
	}
	if !EqualHash(Sha256Hex(payload), meta.CompressedSHA256) {
		return fmt.Errorf("compressed sha256 mismatch")
	}

	decompLimit, err := EffectiveDecompressedLimit(meta, r.opts.MaxOriginalSize)
	if err != nil {
		return err
	}

	content, err := DecodeAndDecompressLimited(Encoding(meta.Encoding), payload, decompLimit)
	if err != nil {
		return err
	}
	if int64(len(content)) != meta.OriginalSize {
		return fmt.Errorf("original size mismatch: got=%d expected=%d", len(content), meta.OriginalSize)
	}
	if meta.MaxOriginalSize > 0 && int64(len(content)) > meta.MaxOriginalSize {
		return fmt.Errorf("original size exceeds max_original_size: %d > %d", len(content), meta.MaxOriginalSize)
	}
	if r.opts.MaxOriginalSize > 0 && int64(len(content)) > r.opts.MaxOriginalSize {
		return fmt.Errorf("original size exceeds runtime max: %d > %d", len(content), r.opts.MaxOriginalSize)
	}
	if !EqualHash(Sha256Hex(content), meta.ContentSHA256) {
		return fmt.Errorf("content sha256 mismatch")
	}
	if err := WriteFileAtomically(outputPath, content); err != nil {
		return err
	}

	r.setCurrentVersionAndCache(meta.CurVersion, cache)
	klog.InfoS("RankTable reconciled", "version", meta.CurVersion, "outputPath", outputPath, "shards", meta.TotalShards)
	return nil
}

// fetchAssemble returns concatenated compressed bytes and a per-shard cache snapshot.
// Shards may be reused from r.lastShardByID when incremental metadata allows it.
func (r *Reconciler) fetchAssemble(ctx context.Context, meta *IndexMeta) ([]byte, map[int]shardCacheEntry, error) {
	shards := make([]ShardMeta, len(meta.Shards))
	copy(shards, meta.Shards)
	sort.Slice(shards, func(i, j int) bool { return shards[i].ID < shards[j].ID })

	changed, err := ParseChangedShards(meta.ChangedShards)
	if err != nil {
		return nil, nil, err
	}
	limiter := rate.NewLimiter(rate.Limit(r.opts.RequestQPS), max(1, r.opts.Workers))

	type result struct {
		id   int
		data []byte
		err  error
	}

	out := make(chan result, len(shards))
	sem := make(chan struct{}, r.opts.Workers)
	var wg sync.WaitGroup

	curVersion, cacheSnapshot := r.snapshotVersionAndCache()

	fetchOpt := FetchShardOptions{AllowPlainShard: r.opts.AllowPlainShard}

	for _, s := range shards {
		if shouldReuseShard(meta, changed, curVersion, cacheSnapshot, s) {
			e := cacheSnapshot[s.ID]
			out <- result{id: s.ID, data: e.data}
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(shard ShardMeta) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := limiter.Wait(ctx); err != nil {
				out <- result{id: shard.ID, err: err}
				return
			}
			b, err := FetchShard(ctx, r.client, shard, fetchOpt)
			observeShardFetch(err)
			if err != nil {
				out <- result{id: shard.ID, err: err}
				return
			}
			if int64(len(b)) != shard.Size {
				out <- result{id: shard.ID, err: fmt.Errorf("shard %d size mismatch: got=%d expected=%d", shard.ID, len(b), shard.Size)}
				return
			}
			if !EqualHash(Sha256Hex(b), shard.SHA256) {
				out <- result{id: shard.ID, err: fmt.Errorf("shard %d sha256 mismatch", shard.ID)}
				return
			}
			out <- result{id: shard.ID, data: b}
		}(s)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	chunks := make(map[int][]byte, len(shards))
	for res := range out {
		if res.err != nil {
			return nil, nil, res.err
		}
		chunks[res.id] = res.data
	}
	if len(chunks) != len(shards) {
		return nil, nil, fmt.Errorf("missing shards: got=%d expected=%d", len(chunks), len(shards))
	}

	cache := make(map[int]shardCacheEntry, len(shards))
	merged := make([]byte, 0, meta.CompressedSize)
	for _, s := range shards {
		merged = append(merged, chunks[s.ID]...)
		cache[s.ID] = shardCacheEntry{
			version: meta.CurVersion,
			sha:     s.SHA256,
			data:    chunks[s.ID],
		}
	}
	return merged, cache, nil
}

// shouldReuseShard returns true if the in-memory cache from a prior reconcile matches
// prev_version / changed_shards rules so this shard need not be GET again.
func shouldReuseShard(meta *IndexMeta, changed map[int]struct{}, currentVersion string, cache map[int]shardCacheEntry, shard ShardMeta) bool {
	if meta.PrevVersion == "" || meta.PrevVersion != currentVersion {
		return false
	}
	if len(changed) > 0 {
		if _, ok := changed[shard.ID]; ok {
			return false
		}
	}
	e, ok := cache[shard.ID]
	if !ok {
		return false
	}
	return e.version == currentVersion && EqualHash(e.sha, shard.SHA256) && int64(len(e.data)) == shard.Size
}

func (r *Reconciler) runLoop(ctx context.Context, indexPath, outputPath string) {
	const coalesceDelay = 300 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.triggerCh:
		}

		for {
			start := time.Now()
			err := r.ReconcileOnce(ctx, indexPath, outputPath)
			if err != nil {
				klog.ErrorS(err, "RankTable reconcile failed")
			} else {
				klog.V(3).InfoS("RankTable reconcile succeeded", "elapsed", time.Since(start).String())
			}

			// If more triggers arrived during the reconcile, run once more after a short delay.
			select {
			case <-ctx.Done():
				return
			case <-r.triggerCh:
				// Drain any extra queued signals so we run at most one extra reconcile.
				r.drainTriggers()
				time.Sleep(coalesceDelay)
				continue
			default:
				return
			}
		}
	}
}

func (r *Reconciler) drainTriggers() {
	for {
		select {
		case <-r.triggerCh:
			continue
		default:
			return
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
