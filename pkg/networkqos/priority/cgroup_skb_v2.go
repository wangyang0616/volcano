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
	"os"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

const (
	cgroupV2ProgramName = "_bwm_out_cg"
	cgroupV2MapName     = "cgrp_prio"
)

type priorityMap interface {
	Update(key, value any, flags ebpf.MapUpdateFlags) error
}

type linkHandle interface {
	Close() error
}

type loadedObjects struct {
	program     *ebpf.Program
	priorityMap priorityMap
	close       func()
}

type objectLoader interface {
	Init(path string) error
	Load() (*loadedObjects, error)
}

type cgroupAttacher interface {
	Attach(path string, program *ebpf.Program) (linkHandle, error)
}

type collectionFactory func(spec *ebpf.CollectionSpec) (*ebpf.Collection, error)

type elfObjectLoader struct {
	spec          *ebpf.CollectionSpec
	newCollection collectionFactory
}

func newELFObjectLoader() *elfObjectLoader {
	return &elfObjectLoader{newCollection: ebpf.NewCollection}
}

func (l *elfObjectLoader) collectionFactory() collectionFactory {
	if l.newCollection != nil {
		return l.newCollection
	}
	return ebpf.NewCollection
}

func (l *elfObjectLoader) Init(path string) error {
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("failed to remove BPF memlock limit: %w", err)
	}

	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return fmt.Errorf("failed to load BPF collection spec %s: %w", path, err)
	}
	if _, found := spec.Programs[cgroupV2ProgramName]; !found {
		return fmt.Errorf("BPF program %q not found in %s", cgroupV2ProgramName, path)
	}
	if _, found := spec.Maps[cgroupV2MapName]; !found {
		return fmt.Errorf("BPF map %q not found in %s", cgroupV2MapName, path)
	}

	compatibleSpec, probe, err := l.loadCompatibleCollection(spec)
	if err != nil {
		return fmt.Errorf("failed to load BPF priority program: %w", err)
	}
	probe.Close()
	l.spec = compatibleSpec
	return nil
}

// loadCompatibleCollection first tries the object with its BTF metadata. Some
// enterprise kernels support the cgroup_skb program type but don't support
// BPF_BTF_LOAD. The NetworkQoS program doesn't require BTF at runtime, so retry
// without BTF metadata only when the first failure is specifically a BTF
// capability failure.
func (l *elfObjectLoader) loadCompatibleCollection(spec *ebpf.CollectionSpec) (*ebpf.CollectionSpec, *ebpf.Collection, error) {
	newCollection := l.collectionFactory()
	collection, err := newCollection(spec.Copy())
	if err == nil {
		return spec, collection, nil
	}
	if !isBTFLoadUnsupported(err) {
		return nil, nil, err
	}

	btfLessSpec, fallbackErr := collectionSpecWithoutBTF(spec)
	if fallbackErr != nil {
		return nil, nil, fmt.Errorf("kernel rejected BTF and BTF-less fallback is unavailable: %v: %w", fallbackErr, err)
	}
	collection, fallbackErr = newCollection(btfLessSpec.Copy())
	if fallbackErr != nil {
		return nil, nil, fmt.Errorf("BTF-less fallback failed after kernel rejected BTF (%v): %w", err, fallbackErr)
	}

	klog.InfoS("Kernel BTF loading is unavailable, using BTF-less cgroup v2 NetworkQoS program")
	return btfLessSpec, collection, nil
}

func isBTFLoadUnsupported(err error) bool {
	return errors.Is(err, ebpf.ErrNotSupported) && strings.Contains(err.Error(), "load BTF")
}

type coreRelocationDetector func(instruction *asm.Instruction) bool

func collectionSpecWithoutBTF(spec *ebpf.CollectionSpec) (*ebpf.CollectionSpec, error) {
	return collectionSpecWithoutBTFUsing(spec, func(instruction *asm.Instruction) bool {
		return btf.CORERelocationMetadata(instruction) != nil
	})
}

// collectionSpecWithoutBTFUsing removes only metadata which makes the kernel
// load BTF. Symbol, reference and associated-map metadata are retained since
// they are required to link instructions. CO-RE programs and global variables
// cannot be loaded safely without BTF and are deliberately rejected.
func collectionSpecWithoutBTFUsing(spec *ebpf.CollectionSpec, hasCORERelocation coreRelocationDetector) (*ebpf.CollectionSpec, error) {
	if spec == nil {
		return nil, errors.New("BPF collection spec is nil")
	}
	if len(spec.Variables) != 0 {
		return nil, errors.New("BPF collection contains global variables")
	}

	result := spec.Copy()
	result.Types = nil

	visitedMaps := make(map[*ebpf.MapSpec]struct{})
	var removeMapBTF func(mapSpec *ebpf.MapSpec)
	removeMapBTF = func(mapSpec *ebpf.MapSpec) {
		if mapSpec == nil {
			return
		}
		if _, found := visitedMaps[mapSpec]; found {
			return
		}
		visitedMaps[mapSpec] = struct{}{}
		mapSpec.Key = nil
		mapSpec.Value = nil
		removeMapBTF(mapSpec.InnerMap)
	}
	for _, mapSpec := range result.Maps {
		removeMapBTF(mapSpec)
	}

	for programName, programSpec := range result.Programs {
		for index := range programSpec.Instructions {
			instruction := &programSpec.Instructions[index]
			if hasCORERelocation(instruction) {
				return nil, fmt.Errorf("BPF program %q instruction %d contains a CO-RE relocation", programName, index)
			}

			symbol := instruction.Symbol()
			reference := instruction.Reference()
			associatedMap := instruction.Map()
			instruction.Metadata = asm.Metadata{}
			if symbol != "" {
				*instruction = instruction.WithSymbol(symbol)
			}
			if associatedMap != nil {
				if err := instruction.AssociateMap(associatedMap); err != nil {
					return nil, fmt.Errorf("restore map reference in BPF program %q instruction %d: %w", programName, index, err)
				}
			} else if reference != "" {
				*instruction = instruction.WithReference(reference)
			}
		}
	}

	return result, nil
}

func (l *elfObjectLoader) Load() (*loadedObjects, error) {
	if l.spec == nil {
		return nil, errors.New("BPF object loader is not initialized")
	}

	collection, err := l.collectionFactory()(l.spec.Copy())
	if err != nil {
		return nil, fmt.Errorf("failed to create BPF priority collection: %w", err)
	}
	program, found := collection.Programs[cgroupV2ProgramName]
	if !found {
		collection.Close()
		return nil, fmt.Errorf("loaded BPF program %q not found", cgroupV2ProgramName)
	}
	priorityMap, found := collection.Maps[cgroupV2MapName]
	if !found {
		collection.Close()
		return nil, fmt.Errorf("loaded BPF map %q not found", cgroupV2MapName)
	}

	return &loadedObjects{
		program:     program,
		priorityMap: priorityMap,
		close:       collection.Close,
	}, nil
}

type ebpfCgroupAttacher struct{}

func (ebpfCgroupAttacher) Attach(path string, program *ebpf.Program) (linkHandle, error) {
	cgroupLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    path,
		Attach:  ebpf.AttachCGroupInetEgress,
		Program: program,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to attach BPF priority program to cgroup %s: %w", path, err)
	}
	return cgroupLink, nil
}

type podAttachment struct {
	cgroupPath string
	priority   uint32
	objects    *loadedObjects
	link       linkHandle
}

type cgroupSKBV2Manager struct {
	mu          sync.Mutex
	cgroupMgr   cgroupPathProvider
	programPath string
	loader      objectLoader
	attacher    cgroupAttacher
	attachments map[types.UID]*podAttachment
	initialized bool
	enabled     bool
}

func newCgroupSKBV2Manager(cgroupMgr cgroupPathProvider, programPath string) Manager {
	return &cgroupSKBV2Manager{
		cgroupMgr:   cgroupMgr,
		programPath: programPath,
		loader:      newELFObjectLoader(),
		attacher:    ebpfCgroupAttacher{},
		attachments: make(map[types.UID]*podAttachment),
	}
}

func (m *cgroupSKBV2Manager) Init() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.initialized {
		return nil
	}
	if err := m.loader.Init(m.programPath); err != nil {
		return err
	}
	m.initialized = true
	return nil
}

func (m *cgroupSKBV2Manager) Enable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return errors.New("cgroup v2 network priority manager is not initialized")
	}
	m.enabled = true
	return nil
}

func (m *cgroupSKBV2Manager) SetPodPriority(podUID types.UID, qosClass corev1.PodQOSClass, priority uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return errors.New("cgroup v2 network priority manager is not initialized")
	}
	// Close marks the manager disabled while holding the same lock. Taking the
	// lock before cgroup discovery also makes an already-dequeued Pod event a
	// clean no-op after disable, even if its cgroup has since disappeared.
	if !m.enabled {
		return nil
	}

	cgroupPath, err := m.cgroupMgr.GetPodCgroupPath(qosClass, cgroup.CgroupUnifiedSubsystem, podUID)
	if err != nil {
		return fmt.Errorf("failed to get pod cgroup path %s: %w", podUID, err)
	}
	if _, err := os.Stat(cgroupPath); err != nil {
		return fmt.Errorf("failed to access pod cgroup path %s: %w", cgroupPath, err)
	}

	if attachment, found := m.attachments[podUID]; found {
		if attachment.cgroupPath == cgroupPath {
			if err := updatePriorityMap(attachment.objects.priorityMap, priority); err != nil {
				return fmt.Errorf("failed to update network priority for pod %s: %w", podUID, err)
			}
			attachment.priority = priority
			return nil
		}
		delete(m.attachments, podUID)
		if err := closeAttachment(attachment); err != nil {
			return fmt.Errorf("failed to replace network priority attachment for pod %s: %w", podUID, err)
		}
	}

	objects, err := m.loader.Load()
	if err != nil {
		return err
	}
	if err := updatePriorityMap(objects.priorityMap, priority); err != nil {
		objects.close()
		return fmt.Errorf("failed to initialize network priority for pod %s: %w", podUID, err)
	}

	cgroupLink, err := m.attacher.Attach(cgroupPath, objects.program)
	if err != nil {
		objects.close()
		return err
	}

	m.attachments[podUID] = &podAttachment{
		cgroupPath: cgroupPath,
		priority:   priority,
		objects:    objects,
		link:       cgroupLink,
	}
	return nil
}

func updatePriorityMap(priorityMap priorityMap, priority uint32) error {
	key := uint32(0)
	return priorityMap.Update(&key, &priority, ebpf.UpdateAny)
}

func (m *cgroupSKBV2Manager) RemovePodPriority(podUID types.UID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	attachment, found := m.attachments[podUID]
	if !found {
		return nil
	}
	delete(m.attachments, podUID)
	return closeAttachment(attachment)
}

func (m *cgroupSKBV2Manager) HealthCheck() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return errors.New("cgroup v2 network priority manager is not initialized")
	}
	if _, err := os.Stat(m.programPath); err != nil {
		return fmt.Errorf("failed to access BPF priority program %s: %w", m.programPath, err)
	}
	return nil
}

func (m *cgroupSKBV2Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Reject new attachments before releasing any existing link. Set and Close
	// serialize on mu, so no attachment can survive a completed Close call.
	m.enabled = false
	var errs []error
	for podUID, attachment := range m.attachments {
		if err := closeAttachment(attachment); err != nil {
			errs = append(errs, fmt.Errorf("failed to close network priority attachment for pod %s: %w", podUID, err))
		}
		delete(m.attachments, podUID)
	}
	return errors.Join(errs...)
}

func closeAttachment(attachment *podAttachment) error {
	var err error
	if attachment.link != nil {
		err = attachment.link.Close()
	}
	if attachment.objects != nil && attachment.objects.close != nil {
		attachment.objects.close()
	}
	return err
}
