/*
Copyright 2017 The Volcano Authors.

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

package job

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	kubeschedulinginformers "k8s.io/client-go/informers/scheduling/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	kubeschedulinglisters "k8s.io/client-go/listers/scheduling/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	busv1alpha1 "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcscheme "volcano.sh/apis/pkg/client/clientset/versioned/scheme"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
	batchinformer "volcano.sh/apis/pkg/client/informers/externalversions/batch/v1alpha1"
	businformer "volcano.sh/apis/pkg/client/informers/externalversions/bus/v1alpha1"
	schedulinginformers "volcano.sh/apis/pkg/client/informers/externalversions/scheduling/v1beta1"
	batchlister "volcano.sh/apis/pkg/client/listers/batch/v1alpha1"
	buslister "volcano.sh/apis/pkg/client/listers/bus/v1alpha1"
	schedulinglisters "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"

	"volcano.sh/volcano/pkg/controllers/apis"
	jobcache "volcano.sh/volcano/pkg/controllers/cache"
	"volcano.sh/volcano/pkg/controllers/framework"
	"volcano.sh/volcano/pkg/controllers/job/state"
	controllermetrics "volcano.sh/volcano/pkg/controllers/metrics"
	"volcano.sh/volcano/pkg/features"
)

func init() {
	framework.RegisterController(&jobcontroller{})
}

type delayAction struct {
	// The namespacing name of the job
	jobKey string
	// jobUID identifies the VCJob lifecycle that created this action.
	jobUID types.UID

	// The name of the task
	taskName string

	// The name of the pod
	podName string

	// The UID of the pod
	podUID types.UID

	partition string

	// The event caused the action
	event busv1alpha1.Event

	// The action to take.
	action busv1alpha1.Action

	// The delay before the action is executed
	delay time.Duration

	// The cancel function of the action
	cancel context.CancelFunc
}

func (d *delayAction) lifecycleKey() string {
	if d.jobUID == "" {
		return d.jobKey
	}
	return fmt.Sprintf("%s/%s", d.jobKey, d.jobUID)
}

func (d *delayAction) targetKey() string {
	if d.podUID == "" {
		return d.podName
	}
	return fmt.Sprintf("%s/%s", d.podName, d.podUID)
}

// jobcontroller the Job jobcontroller type.
type jobcontroller struct {
	kubeClient kubernetes.Interface
	vcClient   vcclientset.Interface

	jobInformer   batchinformer.JobInformer
	podInformer   coreinformers.PodInformer
	pvcInformer   coreinformers.PersistentVolumeClaimInformer
	pgInformer    schedulinginformers.PodGroupInformer
	svcInformer   coreinformers.ServiceInformer
	cmdInformer   businformer.CommandInformer
	pcInformer    kubeschedulinginformers.PriorityClassInformer
	queueInformer schedulinginformers.QueueInformer

	informerFactory   informers.SharedInformerFactory
	vcInformerFactory vcinformer.SharedInformerFactory

	// A store of jobs
	jobLister batchlister.JobLister
	jobSynced func() bool

	// A store of pods
	podLister corelisters.PodLister
	podSynced func() bool

	pvcLister corelisters.PersistentVolumeClaimLister
	pvcSynced func() bool

	// A store of podgroups
	pgLister schedulinglisters.PodGroupLister
	pgSynced func() bool

	// A store of service
	svcLister corelisters.ServiceLister
	svcSynced func() bool

	cmdLister buslister.CommandLister
	cmdSynced func() bool

	pcLister kubeschedulinglisters.PriorityClassLister
	pcSynced func() bool

	queueLister schedulinglisters.QueueLister
	queueSynced func() bool

	// queue that need to sync up
	queueList    []workqueue.TypedRateLimitingInterface[any]
	commandQueue workqueue.TypedRateLimitingInterface[any]
	cache        jobcache.Cache
	// Job Event recorder
	recorder record.EventRecorder

	errTasks      workqueue.TypedRateLimitingInterface[any]
	workers       uint32
	maxRequeueNum int

	delayActionMapLock sync.RWMutex
	// delayActionMap stores delayed actions for jobs, where outer map key is the
	// job lifecycle key (namespace/name/uid),
	// inner map key is the pod lifecycle key (name/uid), and value is the delayed action to be performed
	delayActionMap map[string]map[string]*delayAction
}

func (cc *jobcontroller) Name() string {
	return "job-controller"
}

// Initialize creates the new Job controller.
func (cc *jobcontroller) Initialize(opt *framework.ControllerOption) error {
	cc.kubeClient = opt.KubeClient
	cc.vcClient = opt.VolcanoClient

	sharedInformers := opt.SharedInformerFactory
	workers := opt.WorkerNum
	// Initialize event client
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: cc.kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(vcscheme.Scheme, v1.EventSource{Component: "vc-controller-manager"})

	cc.informerFactory = sharedInformers
	cc.queueList = make([]workqueue.TypedRateLimitingInterface[any], workers)
	cc.commandQueue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
	cc.cache = jobcache.New()
	cc.errTasks = newRateLimitingQueue()
	cc.recorder = recorder
	cc.workers = workers
	cc.maxRequeueNum = opt.MaxRequeueNum
	if cc.maxRequeueNum < 0 {
		cc.maxRequeueNum = -1
	}

	var i uint32
	for i = 0; i < workers; i++ {
		cc.queueList[i] = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
	}

	factory := opt.VCSharedInformerFactory
	cc.vcInformerFactory = factory
	if utilfeature.DefaultFeatureGate.Enabled(features.VolcanoJobSupport) {
		cc.jobInformer = factory.Batch().V1alpha1().Jobs()
		cc.jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    cc.addJob,
			UpdateFunc: cc.updateJob,
			DeleteFunc: cc.deleteJob,
		})
		cc.jobLister = cc.jobInformer.Lister()
		cc.jobSynced = cc.jobInformer.Informer().HasSynced
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.QueueCommandSync) {
		cc.cmdInformer = factory.Bus().V1alpha1().Commands()
		cc.cmdInformer.Informer().AddEventHandler(
			cache.FilteringResourceEventHandler{
				FilterFunc: func(obj interface{}) bool {
					switch v := obj.(type) {
					case *busv1alpha1.Command:
						if v.TargetObject != nil &&
							v.TargetObject.APIVersion == batchv1alpha1.SchemeGroupVersion.String() &&
							v.TargetObject.Kind == "Job" {
							return true
						}

						return false
					default:
						return false
					}
				},
				Handler: cache.ResourceEventHandlerFuncs{
					AddFunc: cc.addCommand,
				},
			},
		)
		cc.cmdLister = cc.cmdInformer.Lister()
		cc.cmdSynced = cc.cmdInformer.Informer().HasSynced
	}

	cc.podInformer = sharedInformers.Core().V1().Pods()
	cc.podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    cc.addPod,
		UpdateFunc: cc.updatePod,
		DeleteFunc: cc.deletePod,
	})

	cc.podLister = cc.podInformer.Lister()
	cc.podSynced = cc.podInformer.Informer().HasSynced

	cc.pvcInformer = sharedInformers.Core().V1().PersistentVolumeClaims()
	cc.pvcLister = cc.pvcInformer.Lister()
	cc.pvcSynced = cc.pvcInformer.Informer().HasSynced

	cc.svcInformer = sharedInformers.Core().V1().Services()
	cc.svcLister = cc.svcInformer.Lister()
	cc.svcSynced = cc.svcInformer.Informer().HasSynced

	cc.pgInformer = factory.Scheduling().V1beta1().PodGroups()
	cc.pgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: cc.updatePodGroup,
	})
	cc.pgLister = cc.pgInformer.Lister()
	cc.pgSynced = cc.pgInformer.Informer().HasSynced

	if utilfeature.DefaultFeatureGate.Enabled(features.PriorityClass) {
		cc.pcInformer = sharedInformers.Scheduling().V1().PriorityClasses()
		cc.pcLister = cc.pcInformer.Lister()
		cc.pcSynced = cc.pcInformer.Informer().HasSynced
	}

	cc.queueInformer = factory.Scheduling().V1beta1().Queues()
	cc.queueLister = cc.queueInformer.Lister()
	cc.queueSynced = cc.queueInformer.Informer().HasSynced

	cc.delayActionMap = make(map[string]map[string]*delayAction)

	// Register actions
	state.SyncJob = cc.syncJob
	state.KillJob = cc.killJob
	state.KillTarget = cc.killTarget
	return nil
}

// Run start JobController.
func (cc *jobcontroller) Run(stopCh <-chan struct{}) {
	cc.informerFactory.Start(stopCh)
	cc.vcInformerFactory.Start(stopCh)

	for informerType, ok := range cc.informerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			klog.Errorf("caches failed to sync: %v", informerType)
			return
		}
	}

	for informerType, ok := range cc.vcInformerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			klog.Errorf("caches failed to sync: %v", informerType)
			return
		}
	}

	go wait.Until(cc.handleCommands, 0, stopCh)
	var i uint32
	for i = 0; i < cc.workers; i++ {
		go func(num uint32) {
			wait.Until(
				func() {
					cc.worker(num)
				},
				time.Second,
				stopCh)
		}(i)
	}

	go cc.cache.Run(stopCh)

	// Re-sync error tasks.
	go wait.Until(cc.processResyncTask, 0, stopCh)

	klog.Infof("JobController is running ...... ")
}

func (cc *jobcontroller) worker(i uint32) {
	klog.Infof("worker %d start ...... ", i)

	for cc.processNextReq(i) {
	}
}

func (cc *jobcontroller) belongsToThisRoutine(key string, count uint32) bool {
	val := cc.genHash(key)
	return val%cc.workers == count
}

func (cc *jobcontroller) getWorkerQueue(key string) workqueue.TypedRateLimitingInterface[any] {
	val := cc.genHash(key)
	queue := cc.queueList[val%cc.workers]
	return queue
}

func (cc *jobcontroller) genHash(key string) uint32 {
	hashVal := fnv.New32()
	hashVal.Write([]byte(key))
	return hashVal.Sum32()
}

func (cc *jobcontroller) processNextReq(count uint32) bool {
	queue := cc.queueList[count]
	obj, shutdown := queue.Get()
	if shutdown {
		klog.Errorf("Fail to pop item from queue")
		return false
	}

	req := obj.(apis.Request)
	defer queue.Done(req)

	key := jobcache.JobKeyByReq(&req)
	if !cc.belongsToThisRoutine(key, count) {
		klog.Errorf("should not occur The job does not belongs to this routine key:%s, worker:%d...... ", key, count)
		queueLocal := cc.getWorkerQueue(key)
		queueLocal.Add(req)
		return true
	}

	klog.V(3).Infof("Try to handle request <%v>", req)

	cc.CleanPodDelayActionsIfNeed(req)

	jobInfo, err := cc.cache.Get(key)
	if err != nil {
		controllermetrics.IncJobControllerCacheMiss()
		klog.Errorf("Failed to get job by <%v> from cache: %v; recovering from informer", req, err)
		cc.recoverCurrentJob(queue, req)
		return true
	}

	if req.JobUid != "" && req.JobUid != jobInfo.UID {
		controllermetrics.IncJobControllerLifecycleMismatch("worker-request")
		klog.V(2).Infof("Ignore stale request for Job <%s> uid <%s>; current cached uid is <%s>", key, req.JobUid, jobInfo.UID)
		cc.recoverCurrentJob(queue, req)
		return true
	}

	st := state.NewState(jobInfo)
	if st == nil {
		klog.Errorf("Invalid state <%s> of Job <%v/%v>",
			jobInfo.Job.Status.State, jobInfo.Job.Namespace, jobInfo.Job.Name)
		return true
	}

	delayAct := applyPolicies(jobInfo.Job, &req)

	if delayAct.delay != 0 {
		klog.V(3).Infof("Execute <%v> on Job <%s/%s> after %s",
			delayAct.action, req.Namespace, req.JobName, delayAct.delay.String())
		cc.recordJobEvent(jobInfo.Job.Namespace, jobInfo.Job.Name, batchv1alpha1.ExecuteAction, fmt.Sprintf(
			"Execute action %s after %s", delayAct.action, delayAct.delay.String()))
		cc.AddDelayActionForJob(req, delayAct)
		// Registering the delayed action completes this queue attempt. In
		// particular, clear any rate-limit history left by an earlier retry;
		// the delayed action will enter the queue as a fresh request.
		queue.Forget(req)
		return true
	}

	klog.V(3).Infof("Execute <%v> on Job <%s/%s> in <%s> by <%T>.",
		delayAct.action, req.Namespace, req.JobName, jobInfo.Job.Status.State.Phase, st)

	if delayAct.action != busv1alpha1.SyncJobAction {
		cc.recordJobEvent(jobInfo.Job.Namespace, jobInfo.Job.Name, batchv1alpha1.ExecuteAction, fmt.Sprintf(
			"Start to execute action %s ", delayAct.action))
	}

	action := GetStateAction(delayAct)

	if err := st.Execute(action); err != nil {
		cc.handleJobError(queue, req, st, err, delayAct.action)
		return true
	}

	// If no error, forget it.
	queue.Forget(req)

	// If the action is not an internal action, cancel all delayed actions
	if !isInternalAction(delayAct.action) {
		cc.cleanupDelayActions(delayAct)
	}

	return true
}

// recoverCurrentJob repairs the controller cache from the informer and makes
// sure a current-lifecycle reconciliation remains queued. A cache miss must not
// permanently consume the only event capable of reconciling a recreated Job.
func (cc *jobcontroller) recoverCurrentJob(queue workqueue.TypedRateLimitingInterface[any], req apis.Request) {
	if cc.jobLister == nil {
		queue.AddRateLimited(req)
		return
	}

	job, err := cc.jobLister.Jobs(req.Namespace).Get(req.JobName)
	if apierrors.IsNotFound(err) {
		queue.Forget(req)
		return
	}
	if err != nil {
		klog.Errorf("Failed to recover Job <%s/%s> from informer: %v", req.Namespace, req.JobName, err)
		queue.AddRateLimited(req)
		return
	}

	key := jobcache.JobKeyByName(job.Namespace, job.Name)
	jobInfo, getErr := cc.cache.Get(key)
	if getErr != nil || jobInfo.UID != job.UID {
		if getErr == nil && jobInfo.UID != job.UID {
			// Add deliberately refuses to overwrite a live, different UID: a
			// standalone Add could be stale. Here the informer lister is the
			// source of truth, so retire the lifecycle we actually observed
			// before installing the current one.
			if err := cc.cache.Delete(jobInfo.Job); err != nil {
				klog.Errorf("Failed to retire stale Job <%s> uid <%s> during recovery: %v", key, jobInfo.UID, err)
				queue.AddRateLimited(req)
				return
			}
		}
		if err := cc.cache.Add(job); err != nil {
			// Another informer event may have repaired the same lifecycle first.
			if current, currentErr := cc.cache.Get(key); currentErr != nil || current.UID != job.UID {
				klog.Errorf("Failed to recover Job <%s> uid <%s> in cache: %v", key, job.UID, err)
				queue.AddRateLimited(req)
				return
			}
		}
	}

	recoveredReq := req
	if req.JobUid != "" && req.JobUid != job.UID {
		recoveredReq = apis.Request{
			Namespace: job.Namespace,
			JobName:   job.Name,
			JobUid:    job.UID,
			Event:     busv1alpha1.OutOfSyncEvent,
		}
	} else {
		recoveredReq.JobUid = job.UID
	}
	queue.Forget(req)
	queue.Add(recoveredReq)
}

// CleanPodDelayActionsIfNeed is used to clean delayed actions for Pod events when the pod phase changed:
// if the event is not PodPending event:
//   - cancel corresponding Pod Pending delayed action
//   - if the event is PodRunning state, cancel corresponding Pod Failed and Pod Evicted delayed actions
func (cc *jobcontroller) CleanPodDelayActionsIfNeed(req apis.Request) {
	// Skip cleaning delayed actions for non-pod events
	if !cc.isPodEvent(req) {
		return
	}

	if req.Event != busv1alpha1.PodPendingEvent {
		requestAction := &delayAction{
			jobKey:  jobcache.JobKeyByReq(&req),
			jobUID:  req.JobUid,
			podName: req.PodName,
			podUID:  req.PodUID,
		}
		key := requestAction.lifecycleKey()
		cc.delayActionMapLock.Lock()
		defer cc.delayActionMapLock.Unlock()

		if taskMap, exists := cc.delayActionMap[key]; exists {
			targetKey := requestAction.targetKey()
			if delayAct, exists := taskMap[targetKey]; exists {
				shouldCancel := false

				if delayAct.event == busv1alpha1.PodPendingEvent {
					// For PodPending delayed action, we need to check if the Pod UID matches
					// Because if a Pod is deleted and immediately recreated,
					// the new Pod's pending event may be queued before the old Pod's delete event
					if req.PodUID == delayAct.podUID {
						shouldCancel = true
					}
				}

				if (delayAct.event == busv1alpha1.PodFailedEvent || delayAct.event == busv1alpha1.PodEvictedEvent) &&
					req.Event == busv1alpha1.PodRunningEvent {
					shouldCancel = true
				}

				if shouldCancel {
					klog.V(3).Infof("Cancel delayed action <%v> for pod <%s> because of event <%s> of Job <%s>", delayAct.action, req.PodName, req.Event, delayAct.jobKey)
					delayAct.cancel()
					delete(taskMap, targetKey)
					if len(taskMap) == 0 {
						delete(cc.delayActionMap, key)
					}
				}
			}
		}
	}
}

func (cc *jobcontroller) isPodEvent(req apis.Request) bool {
	return req.Event == busv1alpha1.PodPendingEvent ||
		req.Event == busv1alpha1.PodRunningEvent ||
		req.Event == busv1alpha1.PodFailedEvent ||
		req.Event == busv1alpha1.PodEvictedEvent
}

func (cc *jobcontroller) AddDelayActionForJob(req apis.Request, delayAct *delayAction) {
	cc.delayActionMapLock.Lock()
	defer cc.delayActionMapLock.Unlock()

	lifecycleKey := delayAct.lifecycleKey()
	m, ok := cc.delayActionMap[lifecycleKey]
	if !ok {
		m = make(map[string]*delayAction)
		cc.delayActionMap[lifecycleKey] = m
	}
	targetKey := delayAct.targetKey()
	if oldDelayAct, exists := m[targetKey]; exists && oldDelayAct.action == delayAct.action {
		return
	} else if exists && oldDelayAct.cancel != nil {
		oldDelayAct.cancel()
	}
	m[targetKey] = delayAct

	ctx, cancel := context.WithTimeout(context.Background(), delayAct.delay)
	delayAct.cancel = cancel

	go func() {
		<-ctx.Done()
		if ctx.Err() == context.Canceled {
			klog.V(4).Infof("Job<%s/%s>'s delayed action %s is canceled", req.Namespace, req.JobName, delayAct.action)
			return
		}

		klog.V(4).Infof("Job<%s/%s> uid <%s>'s delayed action %s is expired, enqueue it", req.Namespace, req.JobName, delayAct.jobUID, delayAct.action)

		cc.removeDelayAction(delayAct)
		queue := cc.getWorkerQueue(delayAct.jobKey)
		delayedReq := req
		delayedReq.JobUid = delayAct.jobUID
		delayedReq.Action = delayAct.action
		queue.Add(delayedReq)
	}()
}

func (cc *jobcontroller) removeDelayAction(delayAct *delayAction) {
	cc.delayActionMapLock.Lock()
	defer cc.delayActionMapLock.Unlock()

	key := delayAct.lifecycleKey()
	m, exists := cc.delayActionMap[key]
	if !exists {
		return
	}
	if current, found := m[delayAct.targetKey()]; found && current == delayAct {
		delete(m, delayAct.targetKey())
	}
	if len(m) == 0 {
		delete(cc.delayActionMap, key)
	}
}

func (cc *jobcontroller) handleJobError(queue workqueue.TypedRateLimitingInterface[any], req apis.Request, st state.State, err error, action busv1alpha1.Action) {
	if cc.maxRequeueNum == -1 || queue.NumRequeues(req) < cc.maxRequeueNum {
		klog.V(2).Infof("Failed to handle Job <%s/%s>: %v",
			req.Namespace, req.JobName, err)
		queue.AddRateLimited(req)
		return
	}

	cc.recordJobEvent(req.Namespace, req.JobName, batchv1alpha1.ExecuteAction,
		fmt.Sprintf("Job failed on action %s for retry limit reached", action))
	klog.Warningf("Terminating Job <%s/%s> and releasing resources", req.Namespace, req.JobName)

	if err = st.Execute(state.Action{Action: busv1alpha1.TerminateJobAction}); err != nil {
		klog.Errorf("Failed to terminate Job<%s/%s>: %v", req.Namespace, req.JobName, err)
	}
	klog.Warningf("Dropping job<%s/%s> out of the queue: %v because max retries has reached",
		req.Namespace, req.JobName, err)
}

// cleanupDelayActions cleans up delayed actions
// After a delayed action is executed, other delayed actions of the same type need to be cleaned up to avoid duplicate execution
// Parameters:
//   - currentDelayAction: the delayed action that has just been executed
//
// Implementation logic:
//  1. Get the type of current delayed action (Job level, Task level or Pod level)
//  2. Iterate through all delayed actions under this Job
//  3. If the delayed action type matches, cancel it and remove from the map
//
// Usage scenarios:
//   - When a Pod failure triggers Job termination, need to cancel other Job delayed actions under this Job
func (cc *jobcontroller) cleanupDelayActions(currentDelayAction *delayAction) {
	cc.delayActionMapLock.Lock()
	defer cc.delayActionMapLock.Unlock()

	actionType := GetActionType(currentDelayAction.action)

	if m, exists := cc.delayActionMap[currentDelayAction.lifecycleKey()]; exists {
		for _, delayAct := range m {
			if GetActionType(delayAct.action) == actionType {
				// For Task level actions, only cancel delayed actions for the same task
				if actionType == TaskAction && delayAct.taskName != currentDelayAction.taskName {
					continue
				}
				// For Pod level actions, only cancel delayed actions for the same pod
				if actionType == PodAction && delayAct.podName != currentDelayAction.podName {
					continue
				}
				// For partition group level actions, only cancel delayed actions for the same group
				if actionType == PartitionAction && delayAct.partition != currentDelayAction.partition {
					continue
				}

				if delayAct.cancel != nil {
					klog.V(3).Infof("Cancel delayed action <%v> for pod <%s> because of event <%s> and action <%s> of Job <%s>", delayAct.action, delayAct.podName, currentDelayAction.event, currentDelayAction.action, delayAct.jobKey)
					delayAct.cancel()
				}
				delete(m, delayAct.targetKey())
			}
		}
		if len(m) == 0 {
			delete(cc.delayActionMap, currentDelayAction.lifecycleKey())
		}
	}
}
