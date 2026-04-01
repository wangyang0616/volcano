package aggregator

import (
	"context"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"
)

// SidecarOptions configures paths, periodic poll, and startup jitter for RunSidecar.
type SidecarOptions struct {
	IndexFilePath string
	OutputPath    string
	PollInterval  time.Duration
	StartupJitter time.Duration
}

// RunInit optionally waits up to startupJitter (spread) then runs a single ReconcileOnce.
// Intended for use as an initContainer before the workload starts.
func RunInit(ctx context.Context, r *Reconciler, indexFilePath, outputPath string, startupJitter time.Duration) error {
	if startupJitter > 0 {
		delay := time.Duration(rand.Int63n(int64(startupJitter)))
		klog.V(3).InfoS("Init reconcile startup jitter", "delay", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.ReconcileOnce(ctx, indexFilePath, outputPath)
}

// RunSidecar watches the index file’s directory (ConfigMap volume updates), polls on
// a ticker, and triggers reconciliation after optional startup jitter.
func RunSidecar(ctx context.Context, r *Reconciler, opt SidecarOptions) error {
	if opt.PollInterval <= 0 {
		opt.PollInterval = 30 * time.Second
	}
	if opt.StartupJitter > 0 {
		delay := time.Duration(rand.Int63n(int64(opt.StartupJitter)))
		klog.V(3).InfoS("Sidecar startup jitter", "delay", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Initial sync.
	r.Trigger(ctx, opt.IndexFilePath, opt.OutputPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	// Mount updates often happen via symlink swap in parent directory.
	parentDir := filepath.Dir(opt.IndexFilePath)
	if err := watcher.Add(parentDir); err != nil {
		return err
	}

	ticker := time.NewTicker(opt.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-watcher.Errors:
			if err != nil {
				klog.ErrorS(err, "fsnotify error")
			}
		case ev := <-watcher.Events:
			if ev.Name == "" {
				continue
			}
			if filepath.Clean(ev.Name) != filepath.Clean(opt.IndexFilePath) && filepath.Clean(filepath.Dir(ev.Name)) != filepath.Clean(parentDir) {
				continue
			}
			if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) || ev.Has(fsnotify.Chmod) {
				klog.V(4).InfoS("Index file event received", "event", ev.String())
				r.Trigger(ctx, opt.IndexFilePath, opt.OutputPath)
			}
		case <-ticker.C:
			r.Trigger(ctx, opt.IndexFilePath, opt.OutputPath)
		}
	}
}
