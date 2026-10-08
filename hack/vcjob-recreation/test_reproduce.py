# Copyright 2026 The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Offline tests. No Kubernetes access or real reproduction-rate claims."""

import contextlib
import copy
import io
import json
import pathlib
import tempfile
import unittest
from unittest import mock

import reproduce as r


def pod(name, uid, job_uid):
    return {"metadata": {"name": name, "uid": uid, "resourceVersion": "1", "finalizers": [],
                         "ownerReferences": [{"apiVersion": "batch.volcano.sh/v1alpha1", "kind": "Job", "controller": True, "uid": job_uid}]},
            "status": {"phase": "Running", "containerStatuses": [{"imageID": "sha256:workload"}]}}


class Clock:
    def __init__(self):
        self.now = 0.0

    def sleep(self, seconds):
        self.now += seconds


class FakeCluster:
    def __init__(self, failure=None, control_works=True, late=False):
        self.jobs, self.pods, self.generations = {}, {}, {}
        self.sequence = 0
        self.failure, self.control_works, self.late = failure, control_works, late
        self.stuck_uids = set()
        self.patches = []
        self.probe_deleted = False
        self.natural = False
        self.natural_gc_remaining = None
        self.namespace = None

    def uid(self):
        self.sequence += 1
        return "uid-" + str(self.sequence)

    def fill(self, job, count):
        name, uid = job["metadata"]["name"], job["metadata"]["uid"]
        for i in range(count):
            pod_name = name + "-worker-" + str(i)
            if pod_name not in self.pods:
                self.pods[pod_name] = pod(pod_name, self.uid(), uid)

    def create(self, obj):
        if obj["kind"] == "Namespace":
            self.namespace = copy.deepcopy(obj)
            self.namespace["metadata"]["uid"] = self.uid()
            return copy.deepcopy(self.namespace)
        job = copy.deepcopy(obj)
        name = job["metadata"]["name"]
        job["metadata"]["uid"] = self.uid()
        self.jobs[name] = job
        generation = self.generations.get(name, 0) + 1
        self.generations[name] = generation
        count = job["spec"]["tasks"][0]["replicas"]
        if generation % 2 == 0 and self.failure == "initial_recovery" and "-control-" not in name:
            count = 0
        if self.failure == "drained_race" and generation > 1 and "-control-" not in name and not self.pods:
            count = 0
            self.stuck_uids.add(job["metadata"]["uid"])
        if "-control-" in name and not self.control_works:
            count = 0
        self.fill(job, count)
        if self.natural and generation == 2:
            self.natural_gc_remaining = 1
        if "-control-" in name and self.late:
            self.failure = None
            target = self.jobs[name.split("-control-")[0]]
            self.fill(target, target["spec"]["tasks"][0]["replicas"])
        return copy.deepcopy(job)

    def get(self, resource, name=None, namespace=None, selector=None):
        if resource == "nodes" or resource in ("podgroups.scheduling.volcano.sh", "events"):
            return {"items": []}
        if resource == "namespace" and name == "kube-system":
            return {"metadata": {"uid": "offline-cluster"}}
        if resource == "namespace":
            return copy.deepcopy(self.namespace)
        if resource == "queues.scheduling.volcano.sh":
            return {"metadata": {"name": name}}
        if resource == "pods":
            if self.natural_gc_remaining == 0:
                self.pods = {name: p for name, p in self.pods.items() if not p["metadata"].get("deletionTimestamp")}
            elif self.natural_gc_remaining is not None:
                self.natural_gc_remaining -= 1
            # Once the held old Pod is released, a working controller fills names.
            for key, job in self.jobs.items():
                if ("-control-" not in key and job["metadata"]["uid"] not in self.stuck_uids
                        and self.failure != "initial_recovery" and not (self.failure == "post_cleanup_recovery" and self.probe_deleted)):
                    self.fill(job, job["spec"]["tasks"][0]["replicas"])
            return {"items": copy.deepcopy(list(self.pods.values()))}
        if resource == "pod":
            return copy.deepcopy(self.pods.get(name))
        if resource == r.JOB:
            return copy.deepcopy(self.jobs.get(name)) if name else {"items": copy.deepcopy(list(self.jobs.values()))}
        raise AssertionError(resource)

    def delete(self, resource, name, namespace=None, uid=None):
        if resource == "namespace":
            assert self.namespace["metadata"]["uid"] == uid
            self.namespace = None
            self.jobs.clear()
            self.pods.clear()
        elif resource == r.JOB:
            job = self.jobs.pop(name, None)
            if job:
                if uid:
                    assert job["metadata"]["uid"] == uid
                for key, p in list(self.pods.items()):
                    if r.owner_uid(p) == job["metadata"]["uid"]:
                        if p["metadata"]["finalizers"] or self.natural:
                            p["metadata"]["deletionTimestamp"] = "deleting"
                        else:
                            del self.pods[key]
        elif resource == "pod":
            self.probe_deleted = True
            target = self.pods.pop(name)
            assert target["metadata"]["uid"] == uid
            for job in self.jobs.values():
                if job["metadata"]["uid"] == r.owner_uid(target) and self.failure != "post_cleanup_recovery":
                    self.fill(job, job["spec"]["tasks"][0]["replicas"])
        else:
            raise AssertionError(resource)

    def call(self, args, data=None, namespace=None):
        if args[:1] == ["version"]:
            return json.dumps({"serverVersion": {"gitVersion": "offline-fixture"}})
        assert args[:2] == ["patch", "pod"]
        p = self.pods[args[2]]
        patch = json.loads(args[-1])
        self.patches.append(patch)
        assert patch[0]["value"] == p["metadata"]["uid"]
        assert patch[1]["value"] == p["metadata"]["resourceVersion"]
        p["metadata"]["finalizers"] = patch[2]["value"]
        p["metadata"]["resourceVersion"] = str(int(p["metadata"]["resourceVersion"]) + 1)
        if p["metadata"].get("deletionTimestamp") and not p["metadata"]["finalizers"]:
            del self.pods[args[2]]
        return ""


class ExperimentTests(unittest.TestCase):
    def setUp(self):
        self.args = r.parser().parse_args(["run", "--context", "fake", "--label", "offline", "--output", "unused",
                                          "--replicas", "2", "--hold-seconds", "0", "--stability-seconds", "0",
                                          "--startup-timeout", "0.3", "--recovery-timeout", "0.2", "--poll-seconds", "0.1"])
        self.clock = Clock()
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(mock.patch.object(r.time, "monotonic", side_effect=lambda: self.clock.now))
        self.stack.enter_context(mock.patch.object(r.time, "sleep", side_effect=self.clock.sleep))

    def experiment(self, **kwargs):
        cluster = FakeCluster(**kwargs)
        experiment = r.Experiment(self.args, cluster)
        experiment.initial_controller = {"stable": True}
        experiment.controller = mock.Mock(return_value={"stable": True})
        return experiment, cluster

    def test_healthy_recreation_and_subsequent_reconciliation(self):
        experiment, cluster = self.experiment()
        row = {"recreate_delay_seconds": 0}
        experiment.trial(1, row)
        self.assertTrue(row["overlap_confirmed"])
        self.assertNotEqual(row["old_uid"], row["new_uid"])
        self.assertEqual(row["observed_replicas"], 2)
        self.assertIn("post_cleanup_recovery_seconds", row)
        self.assertFalse(experiment.held)
        experiment.cleanup_trial(row["job_name"])
        self.assertFalse(cluster.pods)
        self.assertFalse(cluster.jobs)

    def test_reuses_job_name_and_successor_across_both_windows(self):
        experiment, cluster = self.experiment()
        first = {"recreate_delay_seconds": 0}
        first_uid = experiment.trial(1, first)
        second = {"recreate_delay_seconds": 0}
        second_uid = experiment.trial(2, second, first_uid)
        self.assertEqual(first["job_name"], second["job_name"])
        self.assertEqual((first["window"], second["window"]), ("overlap", "drained"))
        self.assertEqual(second["old_uid"], first["new_uid"])
        self.assertEqual(second["old_uid"], first_uid)
        self.assertNotEqual(second_uid, first_uid)
        self.assertFalse(second["overlap_confirmed"])
        self.assertLessEqual(second["old_pods_gone_at"], r.utcnow())
        self.assertEqual(len(cluster.patches), 2)
        experiment.cleanup_trial(second["job_name"])
        self.assertFalse(cluster.jobs)
        self.assertFalse(cluster.pods)

    def test_drained_window_can_detect_failure_missed_by_overlap(self):
        experiment, cluster = self.experiment(failure="drained_race")
        first = {"recreate_delay_seconds": 0}
        first_uid = experiment.trial(1, first)
        self.assertEqual(first["window"], "overlap")
        second = {"recreate_delay_seconds": 0, "iteration": 2}
        with self.assertRaisesRegex(r.Reproduced, "initial_recovery"):
            experiment.trial(2, second, first_uid)
        self.assertEqual(second["window"], "drained")
        self.assertEqual(second["old_uid"], first_uid)
        self.assertEqual(second["observed_replicas"], 0)
        experiment.cleanup_trial(second["job_name"], second["control_name"])
        self.assertFalse(cluster.jobs)
        self.assertFalse(cluster.pods)

    def test_natural_mode_confirms_overlap_without_finalizer(self):
        experiment, cluster = self.experiment()
        self.args.mode = "natural"
        cluster.natural = True
        row = {"recreate_delay_seconds": 0}
        experiment.trial(1, row)
        self.assertTrue(row["overlap_confirmed"])
        self.assertFalse(cluster.patches)
        self.assertEqual(row["observed_replicas"], 2)

    def test_initial_missing_replicas_with_healthy_control_reproduces(self):
        experiment, _ = self.experiment(failure="initial_recovery")
        row = {"recreate_delay_seconds": 0}
        with self.assertRaisesRegex(r.Reproduced, "initial_recovery"):
            experiment.trial(1, row)
        self.assertEqual(row["observed_replicas"], 0)
        self.assertIn("old_pods_gone_at", row)
        self.assertIn("control_uid", row)

    def test_loss_of_reconciliation_detected_after_initial_recovery(self):
        experiment, _ = self.experiment(failure="post_cleanup_recovery")
        row = {"recreate_delay_seconds": 0}
        with self.assertRaisesRegex(r.Reproduced, "post_cleanup_recovery"):
            experiment.trial(1, row)
        self.assertEqual(row["observed_replicas"], 1)

    def test_cluster_creation_failure_is_not_counted_as_reproduction(self):
        experiment, _ = self.experiment(failure="initial_recovery", control_works=False)
        with self.assertRaises(r.DeadlineExceeded):
            experiment.trial(1, {"recreate_delay_seconds": 0})

    def test_late_recovery_is_inconclusive(self):
        experiment, _ = self.experiment(failure="initial_recovery", late=True)
        with self.assertRaisesRegex(r.TestError, "after observation deadline") as caught:
            experiment.trial(1, {"recreate_delay_seconds": 0})
        self.assertNotIsInstance(caught.exception, r.Reproduced)

    def test_controller_restart_invalidates_trial(self):
        experiment, _ = self.experiment()
        experiment.controller.side_effect = [{"stable": True}, {"stable": False}]
        with self.assertRaisesRegex(r.TestError, "changed/restarted"):
            experiment.trial(1, {"recreate_delay_seconds": 0})

    def test_release_preserves_other_finalizers_and_tests_uid(self):
        experiment, cluster = self.experiment()
        p = pod("held", "pod-uid", "job-uid")
        p["metadata"]["finalizers"] = ["some.other/finalizer"]
        cluster.pods["held"] = p
        experiment.set_hold(copy.deepcopy(p), True)
        experiment.release_all()
        self.assertEqual(cluster.pods["held"]["metadata"]["finalizers"], ["some.other/finalizer"])
        self.assertEqual(cluster.patches[-1][0], {"op": "test", "path": "/metadata/uid", "value": "pod-uid"})

    def test_release_never_patches_replacement_pod(self):
        experiment, cluster = self.experiment()
        old = pod("same", "old", "job")
        experiment.held["same"] = old
        cluster.pods["same"] = pod("same", "new", "job")
        experiment.release_all()
        self.assertFalse(cluster.patches)

    def test_api_error_propagates_without_becoming_timeout(self):
        experiment, _ = self.experiment()
        experiment.recovered = mock.Mock(side_effect=r.TestError("Forbidden"))
        with self.assertRaisesRegex(r.TestError, "Forbidden") as caught:
            experiment.await_recovery("job", "uid", {}, "initial_recovery")
        self.assertNotIsInstance(caught.exception, r.Reproduced)


    def test_full_100_trial_runs_write_reports_and_cleanup(self):
        for failure, expected_code, expected_count in (("initial_recovery", 1, 100), (None, 0, 0)):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                cluster = FakeCluster(failure=failure)
                self.args.output = str(pathlib.Path(directory) / "results")
                self.args.iterations = 100
                with mock.patch.object(r, "Kubectl", return_value=cluster), mock.patch.object(r.Experiment, "controller", return_value={"stable": True}), contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(r.run(self.args), expected_code)
                summary = json.loads((pathlib.Path(self.args.output) / "summary.json").read_text())
                self.assertEqual(summary["attempted"], 100)
                self.assertEqual(summary["reproduced"], expected_count)
                self.assertEqual(summary["inconclusive"], 0)
                self.assertEqual(len((pathlib.Path(self.args.output) / "results.jsonl").read_text().splitlines()), 100)
                rows = [json.loads(line) for line in (pathlib.Path(self.args.output) / "results.jsonl").read_text().splitlines()]
                self.assertEqual([rows[0]["window"], rows[1]["window"]], ["overlap", "drained"])
                self.assertGreaterEqual(rows[1]["recreate_delay_seconds"], self.args.drained_settle_seconds)
                if failure is None:
                    self.assertEqual(rows[1]["old_uid"], rows[0]["new_uid"])
                self.assertIsNone(cluster.namespace)
                self.assertFalse(cluster.pods)

    def test_interrupt_writes_partial_report_and_releases_held_pod(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = FakeCluster()
            self.args.output = str(pathlib.Path(directory) / "interrupted")
            def interrupt(experiment, index, row, prior_uid):
                job = cluster.create(experiment.job("interrupted"))
                p = r.live_pods(experiment.pods(), job["metadata"]["uid"])[0]
                experiment.set_hold(p, True)
                raise KeyboardInterrupt()
            with mock.patch.object(r, "Kubectl", return_value=cluster), mock.patch.object(r.Experiment, "controller", return_value={"stable": True}), mock.patch.object(r.Experiment, "trial", interrupt), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(r.run(self.args), 2)
            result = json.loads((pathlib.Path(self.args.output) / "summary.json").read_text())
            self.assertEqual(result["inconclusive"], 1)
            self.assertEqual(result["not_attempted"], 99)
            self.assertIsNone(cluster.namespace)
            self.assertFalse(cluster.pods)



class LaunchTests(unittest.TestCase):
    def test_no_arguments_starts_default_100_round_run(self):
        with mock.patch.object(r.sys, "argv", ["reproduce.py"]), mock.patch.object(r, "run", return_value=0) as run, mock.patch.object(r.signal, "signal"):
            self.assertEqual(r.main(), 0)
        args = run.call_args.args[0]
        self.assertEqual(args.iterations, 100)
        self.assertEqual(args.replicas, 8)
        self.assertEqual(args.label, "current")
        self.assertIsNone(args.context)
        self.assertIsNone(args.output)

    def test_current_context_resolved_once_and_output_generated(self):
        args = r.parser().parse_args(["run"])
        with mock.patch.object(r.subprocess, "run", return_value=mock.Mock(returncode=0, stdout="local-cluster\n", stderr="")) as command:
            r.prepare_run(args)
            output = args.output
            r.prepare_run(args)
        self.assertEqual(args.context, "local-cluster")
        self.assertEqual(args.output, output)
        self.assertEqual(pathlib.Path(output).parent, pathlib.Path("_artifacts/vcjob-recreation"))
        command.assert_called_once_with(["kubectl", "config", "current-context"], text=True, capture_output=True, timeout=10)

    def test_explicit_options_skip_current_context_lookup(self):
        args = r.parser().parse_args(["run", "--context", "selected", "--label", "before", "--output", "/tmp/chosen", "--iterations", "3"])
        with mock.patch.object(r.subprocess, "run") as command:
            r.prepare_run(args)
        command.assert_not_called()
        self.assertEqual((args.context, args.label, args.output, args.iterations), ("selected", "before", "/tmp/chosen", 3))

    def test_no_current_context_stops_before_cluster_access(self):
        args = r.parser().parse_args(["run"])
        with mock.patch.object(r.subprocess, "run", return_value=mock.Mock(returncode=1, stdout="", stderr="current-context is not set")), mock.patch.object(r, "Kubectl") as kube:
            with self.assertRaisesRegex(r.TestError, "current-context is not set"):
                r.run(args)
        kube.assert_not_called()
        self.assertIsNone(args.output)

    def test_help_does_not_read_kubeconfig_or_start_tests(self):
        with mock.patch.object(r.sys, "argv", ["reproduce.py", "--help"]), mock.patch.object(r.subprocess, "run") as command, mock.patch.object(r, "run") as run, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit) as result:
                r.main()
        self.assertEqual(result.exception.code, 0)
        command.assert_not_called()
        run.assert_not_called()


class ReportingTests(unittest.TestCase):
    def test_counts_use_only_live_pods_of_current_job(self):
        current = pod("a", "p1", "new")
        stale = pod("b", "p2", "old")
        terminating = pod("c", "p3", "new")
        terminating["metadata"]["deletionTimestamp"] = "now"
        pending = pod("d", "p4", "new")
        pending["status"]["phase"] = "Pending"
        self.assertEqual(r.live_pods([current, stale, terminating, pending], "new"), [current, pending])
        stale["metadata"]["ownerReferences"][0]["apiVersion"] = "batch/v1"
        self.assertIsNone(r.owner_uid(stale))

    def test_inconclusive_is_excluded_and_unattempted_reported(self):
        rows = [{"outcome": "passed"}] * 70 + [{"outcome": "reproduced"}] * 10 + [{"outcome": "inconclusive"}] * 5
        result = r.summarize(rows, 100)
        self.assertEqual(result["valid"], 80)
        self.assertEqual(result["not_attempted"], 15)
        self.assertEqual(result["observed_reproduction_rate"], 0.125)

    def test_window_counts_report_reproduction_separately(self):
        rows = [{"window": "overlap", "outcome": "passed"},
                {"window": "drained", "outcome": "reproduced"},
                {"window": "drained", "outcome": "inconclusive"}]
        result = r.summarize(rows, 4)
        self.assertEqual(result["by_window"]["overlap"]["observed_reproduction_rate"], 0)
        self.assertEqual(result["by_window"]["drained"]["observed_reproduction_rate"], 1)
        self.assertEqual(result["by_window"]["drained"]["inconclusive"], 1)

    def test_zero_failures_bound_and_empty_sample(self):
        result = r.summarize([{"outcome": "passed"}] * 100, 100)
        self.assertAlmostEqual(result["zero_failure_upper_95"], 0.0295130496)
        self.assertIsNone(r.summarize([], 100)["observed_reproduction_rate"])

    def fixtures(self, root, before_failures=20, after_failures=0):
        for label, failures in (("before", before_failures), ("after", after_failures)):
            folder = root / label
            folder.mkdir()
            r.save(folder / "metadata.json", {"protocol_version": r.PROTOCOL, "script_sha256": "fixture", "finished_at": "offline", "label": label, "parameters": {"iterations": 100},
                "controller": {"replicas": 1, "containers": [{"image": label, "args": []}],
                               "pods": [{"containers": [{"imageID": label}]}]}})
            summary = r.summarize([{"outcome": "passed"}] * (100 - failures) + [{"outcome": "reproduced"}] * failures, 100)
            summary["errors"] = []
            r.save(folder / "summary.json", summary)
        return r.parser().parse_args(["compare", "--before", str(root / "before"), "--after", str(root / "after")])

    def test_compare_valid_synthetic_samples(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()) as output:
            args = self.fixtures(pathlib.Path(directory))
            self.assertEqual(r.compare(args), 0)
            self.assertIn("20.00% (20/100)", output.getvalue())
            self.assertIn("2.95%", output.getvalue())

    def test_compare_without_baseline_reproduction_cannot_prove_fix(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            args = self.fixtures(pathlib.Path(directory), before_failures=0)
            self.assertEqual(r.compare(args), 2)

    def test_compare_rejects_protocol_mismatch_and_identical_images(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            args = self.fixtures(root)
            path = root / "after" / "metadata.json"
            metadata = json.loads(path.read_text())
            metadata["parameters"]["iterations"] = 50
            r.save(path, metadata)
            with self.assertRaisesRegex(r.TestError, "parameters differ"):
                r.compare(args)
            metadata["parameters"]["iterations"] = 100
            metadata["controller"]["pods"][0]["containers"][0]["imageID"] = "before"
            r.save(path, metadata)
            with self.assertRaisesRegex(r.TestError, "identical"):
                r.compare(args)

    def test_compare_cannot_conclude_when_controller_flags_changed(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()) as output:
            root = pathlib.Path(directory)
            args = self.fixtures(root)
            path = root / "after" / "metadata.json"
            metadata = json.loads(path.read_text())
            metadata["controller"]["containers"][0]["args"] = ["--max-requeue-num=1"]
            r.save(path, metadata)
            self.assertEqual(r.compare(args), 2)
            self.assertIn("INCOMPARABLE", output.getvalue())
            self.assertNotIn("no failures were observed", output.getvalue())

    def test_compare_rejects_unfinished_run(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            args = self.fixtures(root)
            path = root / "after" / "metadata.json"
            metadata = json.loads(path.read_text())
            del metadata["finished_at"]
            r.save(path, metadata)
            with self.assertRaisesRegex(r.TestError, "not finalized"):
                r.compare(args)

    def test_kubectl_delete_uses_context_and_uid_precondition(self):
        kube = r.Kubectl("explicit-context", 7)
        with mock.patch.object(r.subprocess, "run", return_value=mock.Mock(returncode=0, stdout="", stderr="")) as command:
            kube.delete("pod", "victim", "test-ns", uid="expected-uid")
        argv = command.call_args.args[0]
        self.assertEqual(argv[1:3], ["--context", "explicit-context"])
        self.assertIn("/api/v1/namespaces/test-ns/pods/victim", argv)
        self.assertEqual(json.loads(command.call_args.kwargs["input"])["preconditions"], {"uid": "expected-uid"})
        self.assertEqual(command.call_args.kwargs["timeout"], 12)

    def test_cleanup_refuses_replaced_namespace(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "metadata.json"
            r.save(path, {"context": "ctx", "namespace": "ns", "namespace_uid": "old", "run_id": "run"})
            args = r.parser().parse_args(["cleanup", "--context", "ctx", "--metadata", str(path)])
            with mock.patch.object(r.Kubectl, "get", return_value={"metadata": {"uid": "new", "labels": {r.LABEL: "run"}}}):
                with self.assertRaisesRegex(r.TestError, "identity"):
                    r.cleanup(args)


if __name__ == "__main__":
    unittest.main()
