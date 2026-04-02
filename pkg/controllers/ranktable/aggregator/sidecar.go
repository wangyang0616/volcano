package aggregator

import (
	"context"
	"math/rand"
	"os"
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

	// ExitOnMainContainerExit makes sidecar exit when the specified main container
	// in the same Pod is terminated by detecting a local exit signal file on
	// shared volume. This avoids apiserver polling and is fully in-container.
	//
	// The main container should create MainContainerExitSignalFile on exit.
	ExitOnMainContainerExit     bool
	MainContainerExitSignalFile string
	PodPollInterval             time.Duration
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
	if opt.PodPollInterval <= 0 {
		opt.PodPollInterval = 2 * time.Second
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

	runCtx := ctx
	var cancel context.CancelFunc
	if opt.ExitOnMainContainerExit {
		runCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		go monitorMainContainerExitSignal(runCtx, cancel, opt)
	}

	r.Start(runCtx, opt.IndexFilePath, opt.OutputPath)

	// Initial sync.
	r.Trigger()

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
		case <-runCtx.Done():
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
				r.Trigger()
			}
		case <-ticker.C:
			r.Trigger()
		}
	}
}

func monitorMainContainerExitSignal(ctx context.Context, cancel context.CancelFunc, opt SidecarOptions) {
	if opt.MainContainerExitSignalFile == "" {
		klog.ErrorS(nil, "Exit-on-main-container enabled but main container exit signal file is missing")
		return
	}

	ticker := time.NewTicker(opt.PodPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := os.Stat(opt.MainContainerExitSignalFile); err == nil {
			klog.V(3).InfoS("Main container exit signal file detected; exiting sidecar",
				"exitSignalFile", opt.MainContainerExitSignalFile)
			cancel()
			return
		} else if !os.IsNotExist(err) {
			klog.V(4).ErrorS(err, "Stat main container exit signal file failed",
				"exitSignalFile", opt.MainContainerExitSignalFile)
		}
	}
}
