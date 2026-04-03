package aggregator

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorMainContainerExitSignal_FileAppears(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	dir := t.TempDir()
	signalPath := filepath.Join(dir, "main-container.exit")

	cancelCalled := make(chan struct{}, 1)
	cancelFn := func() {
		select {
		case cancelCalled <- struct{}{}:
		default:
		}
		stop()
	}

	go monitorMainContainerExitSignal(ctx, cancelFn, SidecarOptions{
		MainContainerExitSignalFile: signalPath,
		PodPollInterval:             10 * time.Millisecond,
	})

	// Give monitor goroutine time to start polling.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(signalPath, []byte("done"), 0o644); err != nil {
		t.Fatalf("write signal file: %v", err)
	}

	select {
	case <-cancelCalled:
	case <-time.After(1 * time.Second):
		t.Fatal("expected cancel to be called after signal file appears")
	}
}

func TestMonitorMainContainerExitSignal_MissingPath_NoCancel(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	var called int32
	cancelFn := func() {
		atomic.StoreInt32(&called, 1)
		stop()
	}

	go monitorMainContainerExitSignal(ctx, cancelFn, SidecarOptions{
		MainContainerExitSignalFile: "",
		PodPollInterval:             10 * time.Millisecond,
	})

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("cancel should not be called when signal file path is empty")
	}
}

func TestMonitorMainContainerExitSignal_ContextDone_NoCancel(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	signalPath := filepath.Join(t.TempDir(), "main-container.exit")

	var called int32
	cancelFn := func() {
		atomic.StoreInt32(&called, 1)
	}

	go monitorMainContainerExitSignal(ctx, cancelFn, SidecarOptions{
		MainContainerExitSignalFile: signalPath,
		PodPollInterval:             10 * time.Millisecond,
	})

	stop()
	time.Sleep(30 * time.Millisecond)
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("cancel should not be called when context is canceled first")
	}
}
