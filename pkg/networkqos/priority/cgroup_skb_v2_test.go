/*
Copyright 2026 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package priority

import (
	"testing"

	"github.com/cilium/ebpf"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

type fakePriorityMap struct {
	values []uint32
}

func (m *fakePriorityMap) Update(_, value any, _ ebpf.MapUpdateFlags) error {
	m.values = append(m.values, *value.(*uint32))
	return nil
}

type fakeLink struct {
	closed int
}

func (l *fakeLink) Close() error {
	l.closed++
	return nil
}

type fakeObjectLoader struct {
	initializedPath string
	loads           int
	maps            []*fakePriorityMap
	objectCloses    int
}

func (l *fakeObjectLoader) Init(path string) error {
	l.initializedPath = path
	return nil
}

func (l *fakeObjectLoader) Load() (*loadedObjects, error) {
	l.loads++
	priorityMap := &fakePriorityMap{}
	l.maps = append(l.maps, priorityMap)
	return &loadedObjects{
		priorityMap: priorityMap,
		close: func() {
			l.objectCloses++
		},
	}, nil
}

type fakeCgroupAttacher struct {
	paths []string
	links []*fakeLink
}

func (a *fakeCgroupAttacher) Attach(path string, _ *ebpf.Program) (linkHandle, error) {
	attachedLink := &fakeLink{}
	a.paths = append(a.paths, path)
	a.links = append(a.links, attachedLink)
	return attachedLink, nil
}

func TestCgroupSKBV2ManagerAttachmentLifecycle(t *testing.T) {
	cgroupPath := t.TempDir()
	provider := &fakeCgroupPathProvider{path: cgroupPath, version: cgroup.CgroupV2}
	loader := &fakeObjectLoader{}
	attacher := &fakeCgroupAttacher{}
	manager := &cgroupSKBV2Manager{
		cgroupMgr:   provider,
		programPath: "/bwm_prio_kern.o",
		loader:      loader,
		attacher:    attacher,
		attachments: make(map[types.UID]*podAttachment),
	}

	if err := manager.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if loader.initializedPath != manager.programPath {
		t.Fatalf("initialized path = %q, want %q", loader.initializedPath, manager.programPath)
	}
	if err := manager.Enable(); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	if err := manager.SetPodPriority("pod-1", corev1.PodQOSBestEffort, 1); err != nil {
		t.Fatalf("first SetPodPriority() error = %v", err)
	}
	if provider.subsystem != cgroup.CgroupUnifiedSubsystem {
		t.Fatalf("subsystem = %q, want unified hierarchy", provider.subsystem)
	}
	if loader.loads != 1 || len(attacher.paths) != 1 || attacher.paths[0] != cgroupPath {
		t.Fatalf("unexpected load/attach counts: loads=%d paths=%v", loader.loads, attacher.paths)
	}
	if got := loader.maps[0].values; len(got) != 1 || got[0] != 1 {
		t.Fatalf("initial priority values = %v, want [1]", got)
	}

	if err := manager.SetPodPriority("pod-1", corev1.PodQOSBestEffort, ^uint32(0)); err != nil {
		t.Fatalf("second SetPodPriority() error = %v", err)
	}
	if loader.loads != 1 || len(attacher.paths) != 1 {
		t.Fatalf("priority update unexpectedly reloaded or reattached: loads=%d attaches=%d", loader.loads, len(attacher.paths))
	}
	if got := loader.maps[0].values; len(got) != 2 || got[1] != ^uint32(0) {
		t.Fatalf("updated priority values = %v", got)
	}

	if err := manager.RemovePodPriority("pod-1"); err != nil {
		t.Fatalf("RemovePodPriority() error = %v", err)
	}
	if attacher.links[0].closed != 1 || loader.objectCloses != 1 || len(manager.attachments) != 0 {
		t.Fatalf("attachment was not fully released: linkCloses=%d objectCloses=%d attachments=%d", attacher.links[0].closed, loader.objectCloses, len(manager.attachments))
	}
	if err := manager.RemovePodPriority("pod-1"); err != nil {
		t.Fatalf("second RemovePodPriority() error = %v", err)
	}

	if err := manager.SetPodPriority("pod-2", corev1.PodQOSBestEffort, 1); err != nil {
		t.Fatalf("SetPodPriority() before Close error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if attacher.links[1].closed != 1 || loader.objectCloses != 2 || len(manager.attachments) != 0 {
		t.Fatalf("Close() did not release all attachments: linkCloses=%d objectCloses=%d attachments=%d", attacher.links[1].closed, loader.objectCloses, len(manager.attachments))
	}

	// An event which was already dequeued before Close must not recreate the
	// attachment or inspect a stale cgroup after the feature has been disabled.
	provider.path = "/cgroup/removed-with-pod"
	if err := manager.SetPodPriority("pod-3", corev1.PodQOSBestEffort, 1); err != nil {
		t.Fatalf("SetPodPriority() after Close error = %v", err)
	}
	if loader.loads != 2 || len(attacher.paths) != 2 || len(manager.attachments) != 0 {
		t.Fatalf("disabled manager created an attachment: loads=%d attaches=%d attachments=%d", loader.loads, len(attacher.paths), len(manager.attachments))
	}

	if err := manager.Enable(); err != nil {
		t.Fatalf("second Enable() error = %v", err)
	}
	provider.path = cgroupPath
	if err := manager.SetPodPriority("pod-3", corev1.PodQOSBestEffort, 1); err != nil {
		t.Fatalf("SetPodPriority() after re-enable error = %v", err)
	}
	if loader.loads != 3 || len(attacher.paths) != 3 || len(manager.attachments) != 1 {
		t.Fatalf("re-enabled manager did not create attachment: loads=%d attaches=%d attachments=%d", loader.loads, len(attacher.paths), len(manager.attachments))
	}
}

func TestCgroupSKBV2ManagerEnableBeforeInit(t *testing.T) {
	manager := &cgroupSKBV2Manager{}
	if err := manager.Enable(); err == nil {
		t.Fatal("Enable() before Init() expected error, got nil")
	}
}
