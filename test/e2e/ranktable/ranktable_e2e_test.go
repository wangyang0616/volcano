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

package ranktable

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"volcano.sh/volcano/pkg/controllers/ranktable/aggregator"
	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var (
	ctx          *e2eutil.TestContext
	aggImage     string
	aggCmd       string
	enableAggE2E bool
)

var _ = Describe("RankTable Aggregator E2E", func() {
	BeforeEach(func() {
		enableAggE2E = os.Getenv("ENABLE_RANKTABLE_E2E") == "true"
		aggImage = os.Getenv("RANKTABLE_AGGREGATOR_IMAGE")
		aggCmd = os.Getenv("RANKTABLE_AGGREGATOR_CMD")
		if aggCmd == "" {
			aggCmd = "/vc-ranktable-aggregator"
		}
		if !enableAggE2E || aggImage == "" {
			Skip("set ENABLE_RANKTABLE_E2E=true and RANKTABLE_AGGREGATOR_IMAGE=<image> to run ranktable e2e")
		}
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
	})

	AfterEach(func() {
		if ctx != nil {
			e2eutil.CleanupTestContext(ctx)
		}
	})

	It("bootstrap success: init assembles ranktable and workload can read file", func() {
		const (
			indexName  = "rt-index-job-001"
			shardName  = "rt-shard-job-001-v1-0"
			podName    = "rt-consumer"
			outputPath = "/etc/ranktable/jobstart_hccl.json"
			indexPath  = "/etc/ranktable/index/index.yaml"
		)
		content := []byte(`{"rank":0,"job":"job-001"}`)
		sha := aggregator.Sha256Hex(content)

		By("create shard configmap")
		shardCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      shardName,
				Namespace: ctx.Namespace,
				Labels: map[string]string{
					"volcano.sh/job-id":            "job-001",
					"volcano.sh/ranktable-type":    "shard",
					"volcano.sh/ranktable-version": "v1",
				},
			},
			Data: map[string]string{
				aggregator.ShardDataKey: base64.StdEncoding.EncodeToString(content),
			},
		}
		_, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), shardCM, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create index configmap")
		indexYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ranktable_cur_version: "v1"
  ranktable_prev_version: ""
  status: "completed"
  protocol_version: "v1.0"
  encoding: "identity"
  chunk_size: "819200"
  total_shards: "1"
  compressed_size: "%d"
  original_size: "%d"
  compressed_sha256: "%s"
  content_sha256: "%s"
  changed_shards: "[0]"
  shards: |
    [{"id":0,"namespace":"%s","name":"%s","size":%d,"sha256":"%s"}]
`, indexName, ctx.Namespace, len(content), len(content), sha, sha, ctx.Namespace, shardName, len(content), sha)

		indexCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: indexName, Namespace: ctx.Namespace},
			Data: map[string]string{
				"index.yaml": indexYAML,
			},
		}
		_, err = ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), indexCM, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create minimal rbac for pod service account")
		_, err = ctx.Kubeclient.RbacV1().Roles(ctx.Namespace).Create(context.TODO(), &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator", Namespace: ctx.Namespace},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctx.Kubeclient.RbacV1().RoleBindings(ctx.Namespace).Create(context.TODO(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator", Namespace: ctx.Namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ranktable-aggregator"},
			Subjects: []rbacv1.Subject{
				{Kind: "ServiceAccount", Name: "default", Namespace: ctx.Namespace},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create pod with native sidecar (single process) + workload")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ctx.Namespace},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyAlways,
				ServiceAccountName: "default",
				Volumes: []corev1.Volume{
					{
						Name: "ranktable-index",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: indexName},
								Items:                []corev1.KeyToPath{{Key: "index.yaml", Path: "index.yaml"}},
							},
						},
					},
					{
						Name:         "ranktable-shared",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					},
				},
				InitContainers: []corev1.Container{
					{
						Name:          "ranktable-aggregator",
						RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
						Image:         aggImage,
						Command:       []string{aggCmd},
						Args: []string{
							"-index-file-path=" + indexPath,
							"-output-path=" + outputPath,
							"-kube-api-qps=3",
							"-workers=2",
							"-poll-interval=15s",
							"-startup-jitter=0s",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-index", MountPath: "/etc/ranktable/index"},
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:  "workload",
						Image: e2eutil.DefaultBusyBoxImage,
						Command: []string{"sh", "-c",
							"while ! test -s " + outputPath + "; do sleep 1; done; grep -q '\"rank\":0' " + outputPath + " && sleep 3600",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
			},
		}
		_, err = ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("wait pod ready")
		Expect(e2eutil.WaitPodReady(ctx, pod)).NotTo(HaveOccurred())
	})

	It("sidecar refresh V1->V2 (TODO)", func() {
		const (
			indexName  = "rt-index-job-002"
			shardV1    = "rt-shard-job-002-v1-0"
			shardV2    = "rt-shard-job-002-v2-0"
			podName    = "rt-consumer-v2"
			outputPath = "/etc/ranktable/jobstart_hccl.json"
			indexPath  = "/etc/ranktable/index/index.yaml"
		)
		v1Content := []byte(`{"rank":0,"version":"v1"}`)
		v2Content := []byte(`{"rank":0,"version":"v2"}`)
		v1SHA := aggregator.Sha256Hex(v1Content)
		v2SHA := aggregator.Sha256Hex(v2Content)

		By("create v1 shard and index")
		_, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      shardV1,
				Namespace: ctx.Namespace,
				Labels: map[string]string{
					"volcano.sh/job-id":            "job-002",
					"volcano.sh/ranktable-type":    "shard",
					"volcano.sh/ranktable-version": "v1",
				},
			},
			Data: map[string]string{aggregator.ShardDataKey: base64.StdEncoding.EncodeToString(v1Content)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		v1IndexYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ranktable_cur_version: "v1"
  ranktable_prev_version: ""
  status: "completed"
  protocol_version: "v1.0"
  encoding: "identity"
  chunk_size: "819200"
  total_shards: "1"
  compressed_size: "%d"
  original_size: "%d"
  compressed_sha256: "%s"
  content_sha256: "%s"
  changed_shards: "[0]"
  shards: |
    [{"id":0,"namespace":"%s","name":"%s","size":%d,"sha256":"%s"}]
`, indexName, ctx.Namespace, len(v1Content), len(v1Content), v1SHA, v1SHA, ctx.Namespace, shardV1, len(v1Content), v1SHA)

		indexCM, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: indexName, Namespace: ctx.Namespace},
			Data:       map[string]string{"index.yaml": v1IndexYAML},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create minimal rbac for pod service account")
		_, err = ctx.Kubeclient.RbacV1().Roles(ctx.Namespace).Create(context.TODO(), &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator-v2", Namespace: ctx.Namespace},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctx.Kubeclient.RbacV1().RoleBindings(ctx.Namespace).Create(context.TODO(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator-v2", Namespace: ctx.Namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ranktable-aggregator-v2"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "default", Namespace: ctx.Namespace}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create pod with native sidecar + workload logger")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ctx.Namespace},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyAlways,
				ServiceAccountName: "default",
				Volumes: []corev1.Volume{
					{Name: "ranktable-index", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: indexName},
						Items:                []corev1.KeyToPath{{Key: "index.yaml", Path: "index.yaml"}},
					}}},
					{Name: "ranktable-shared", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
				InitContainers: []corev1.Container{
					{
						Name:          "ranktable-aggregator",
						RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
						Image:         aggImage,
						Command:       []string{aggCmd},
						Args: []string{
							"-index-file-path=" + indexPath,
							"-output-path=" + outputPath,
							"-poll-interval=2s",
							"-startup-jitter=0s",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-index", MountPath: "/etc/ranktable/index"},
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:    "workload",
						Image:   e2eutil.DefaultBusyBoxImage,
						Command: []string{"sh", "-c", "while true; do test -s " + outputPath + " && cat " + outputPath + " || true; sleep 2; done"},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
			},
		}
		_, err = ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2eutil.WaitPodReady(ctx, pod)).NotTo(HaveOccurred())

		By("verify workload sees v1 content")
		Eventually(func() string {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "workload"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return string(b)
		}, 60*time.Second, 2*time.Second).Should(ContainSubstring(`"version":"v1"`))

		By("publish v2 shard and update index to v2")
		_, err = ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      shardV2,
				Namespace: ctx.Namespace,
				Labels: map[string]string{
					"volcano.sh/job-id":            "job-002",
					"volcano.sh/ranktable-type":    "shard",
					"volcano.sh/ranktable-version": "v2",
				},
			},
			Data: map[string]string{aggregator.ShardDataKey: base64.StdEncoding.EncodeToString(v2Content)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		v2IndexYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ranktable_cur_version: "v2"
  ranktable_prev_version: "v1"
  status: "completed"
  protocol_version: "v1.0"
  encoding: "identity"
  chunk_size: "819200"
  total_shards: "1"
  compressed_size: "%d"
  original_size: "%d"
  compressed_sha256: "%s"
  content_sha256: "%s"
  changed_shards: "[0]"
  shards: |
    [{"id":0,"namespace":"%s","name":"%s","size":%d,"sha256":"%s"}]
`, indexName, ctx.Namespace, len(v2Content), len(v2Content), v2SHA, v2SHA, ctx.Namespace, shardV2, len(v2Content), v2SHA)
		indexCM.Data["index.yaml"] = v2IndexYAML
		_, err = ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Update(context.TODO(), indexCM, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("verify workload log eventually contains v2 content")
		Eventually(func() string {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "workload"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return strings.TrimSpace(string(b))
		}, 90*time.Second, 2*time.Second).Should(ContainSubstring(`"version":"v2"`))
	})

	It("corrupted shard keeps old file (TODO)", func() {
		Skip("TODO: publish bad sha shard and assert file is not switched")
	})

	It("invalid changed_shards fails reconcile", func() {
		const (
			indexName  = "rt-index-job-003"
			shardV1    = "rt-shard-job-003-v1-0"
			shardV2    = "rt-shard-job-003-v2-0"
			podName    = "rt-consumer-invalid-changed-shards"
			outputPath = "/etc/ranktable/jobstart_hccl.json"
			indexPath  = "/etc/ranktable/index/index.yaml"
		)
		v1Content := []byte(`{"rank":0,"version":"v1"}`)
		v2Content := []byte(`{"rank":0,"version":"v2"}`)
		v1SHA := aggregator.Sha256Hex(v1Content)
		v2SHA := aggregator.Sha256Hex(v2Content)

		By("create v1 shard and index")
		_, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      shardV1,
				Namespace: ctx.Namespace,
			},
			Data: map[string]string{
				aggregator.ShardDataKey: base64.StdEncoding.EncodeToString(v1Content),
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		v1IndexYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ranktable_cur_version: "v1"
  ranktable_prev_version: ""
  status: "completed"
  protocol_version: "v1.0"
  encoding: "identity"
  chunk_size: "819200"
  total_shards: "1"
  compressed_size: "%d"
  original_size: "%d"
  compressed_sha256: "%s"
  content_sha256: "%s"
  changed_shards: "[0]"
  shards: |
    [{"id":0,"namespace":"%s","name":"%s","size":%d,"sha256":"%s"}]
`, indexName, ctx.Namespace, len(v1Content), len(v1Content), v1SHA, v1SHA, ctx.Namespace, shardV1, len(v1Content), v1SHA)
		indexCM, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: indexName, Namespace: ctx.Namespace},
			Data:       map[string]string{"index.yaml": v1IndexYAML},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create minimal rbac for pod service account")
		_, err = ctx.Kubeclient.RbacV1().Roles(ctx.Namespace).Create(context.TODO(), &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator-v3", Namespace: ctx.Namespace},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctx.Kubeclient.RbacV1().RoleBindings(ctx.Namespace).Create(context.TODO(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator-v3", Namespace: ctx.Namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ranktable-aggregator-v3"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "default", Namespace: ctx.Namespace}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create pod with native sidecar + workload logger")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ctx.Namespace},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyAlways,
				ServiceAccountName: "default",
				Volumes: []corev1.Volume{
					{Name: "ranktable-index", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: indexName},
						Items:                []corev1.KeyToPath{{Key: "index.yaml", Path: "index.yaml"}},
					}}},
					{Name: "ranktable-shared", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
				InitContainers: []corev1.Container{
					{
						Name:          "ranktable-aggregator",
						RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
						Image:         aggImage,
						Command:       []string{aggCmd},
						Args: []string{
							"-index-file-path=" + indexPath,
							"-output-path=" + outputPath,
							"-poll-interval=2s",
							"-startup-jitter=0s",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-index", MountPath: "/etc/ranktable/index"},
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:    "workload",
						Image:   e2eutil.DefaultBusyBoxImage,
						Command: []string{"sh", "-c", "while true; do test -s " + outputPath + " && cat " + outputPath + " || true; sleep 2; done"},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
			},
		}
		_, err = ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2eutil.WaitPodReady(ctx, pod)).NotTo(HaveOccurred())

		By("verify workload sees v1 content first")
		Eventually(func() string {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "workload"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return string(b)
		}, 60*time.Second, 2*time.Second).Should(ContainSubstring(`"version":"v1"`))

		By("publish v2 shard and update index with malformed changed_shards")
		_, err = ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: shardV2, Namespace: ctx.Namespace},
			Data:       map[string]string{aggregator.ShardDataKey: base64.StdEncoding.EncodeToString(v2Content)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		badIndexYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ranktable_cur_version: "v2"
  ranktable_prev_version: "v1"
  status: "completed"
  protocol_version: "v1.0"
  encoding: "identity"
  chunk_size: "819200"
  total_shards: "1"
  compressed_size: "%d"
  original_size: "%d"
  compressed_sha256: "%s"
  content_sha256: "%s"
  changed_shards: "[0"   # malformed json on purpose
  shards: |
    [{"id":0,"namespace":"%s","name":"%s","size":%d,"sha256":"%s"}]
`, indexName, ctx.Namespace, len(v2Content), len(v2Content), v2SHA, v2SHA, ctx.Namespace, shardV2, len(v2Content), v2SHA)
		indexCM.Data["index.yaml"] = badIndexYAML
		_, err = ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Update(context.TODO(), indexCM, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("verify sidecar logs parse changed_shards error")
		Eventually(func() string {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "ranktable-aggregator"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return string(b)
		}, 90*time.Second, 2*time.Second).Should(ContainSubstring("parse changed_shards"))

		By("verify workload does not switch to v2 content")
		Consistently(func() bool {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "workload"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return false
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			s := string(b)
			return strings.Contains(s, `"version":"v1"`) && !strings.Contains(s, `"version":"v2"`)
		}, 20*time.Second, 2*time.Second).Should(BeTrue())
	})

	It("status=initializing does not switch", func() {
		const (
			indexName  = "rt-index-job-004"
			shardV1    = "rt-shard-job-004-v1-0"
			shardV2    = "rt-shard-job-004-v2-0"
			podName    = "rt-consumer-initializing"
			outputPath = "/etc/ranktable/jobstart_hccl.json"
			indexPath  = "/etc/ranktable/index/index.yaml"
		)
		v1Content := []byte(`{"rank":0,"version":"v1"}`)
		v2Content := []byte(`{"rank":0,"version":"v2"}`)
		v1SHA := aggregator.Sha256Hex(v1Content)
		v2SHA := aggregator.Sha256Hex(v2Content)

		By("create v1 shard and index")
		_, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: shardV1, Namespace: ctx.Namespace},
			Data:       map[string]string{aggregator.ShardDataKey: base64.StdEncoding.EncodeToString(v1Content)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		v1IndexYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ranktable_cur_version: "v1"
  ranktable_prev_version: ""
  status: "completed"
  protocol_version: "v1.0"
  encoding: "identity"
  chunk_size: "819200"
  total_shards: "1"
  compressed_size: "%d"
  original_size: "%d"
  compressed_sha256: "%s"
  content_sha256: "%s"
  changed_shards: "[0]"
  shards: |
    [{"id":0,"namespace":"%s","name":"%s","size":%d,"sha256":"%s"}]
`, indexName, ctx.Namespace, len(v1Content), len(v1Content), v1SHA, v1SHA, ctx.Namespace, shardV1, len(v1Content), v1SHA)
		indexCM, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: indexName, Namespace: ctx.Namespace},
			Data:       map[string]string{"index.yaml": v1IndexYAML},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create minimal rbac for pod service account")
		_, err = ctx.Kubeclient.RbacV1().Roles(ctx.Namespace).Create(context.TODO(), &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator-v4", Namespace: ctx.Namespace},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctx.Kubeclient.RbacV1().RoleBindings(ctx.Namespace).Create(context.TODO(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ranktable-aggregator-v4", Namespace: ctx.Namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ranktable-aggregator-v4"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "default", Namespace: ctx.Namespace}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create pod with native sidecar + workload logger")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ctx.Namespace},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyAlways,
				ServiceAccountName: "default",
				Volumes: []corev1.Volume{
					{Name: "ranktable-index", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: indexName},
						Items:                []corev1.KeyToPath{{Key: "index.yaml", Path: "index.yaml"}},
					}}},
					{Name: "ranktable-shared", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
				InitContainers: []corev1.Container{
					{
						Name:          "ranktable-aggregator",
						RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
						Image:         aggImage,
						Command:       []string{aggCmd},
						Args: []string{
							"-index-file-path=" + indexPath,
							"-output-path=" + outputPath,
							"-poll-interval=2s",
							"-startup-jitter=0s",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-index", MountPath: "/etc/ranktable/index"},
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:    "workload",
						Image:   e2eutil.DefaultBusyBoxImage,
						Command: []string{"sh", "-c", "while true; do test -s " + outputPath + " && cat " + outputPath + " || true; sleep 2; done"},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ranktable-shared", MountPath: "/etc/ranktable"},
						},
					},
				},
			},
		}
		_, err = ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2eutil.WaitPodReady(ctx, pod)).NotTo(HaveOccurred())

		By("verify workload sees v1 content first")
		Eventually(func() string {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "workload"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return string(b)
		}, 60*time.Second, 2*time.Second).Should(ContainSubstring(`"version":"v1"`))

		By("publish v2 shard and update index with status=initializing")
		_, err = ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: shardV2, Namespace: ctx.Namespace},
			Data:       map[string]string{aggregator.ShardDataKey: base64.StdEncoding.EncodeToString(v2Content)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		initIndexYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ranktable_cur_version: "v2"
  ranktable_prev_version: "v1"
  status: "initializing"
  protocol_version: "v1.0"
  encoding: "identity"
  chunk_size: "819200"
  total_shards: "1"
  compressed_size: "%d"
  original_size: "%d"
  compressed_sha256: "%s"
  content_sha256: "%s"
  changed_shards: "[0]"
  shards: |
    [{"id":0,"namespace":"%s","name":"%s","size":%d,"sha256":"%s"}]
`, indexName, ctx.Namespace, len(v2Content), len(v2Content), v2SHA, v2SHA, ctx.Namespace, shardV2, len(v2Content), v2SHA)
		indexCM.Data["index.yaml"] = initIndexYAML
		_, err = ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Update(context.TODO(), indexCM, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("verify sidecar logs index not completed error")
		Eventually(func() string {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "ranktable-aggregator"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return string(b)
		}, 90*time.Second, 2*time.Second).Should(ContainSubstring("index not completed"))

		By("verify workload does not switch to v2 content")
		Consistently(func() bool {
			req := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "workload"})
			rc, e := req.Stream(context.TODO())
			if e != nil {
				return false
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			s := string(b)
			return strings.Contains(s, `"version":"v1"`) && !strings.Contains(s, `"version":"v2"`)
		}, 20*time.Second, 2*time.Second).Should(BeTrue())
	})

	It("incremental reuse only fetches changed shards (TODO)", func() {
		Skip("TODO: compare API GET behavior between full and incremental update")
	})
})
