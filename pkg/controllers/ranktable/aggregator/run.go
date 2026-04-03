package aggregator

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"
)

// RunOptions configures the long-running ranktable consumer: paths, polling, and startup jitter.
type RunOptions struct {
	IndexFilePath string
	OutputPath    string
	PollInterval  time.Duration
	StartupJitter time.Duration
}

func applyRunDefaults(opt *RunOptions) {
	if opt.PollInterval <= 0 {
		opt.PollInterval = 30 * time.Second
	}
}

func validateRunOptions(opt RunOptions) error {
	if strings.TrimSpace(opt.IndexFilePath) == "" {
		return fmt.Errorf("index file path is empty")
	}
	if strings.TrimSpace(opt.OutputPath) == "" {
		return fmt.Errorf("output path is empty")
	}
	return nil
}

// Run is the only supported entry for the vc-ranktable-aggregator process. After optional
// startup jitter it may run a synchronous bootstrap ReconcileOnce when the output file is
// missing, then starts the background reconciler, watches the index directory, and polls.
func Run(ctx context.Context, r *Reconciler, opt RunOptions) error {
	applyRunDefaults(&opt)
	if err := validateRunOptions(opt); err != nil {
		return err
	}

	if opt.StartupJitter > 0 {
		delay := time.Duration(rand.Int64N(int64(opt.StartupJitter)))
		klog.V(3).InfoS("Startup jitter", "delay", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// If output is missing, reconcile once inline so the file appears before relying on
	// the background loop. On success we skip the initial Trigger to avoid an extra
	// index load + no-op (poll/fsnotify still drive later updates).
	skipInitialKick := false
	if _, err := os.Stat(opt.OutputPath); err != nil && os.IsNotExist(err) {
		klog.InfoS("RankTable output absent; bootstrap reconcile before watch loop", "outputPath", opt.OutputPath)
		if err := r.ReconcileOnce(ctx, opt.IndexFilePath, opt.OutputPath); err != nil {
			klog.ErrorS(err, "Bootstrap reconcile failed; watch loop will retry")
		} else {
			skipInitialKick = true
		}
	}

	r.Start(ctx, opt.IndexFilePath, opt.OutputPath)
	if !skipInitialKick {
		r.Trigger()
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	// ConfigMap volume updates often rename/replace under the parent of the index file.
	parentDir := filepath.Clean(filepath.Dir(opt.IndexFilePath))
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
			if shouldTriggerReconcileFromIndexDirEvent(ev, opt.IndexFilePath, parentDir) {
				klog.V(4).InfoS("Index path event", "event", ev.String())
				r.Trigger()
			}
		case <-ticker.C:
			r.Trigger()
		}
	}
}

// shouldTriggerReconcileFromIndexDirEvent reports whether an fsnotify event may reflect a
// change to the mounted index (whole ConfigMap volume directory is watched).
func shouldTriggerReconcileFromIndexDirEvent(ev fsnotify.Event, indexPath, parentDir string) bool {
	if ev.Name == "" {
		return false
	}
	cleanName := filepath.Clean(ev.Name)
	cleanIndex := filepath.Clean(indexPath)
	cleanParent := filepath.Clean(parentDir)
	evParent := filepath.Clean(filepath.Dir(ev.Name))
	if cleanName != cleanIndex && evParent != cleanParent {
		return false
	}
	return ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || ev.Has(fsnotify.Remove) ||
		ev.Has(fsnotify.Rename) || ev.Has(fsnotify.Chmod)
}
