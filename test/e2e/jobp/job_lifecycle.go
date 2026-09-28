/*
Copyright 2021 The Volcano Authors.

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

package jobp

import (
	"context"
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	vcbatch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	vcbus "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	"volcano.sh/apis/pkg/apis/helpers"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("Job Life Cycle", func() {
	DescribeTable("Cleans initialized plugin resources when terminating a Pending Job", func(plugin string) {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "pending-plugin-cleanup", Plugins: map[string][]string{plugin: {}},
			Tasks: []e2eutil.TaskSpec{{Name: "head", Img: e2eutil.DefaultBusyBoxImage, Min: 1, Rep: 1,
				Command: "sleep 3600", RestartPolicy: v1.RestartPolicyNever, Req: e2eutil.CPUResource("1000000")}},
		})
		getResource := func() (metav1.Object, error) {
			if plugin == "ssh" {
				return ctx.Kubeclient.CoreV1().Secrets(job.Namespace).Get(context.TODO(), job.Name+"-ssh", metav1.GetOptions{})
			}
			name := job.Name
			if plugin == "ray" {
				name += "-head-svc"
			}
			return ctx.Kubeclient.CoreV1().Services(job.Namespace).Get(context.TODO(), name, metav1.GetOptions{})
		}
		By("persisting plugin initialization while gang admission remains Pending")
		Eventually(func() bool {
			current, err := ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
			return err == nil && current.Status.State.Phase == vcbatch.Pending && current.Status.ControlledResources["plugin-"+plugin] != ""
		}, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())
		obj, err := getResource()
		Expect(err).NotTo(HaveOccurred())
		Expect(metav1.IsControlledBy(obj, job)).To(BeTrue())
		Expect(e2eutil.GetTasksOfJob(ctx, job)).To(BeEmpty())

		By("terminating rather than deleting the Job, so GC cannot mask a cleanup failure")
		_, err = ctx.Vcclient.BusV1alpha1().Commands(job.Namespace).Create(context.TODO(), &vcbus.Command{
			ObjectMeta:   metav1.ObjectMeta{GenerateName: "terminate-pending-", Namespace: job.Namespace},
			TargetObject: metav1.NewControllerRef(job, helpers.JobKind), Action: string(vcbus.TerminateJobAction),
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			current, err := ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
			return err == nil && current.UID == job.UID && current.DeletionTimestamp == nil && current.Status.State.Phase == vcbatch.Terminated
		}, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())
		Eventually(func() bool { _, err := getResource(); return apierrors.IsNotFound(err) }, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())
		if plugin == "svc" {
			_, err = ctx.Kubeclient.NetworkingV1().NetworkPolicies(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			cms, err := ctx.Kubeclient.CoreV1().ConfigMaps(job.Namespace).List(context.TODO(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for i := range cms.Items {
				Expect(metav1.IsControlledBy(&cms.Items[i], job)).To(BeFalse())
			}
		}
	}, Entry("Service plugin", "svc"), Entry("SSH plugin", "ssh"), Entry("Ray plugin", "ray"))

	DescribeTable("Waits for previous lifecycle plugin resources", func(plugin string) {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)
		jobSpec := &e2eutil.JobSpec{
			Name: "plugin-name-reuse", Plugins: map[string][]string{plugin: {}},
			Tasks: []e2eutil.TaskSpec{
				{Name: "head", Img: e2eutil.DefaultBusyBoxImage, Min: 1, Rep: 1, Command: "sleep 3600", RestartPolicy: v1.RestartPolicyNever},
				{Name: "worker", Img: e2eutil.DefaultBusyBoxImage, Min: 1, Rep: 1, Command: "sleep 3600", RestartPolicy: v1.RestartPolicyNever},
			},
		}
		if plugin == "ray" {
			// Exercise Service lifecycle, not the Ray runtime. Do not replace
			// the BusyBox containers' sleeping commands with `ray start`.
			jobSpec.Plugins[plugin] = []string{"--headContainer=ray-runtime", "--workerContainer=ray-runtime"}
		}
		old := e2eutil.CreateJob(ctx, jobSpec)
		Expect(e2eutil.WaitJobReady(ctx, old)).To(Succeed())
		getResource := func() (metav1.Object, error) {
			if plugin == "ssh" {
				return ctx.Kubeclient.CoreV1().Secrets(old.Namespace).Get(context.TODO(), old.Name+"-ssh", metav1.GetOptions{})
			}
			return ctx.Kubeclient.CoreV1().Services(old.Namespace).Get(context.TODO(), old.Name+"-head-svc", metav1.GetOptions{})
		}
		updateResource := func(obj metav1.Object) error {
			switch resource := obj.(type) {
			case *v1.Secret:
				_, err := ctx.Kubeclient.CoreV1().Secrets(old.Namespace).Update(context.TODO(), resource, metav1.UpdateOptions{})
				return err
			case *v1.Service:
				_, err := ctx.Kubeclient.CoreV1().Services(old.Namespace).Update(context.TODO(), resource, metav1.UpdateOptions{})
				return err
			default:
				return fmt.Errorf("unexpected resource %T", obj)
			}
		}
		resource, err := getResource()
		Expect(err).NotTo(HaveOccurred())
		oldResourceUID := resource.GetUID()
		const holdFinalizer = "e2e.volcano.sh/lifecycle-hold"
		releaseResource := func() error {
			return retry.RetryOnConflict(retry.DefaultRetry, func() error {
				obj, err := getResource()
				if apierrors.IsNotFound(err) {
					return nil
				}
				if err != nil {
					return err
				}
				if obj.GetUID() != oldResourceUID {
					return nil
				}
				obj.SetFinalizers(slices.DeleteFunc(obj.GetFinalizers(), func(value string) bool { return value == holdFinalizer }))
				return updateResource(obj)
			})
		}
		// Release only our old resource's finalizer, even if an assertion fails.
		defer func() { Expect(releaseResource()).To(Succeed()) }()
		resource.SetFinalizers(append(resource.GetFinalizers(), holdFinalizer))
		Expect(updateResource(resource)).To(Succeed())

		By("holding the old plugin resource across background Job deletion")
		background := metav1.DeletePropagationBackground
		Expect(ctx.Vcclient.BatchV1alpha1().Jobs(old.Namespace).Delete(context.TODO(), old.Name,
			metav1.DeleteOptions{PropagationPolicy: &background, Preconditions: &metav1.Preconditions{UID: &old.UID}})).To(Succeed())
		Eventually(func() bool {
			_, err := ctx.Vcclient.BatchV1alpha1().Jobs(old.Namespace).Get(context.TODO(), old.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())
		Eventually(func() bool {
			obj, err := getResource()
			return err == nil && obj.GetUID() == oldResourceUID && obj.GetDeletionTimestamp() != nil
		}, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())

		current := e2eutil.CreateJob(ctx, jobSpec)
		Expect(current.UID).NotTo(Equal(old.UID))
		By("observing the plugin conflict without reusing the old resource")
		Eventually(func() bool {
			events, err := ctx.Kubeclient.CoreV1().Events(current.Namespace).List(context.TODO(), metav1.ListOptions{
				FieldSelector: "involvedObject.uid=" + string(current.UID),
			})
			if err != nil {
				return false
			}
			for _, event := range events.Items {
				if event.Reason == string(vcbatch.PluginError) {
					return true
				}
			}
			return false
		}, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())
		Consistently(func() error {
			job, err := ctx.Vcclient.BatchV1alpha1().Jobs(current.Namespace).Get(context.TODO(), current.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if job.UID != current.UID || job.Status.State.Phase != vcbatch.Pending || job.Status.Version != 0 || job.Status.RetryCount != 0 {
				return fmt.Errorf("name conflict changed the new Job lifecycle: %+v", job.Status)
			}
			obj, err := getResource()
			if err != nil {
				return err
			}
			if obj.GetUID() != oldResourceUID || !metav1.IsControlledBy(obj, old) {
				return fmt.Errorf("old resource was replaced or adopted during the wait")
			}
			pods, err := ctx.Kubeclient.CoreV1().Pods(current.Namespace).List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				return err
			}
			for i := range pods.Items {
				if metav1.IsControlledBy(&pods.Items[i], current) {
					return fmt.Errorf("new Pod created before plugin name release")
				}
			}
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

		By("releasing the old resource and recovering the same new Job UID")
		Expect(releaseResource()).To(Succeed())
		Expect(e2eutil.WaitJobReady(ctx, current)).To(Succeed())
		Consistently(func() bool {
			obj, err := getResource()
			return err == nil && obj.GetUID() != oldResourceUID && metav1.IsControlledBy(obj, current) && obj.GetDeletionTimestamp() == nil
		}, 5*time.Second, 500*time.Millisecond).Should(BeTrue())
		job, err := ctx.Vcclient.BatchV1alpha1().Jobs(current.Namespace).Get(context.TODO(), current.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(job.UID).To(Equal(current.UID))
		Expect(job.Status.Version).To(BeZero())
		Expect(job.Status.RetryCount).To(BeZero())
	},
		Entry("SSH Secret", "ssh"),
		Entry("Ray head Service", "ray"),
	)

	It("Reconciles a VCJob recreated with the same name after stale cleanup retries", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		const replicas int32 = 8
		var terminationGracePeriodSeconds int64 = 15
		jobSpec := &e2eutil.JobSpec{
			Name:     "same-name-recreate-job",
			Plugins:  map[string][]string{"svc": {}},
			Policies: []vcbatch.LifecyclePolicy{{Events: []vcbus.Event{vcbus.PodEvictedEvent}, Action: vcbus.RestartJobAction}},
			Tasks: []e2eutil.TaskSpec{{
				Name:                  "worker",
				Img:                   e2eutil.DefaultBusyBoxImage,
				Min:                   replicas,
				Rep:                   replicas,
				Command:               `trap '' TERM; while true; do sleep 1; done`,
				RestartPolicy:         v1.RestartPolicyNever,
				DefaultGracefulPeriod: &terminationGracePeriodSeconds,
			}},
		}

		By("creating the first VCJob lifecycle")
		oldJob := e2eutil.CreateJob(ctx, jobSpec)
		Expect(e2eutil.WaitJobReady(ctx, oldJob)).To(Succeed())
		oldPods := e2eutil.GetTasksOfJob(ctx, oldJob)
		Expect(oldPods).To(HaveLen(int(replicas)))
		for round := 0; round < 3; round++ {
			By(fmt.Sprintf("repeating same-name lifecycle replacement, round %d", round+1))

			By("deleting the first VCJob while keeping its Pods terminating")
			Expect(ctx.Vcclient.BatchV1alpha1().Jobs(oldJob.Namespace).Delete(
				context.TODO(), oldJob.Name, metav1.DeleteOptions{})).To(Succeed())
			Eventually(func() bool {
				_, err := ctx.Vcclient.BatchV1alpha1().Jobs(oldJob.Namespace).Get(
					context.TODO(), oldJob.Name, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())
			Eventually(func() bool {
				pods, err := ctx.Kubeclient.CoreV1().Pods(oldJob.Namespace).List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					return false
				}
				for i := range pods.Items {
					owner := metav1.GetControllerOf(&pods.Items[i])
					if owner != nil && owner.UID == oldJob.UID && pods.Items[i].DeletionTimestamp != nil {
						return true
					}
				}
				return false
			}, e2eutil.OneMinute, 200*time.Millisecond).Should(BeTrue())

			By("creating a new VCJob lifecycle with the same namespace and name")
			newJob := e2eutil.CreateJob(ctx, jobSpec)
			Expect(newJob.UID).NotTo(Equal(oldJob.UID))
			Expect(e2eutil.WaitJobReady(ctx, newJob)).To(Succeed())

			currentPods := func() ([]v1.Pod, error) {
				podList, err := ctx.Kubeclient.CoreV1().Pods(newJob.Namespace).List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				pods := make([]v1.Pod, 0, replicas)
				for i := range podList.Items {
					owner := metav1.GetControllerOf(&podList.Items[i])
					if owner != nil && owner.UID == newJob.UID {
						pods = append(pods, podList.Items[i])
					}
				}
				return pods, nil
			}

			By("waiting for all Pods owned by the new lifecycle")
			Eventually(func() error {
				pods, err := currentPods()
				if err != nil {
					return err
				}
				if len(pods) != int(replicas) {
					return fmt.Errorf("got %d Pods owned by new Job UID %s, want %d", len(pods), newJob.UID, replicas)
				}
				return nil
			}, e2eutil.TwoMinute, 500*time.Millisecond).Should(Succeed())

			By("waiting beyond the old cleanup backoff and verifying the new lifecycle remains healthy")
			Consistently(func() error {
				job, err := ctx.Vcclient.BatchV1alpha1().Jobs(newJob.Namespace).Get(
					context.TODO(), newJob.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if job.UID != newJob.UID {
					return fmt.Errorf("current Job UID changed from %s to %s", newJob.UID, job.UID)
				}
				pods, err := currentPods()
				if err != nil {
					return err
				}
				if len(pods) != int(replicas) {
					return fmt.Errorf("got %d Pods after stale cleanup window, want %d", len(pods), replicas)
				}
				return nil
			}, 15*time.Second, time.Second).Should(Succeed())

			By("deleting one current Pod and verifying reconciliation still works")
			before, err := ctx.Vcclient.BatchV1alpha1().Jobs(newJob.Namespace).Get(context.TODO(), newJob.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(before.Status.Version).To(BeZero(), "old lifecycle events must not restart the new Job")
			Expect(before.Status.RetryCount).To(BeZero())
			pods, err := currentPods()
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(HaveLen(int(replicas)))
			deletedPod := pods[0]
			zero := int64(0)
			Expect(ctx.Kubeclient.CoreV1().Pods(newJob.Namespace).Delete(context.TODO(), deletedPod.Name,
				metav1.DeleteOptions{GracePeriodSeconds: &zero})).To(Succeed())
			Eventually(func() types.UID {
				pod, err := ctx.Kubeclient.CoreV1().Pods(newJob.Namespace).Get(
					context.TODO(), deletedPod.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				owner := metav1.GetControllerOf(pod)
				if owner == nil || owner.UID != newJob.UID {
					return ""
				}
				return pod.UID
			}, e2eutil.TwoMinute, 500*time.Millisecond).Should(And(Not(BeEmpty()), Not(Equal(deletedPod.UID))))
			Expect(e2eutil.WaitJobReady(ctx, newJob)).To(Succeed())
			Eventually(func() bool {
				current, err := ctx.Vcclient.BatchV1alpha1().Jobs(newJob.Namespace).Get(context.TODO(), newJob.Name, metav1.GetOptions{})
				if err != nil || current.Status.Version != 1 || current.Status.RetryCount != 1 {
					return false
				}
				svc, err := ctx.Kubeclient.CoreV1().Services(newJob.Namespace).Get(context.TODO(), newJob.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				owner := metav1.GetControllerOf(svc)
				return owner != nil && owner.UID == newJob.UID
			}, e2eutil.TwoMinute, 500*time.Millisecond).Should(BeTrue(), "current eviction policy and owned Service must recover")
			oldJob = newJob
		}
	})

	DescribeTable("Preserves targeted lifecycle recovery", func(action vcbus.Action) {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)
		policy := vcbatch.LifecyclePolicy{Events: []vcbus.Event{vcbus.PodEvictedEvent}, Action: action}
		if action == vcbus.RestartPodAction {
			policy.Timeout = &metav1.Duration{Duration: time.Second}
		}
		worker := e2eutil.TaskSpec{Name: "worker", Img: e2eutil.DefaultBusyBoxImage, Min: 4, Rep: 4, Command: "sleep 3600", RestartPolicy: v1.RestartPolicyNever}
		if action == vcbus.RestartPartitionAction {
			worker.PartitionPolicy = &vcbatch.PartitionPolicySpec{TotalPartitions: 2, PartitionSize: 2}
		}
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{Name: "targeted-lifecycle", Policies: []vcbatch.LifecyclePolicy{policy}, Tasks: []e2eutil.TaskSpec{
			worker, {Name: "other", Img: e2eutil.DefaultBusyBoxImage, Min: 1, Rep: 1, Command: "sleep 3600", RestartPolicy: v1.RestartPolicyNever},
		}})
		Expect(e2eutil.WaitJobReady(ctx, job)).To(Succeed())
		original := e2eutil.GetTasksOfJob(ctx, job)
		Expect(original).To(HaveLen(5))
		victim, err := ctx.Kubeclient.CoreV1().Pods(job.Namespace).Get(context.TODO(), job.Name+"-worker-0", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if action == vcbus.RestartPartitionAction {
			Expect(victim.Labels[vcbatch.TaskPartitionID]).NotTo(BeEmpty())
		}
		zero := int64(0)
		Expect(ctx.Kubeclient.CoreV1().Pods(job.Namespace).Delete(context.TODO(), victim.Name,
			metav1.DeleteOptions{GracePeriodSeconds: &zero, Preconditions: &metav1.Preconditions{UID: &victim.UID}})).To(Succeed())
		Eventually(func() error {
			for _, old := range original {
				current, err := ctx.Kubeclient.CoreV1().Pods(job.Namespace).Get(context.TODO(), old.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				owner := metav1.GetControllerOf(current)
				if owner == nil || owner.UID != job.UID || current.Status.Phase != v1.PodRunning || current.DeletionTimestamp != nil {
					return fmt.Errorf("Pod %s is not ready in current lifecycle", current.Name)
				}
				restarted := old.Name == victim.Name
				if action == vcbus.RestartTaskAction {
					restarted = old.Annotations[vcbatch.TaskSpecKey] == "worker"
				}
				if action == vcbus.RestartPartitionAction {
					restarted = old.Annotations[vcbatch.TaskSpecKey] == "worker" && old.Labels[vcbatch.TaskPartitionID] == victim.Labels[vcbatch.TaskPartitionID]
				}
				if (current.UID != old.UID) != restarted {
					return fmt.Errorf("Pod %s changed UID=%v, expected=%v", old.Name, current.UID != old.UID, restarted)
				}
			}
			return nil
		}, e2eutil.TwoMinute, 500*time.Millisecond).Should(Succeed())
		current, err := ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(current.Status.Version).To(BeZero(), "targeted actions must not bump the Job version")
	},
		Entry("RestartPod with timeout after the target is deleted", vcbus.RestartPodAction),
		Entry("RestartTask retains Pods outside the task", vcbus.RestartTaskAction),
		Entry("RestartPartition retains Pods outside the partition", vcbus.RestartPartitionAction),
	)

	It("Delete job that is pending state", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		By("create job")
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "pending-delete-job",
			Tasks: []e2eutil.TaskSpec{
				{
					Name: "success",
					Img:  e2eutil.DefaultNginxImage,
					Min:  2,
					Rep:  2,
					Req:  e2eutil.CPUResource("10000"),
				},
			},
		})

		// job phase: pending
		err := e2eutil.WaitJobPhases(ctx, job, []vcbatch.JobPhase{vcbatch.Pending})
		Expect(err).NotTo(HaveOccurred())

		By("delete job")
		err = ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Delete(context.TODO(), job.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = e2eutil.WaitJobCleanedUp(ctx, job)
		Expect(err).NotTo(HaveOccurred())

	})

	It("Delete job that is Running state", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		By("create job")
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "running-delete-job",
			Tasks: []e2eutil.TaskSpec{
				{
					Name: "success",
					Img:  e2eutil.DefaultNginxImage,
					Min:  2,
					Rep:  2,
				},
			},
		})

		// job phase: pending -> running
		err := e2eutil.WaitJobPhases(ctx, job, []vcbatch.JobPhase{vcbatch.Pending, vcbatch.Running})
		Expect(err).NotTo(HaveOccurred())

		By("delete job")
		err = ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Delete(context.TODO(), job.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = e2eutil.WaitJobCleanedUp(ctx, job)
		Expect(err).NotTo(HaveOccurred())

	})

	It("Delete job that is Completed state", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		By("create job")
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "complete-delete-job",
			Tasks: []e2eutil.TaskSpec{
				{
					Name: "completed-task",
					Img:  e2eutil.DefaultBusyBoxImage,
					Min:  2,
					Rep:  2,
					// Sleep 5 seconds ensure job in running state
					Command: "sleep 5",
				},
			},
		})

		// job phase: pending -> running -> Completed
		err := e2eutil.WaitJobPhases(ctx, job, []vcbatch.JobPhase{vcbatch.Pending, vcbatch.Running, vcbatch.Completed})
		Expect(err).NotTo(HaveOccurred())

		By("delete job")
		err = ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Delete(context.TODO(), job.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = e2eutil.WaitJobCleanedUp(ctx, job)
		Expect(err).NotTo(HaveOccurred())

	})

	It("Delete job that is Failed job", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		By("create job")
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "failed-delete-job",
			Policies: []vcbatch.LifecyclePolicy{
				{
					Action: vcbus.AbortJobAction,
					Event:  vcbus.PodFailedEvent,
				},
			},
			Tasks: []e2eutil.TaskSpec{
				{
					Name:          "fail",
					Img:           e2eutil.DefaultNginxImage,
					Min:           1,
					Rep:           1,
					Command:       "sleep 10s && exit 3",
					RestartPolicy: v1.RestartPolicyNever,
				},
			},
		})

		// job phase: pending -> running -> Aborted
		err := e2eutil.WaitJobPhases(ctx, job, []vcbatch.JobPhase{vcbatch.Pending, vcbatch.Running, vcbatch.Aborted})
		Expect(err).NotTo(HaveOccurred())

		By("delete job")
		err = ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Delete(context.TODO(), job.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = e2eutil.WaitJobCleanedUp(ctx, job)
		Expect(err).NotTo(HaveOccurred())

	})

	It("Delete job that is terminated job", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		By("create job")
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "terminate-delete-job",
			Policies: []vcbatch.LifecyclePolicy{
				{
					Action: vcbus.TerminateJobAction,
					Event:  vcbus.PodFailedEvent,
				},
			},
			Tasks: []e2eutil.TaskSpec{
				{
					Name:          "fail",
					Img:           e2eutil.DefaultNginxImage,
					Min:           1,
					Rep:           1,
					Command:       "sleep 10s && exit 3",
					RestartPolicy: v1.RestartPolicyNever,
				},
			},
		})

		// job phase: pending -> running -> Terminated
		err := e2eutil.WaitJobPhases(ctx, job, []vcbatch.JobPhase{vcbatch.Pending, vcbatch.Running, vcbatch.Terminated})
		Expect(err).NotTo(HaveOccurred())

		By("delete job")
		err = ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Delete(context.TODO(), job.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = e2eutil.WaitJobCleanedUp(ctx, job)
		Expect(err).NotTo(HaveOccurred())

	})

	It("Create and Delete job with CPU requirement", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		By("create job")
		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "terminate-delete-job",
			Policies: []vcbatch.LifecyclePolicy{
				{
					Action: vcbus.TerminateJobAction,
					Event:  vcbus.PodFailedEvent,
				},
			},
			Tasks: []e2eutil.TaskSpec{
				{
					Name:          "complete",
					Img:           e2eutil.DefaultNginxImage,
					Min:           1,
					Rep:           1,
					Command:       "sleep 10s",
					RestartPolicy: v1.RestartPolicyNever,
					Req:           e2eutil.CPUResource("1"),
				},
			},
		})

		// job phase: pending -> running -> completed
		err := e2eutil.WaitJobPhases(ctx, job, []vcbatch.JobPhase{vcbatch.Pending, vcbatch.Running, vcbatch.Completed})
		Expect(err).NotTo(HaveOccurred())

		By("delete job")
		err = ctx.Vcclient.BatchV1alpha1().Jobs(job.Namespace).Delete(context.TODO(), job.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = e2eutil.WaitJobCleanedUp(ctx, job)
		Expect(err).NotTo(HaveOccurred())

	})

	It("Checking Event Generation for job", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "terminate-job",
			Policies: []vcbatch.LifecyclePolicy{
				{
					Action: vcbus.TerminateJobAction,
					Event:  vcbus.PodFailedEvent,
				},
			},
			Tasks: []e2eutil.TaskSpec{
				{
					Name:          "complete",
					Img:           e2eutil.DefaultNginxImage,
					Min:           1,
					Rep:           1,
					Command:       "sleep 10s && xyz",
					RestartPolicy: v1.RestartPolicyNever,
				},
			},
		})

		err := e2eutil.WaitJobTerminateAction(ctx, job)
		Expect(err).NotTo(HaveOccurred())
	})

	It("Checking Unschedulable Event Generation for job", func() {
		ctx := e2eutil.InitTestContext(e2eutil.Options{})
		defer e2eutil.CleanupTestContext(ctx)

		nodeName, rep := e2eutil.ComputeNode(ctx, e2eutil.OneCPU)

		nodeAffinity := &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      v1.LabelHostname,
								Operator: v1.NodeSelectorOpIn,
								Values:   []string{nodeName},
							},
						},
					},
				},
			},
		}

		job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
			Name: "unschedulable-job",
			Policies: []vcbatch.LifecyclePolicy{
				{
					Action: vcbus.TerminateJobAction,
					Event:  vcbus.PodFailedEvent,
				},
			},
			Tasks: []e2eutil.TaskSpec{
				{
					Name:          "complete",
					Img:           e2eutil.DefaultNginxImage,
					Min:           rep + 1,
					Rep:           rep + 1,
					Command:       "sleep 10s",
					RestartPolicy: v1.RestartPolicyNever,
					Req:           e2eutil.CPUResource("1"),
					Limit:         e2eutil.CPUResource("1"),
					Affinity:      &v1.Affinity{NodeAffinity: nodeAffinity},
				},
			},
		})

		err := e2eutil.WaitJobUnschedulable(ctx, job)
		Expect(err).NotTo(HaveOccurred())
	})

})
