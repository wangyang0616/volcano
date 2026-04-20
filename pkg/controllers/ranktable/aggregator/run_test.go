package aggregator

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// testDefaultPollInterval mirrors runtime default in applyRunDefaults.
	testDefaultPollInterval = 30 * time.Second
	// testNoopEventMask represents an fsnotify event with no operation bits set.
	testNoopEventMask       = fsnotify.Op(0)
)

func TestShouldTriggerReconcileFromIndexDirEvent(t *testing.T) {
	index := "/mnt/index/index.yaml"
	parent := "/mnt/index"

	if shouldTriggerReconcileFromIndexDirEvent(fsnotify.Event{
		Name: index,
		Op:   fsnotify.Write,
	}, index, parent) != true {
		t.Fatal("direct write on index file should trigger")
	}
	if shouldTriggerReconcileFromIndexDirEvent(fsnotify.Event{
		Name: "/other/file",
		Op:   fsnotify.Write,
	}, index, parent) != false {
		t.Fatal("unrelated path should not trigger")
	}
	if shouldTriggerReconcileFromIndexDirEvent(fsnotify.Event{
		Name: filepath.Join(parent, "..data"),
		Op:   fsnotify.Create,
	}, index, parent) != true {
		t.Fatal("create under index parent should trigger")
	}
	if shouldTriggerReconcileFromIndexDirEvent(fsnotify.Event{
		Name: index,
		Op:   testNoopEventMask,
	}, index, parent) != false {
		t.Fatal("no-op event mask should not trigger")
	}
	if shouldTriggerReconcileFromIndexDirEvent(fsnotify.Event{
		Name: "",
		Op:   fsnotify.Write,
	}, index, parent) != false {
		t.Fatal("empty name should not trigger")
	}
}

func TestApplyRunDefaults(t *testing.T) {
	opt := RunOptions{}
	applyRunDefaults(&opt)
	if opt.PollInterval != testDefaultPollInterval {
		t.Fatalf("defaults: poll=%v", opt.PollInterval)
	}
}

func TestValidateRunOptions(t *testing.T) {
	if err := validateRunOptions(RunOptions{
		IndexFilePath: "/i", OutputPath: "/o",
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateRunOptions(RunOptions{IndexFilePath: "", OutputPath: "/o"}); err == nil {
		t.Fatal("empty index path")
	}
	if err := validateRunOptions(RunOptions{IndexFilePath: "/i", OutputPath: "  "}); err == nil {
		t.Fatal("empty output path")
	}
}
