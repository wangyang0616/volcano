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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

func newBTFCollectionSpec() *ebpf.CollectionSpec {
	function := &btf.Func{
		Name: cgroupV2ProgramName,
		Type: &btf.FuncProto{Return: &btf.Int{Size: 4}},
	}
	return &ebpf.CollectionSpec{
		Maps: map[string]*ebpf.MapSpec{
			cgroupV2MapName: {
				Name:       cgroupV2MapName,
				Type:       ebpf.Array,
				KeySize:    4,
				ValueSize:  4,
				MaxEntries: 1,
				Key:        &btf.Int{Name: "key", Size: 4},
				Value:      &btf.Int{Name: "value", Size: 4},
			},
		},
		Programs: map[string]*ebpf.ProgramSpec{
			cgroupV2ProgramName: {
				Name: cgroupV2ProgramName,
				Type: ebpf.CGroupSKB,
				Instructions: asm.Instructions{
					btf.WithFuncMetadata(
						asm.LoadMapPtr(asm.R1, 0).
							WithSymbol(cgroupV2ProgramName).
							WithReference(cgroupV2MapName),
						function,
					).WithSource(asm.Comment("source line")),
					asm.Return(),
				},
				License: "GPL",
			},
		},
	}
}

func TestELFObjectLoaderFallsBackWithoutBTF(t *testing.T) {
	spec := newBTFCollectionSpec()
	attempts := make([]*ebpf.CollectionSpec, 0, 2)
	loader := &elfObjectLoader{
		newCollection: func(candidate *ebpf.CollectionSpec) (*ebpf.Collection, error) {
			attempts = append(attempts, candidate)
			if len(attempts) == 1 {
				return nil, fmt.Errorf("map %s: load BTF: %w", cgroupV2MapName, ebpf.ErrNotSupported)
			}
			return &ebpf.Collection{}, nil
		},
	}

	compatibleSpec, collection, err := loader.loadCompatibleCollection(spec)
	if err != nil {
		t.Fatalf("loadCompatibleCollection() error = %v", err)
	}
	collection.Close()
	if len(attempts) != 2 {
		t.Fatalf("collection load attempts = %d, want 2", len(attempts))
	}
	if compatibleSpec.Maps[cgroupV2MapName].Key != nil || compatibleSpec.Maps[cgroupV2MapName].Value != nil {
		t.Fatal("fallback collection retained map BTF")
	}
	instruction := &compatibleSpec.Programs[cgroupV2ProgramName].Instructions[0]
	if btf.FuncMetadata(instruction) != nil || instruction.Source() != nil {
		t.Fatal("fallback collection retained program BTF metadata")
	}
	if instruction.Symbol() != cgroupV2ProgramName || instruction.Reference() != cgroupV2MapName {
		t.Fatalf("fallback instruction linkage = symbol %q, reference %q", instruction.Symbol(), instruction.Reference())
	}

	// Building the fallback must not mutate the original parsed object.
	originalInstruction := &spec.Programs[cgroupV2ProgramName].Instructions[0]
	if spec.Maps[cgroupV2MapName].Key == nil || btf.FuncMetadata(originalInstruction) == nil || originalInstruction.Source() == nil {
		t.Fatal("fallback mutated the original collection spec")
	}
}

func TestELFObjectLoaderDoesNotFallbackForOtherUnsupportedFeatures(t *testing.T) {
	attempts := 0
	loader := &elfObjectLoader{
		newCollection: func(_ *ebpf.CollectionSpec) (*ebpf.Collection, error) {
			attempts++
			return nil, fmt.Errorf("create map: %w", ebpf.ErrNotSupported)
		},
	}

	_, _, err := loader.loadCompatibleCollection(newBTFCollectionSpec())
	if err == nil {
		t.Fatal("loadCompatibleCollection() expected error, got nil")
	}
	if attempts != 1 {
		t.Fatalf("collection load attempts = %d, want 1", attempts)
	}
}

func TestCollectionSpecWithoutBTFRejectsCORERelocation(t *testing.T) {
	_, err := collectionSpecWithoutBTFUsing(newBTFCollectionSpec(), func(_ *asm.Instruction) bool {
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "CO-RE relocation") {
		t.Fatalf("collectionSpecWithoutBTFUsing() error = %v, want CO-RE rejection", err)
	}
}

func TestIsBTFLoadUnsupported(t *testing.T) {
	btfError := fmt.Errorf("program: load BTF: %w", ebpf.ErrNotSupported)
	if !isBTFLoadUnsupported(btfError) {
		t.Fatalf("isBTFLoadUnsupported(%v) = false, want true", btfError)
	}
	if isBTFLoadUnsupported(errors.New("load BTF: permission denied")) {
		t.Fatal("permission error was treated as missing BTF support")
	}
}

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
