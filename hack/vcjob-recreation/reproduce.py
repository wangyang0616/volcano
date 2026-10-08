#!/usr/bin/env python3
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
"""Measure same-name VCJob recreation failures using Python 3.9+ and kubectl."""

import argparse
import csv
import datetime
import hashlib
import json
import math
import pathlib
import random
import signal
import subprocess
import sys
import time
import uuid
from urllib.parse import quote

JOB = "jobs.batch.volcano.sh"
LABEL = "recreation-test.volcano.sh/run"
FINALIZER = "recreation-test.volcano.sh/hold"
PROTOCOL = 2


class TestError(Exception):
    pass


class Reproduced(TestError):
    pass


class DeadlineExceeded(TestError):
    pass


def utcnow():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def save(path, value):
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")
    temporary.replace(path)


def owner_uid(pod):
    for owner in pod.get("metadata", {}).get("ownerReferences", []):
        if owner.get("controller") and owner.get("kind") == "Job" and owner.get("apiVersion", "").startswith("batch.volcano.sh/"):
            return owner["uid"]
    return None


def live_pods(pods, uid):
    return [p for p in pods if owner_uid(p) == uid and not p["metadata"].get("deletionTimestamp")]


def running(pods, replicas):
    return len(pods) == replicas and all(p.get("status", {}).get("phase") == "Running" for p in pods)


def window_for(index):
    return "overlap" if index % 2 else "drained"


def summarize(rows, requested):
    counts = {status: sum(row["outcome"] == status for row in rows)
              for status in ("passed", "reproduced", "inconclusive")}
    valid = counts["passed"] + counts["reproduced"]
    result = {"requested": requested, "attempted": len(rows), "not_attempted": requested - len(rows),
              **counts, "valid": valid, "observed_reproduction_rate": None,
              "wilson_95_interval": None, "zero_failure_upper_95": None}
    if valid:
        p, z = counts["reproduced"] / valid, 1.959963984540054
        den = 1 + z * z / valid
        center = (p + z * z / (2 * valid)) / den
        width = z * math.sqrt(p * (1 - p) / valid + z * z / (4 * valid * valid)) / den
        result.update(observed_reproduction_rate=p,
                      wilson_95_interval=[max(0, center - width), min(1, center + width)])
        if not counts["reproduced"]:
            result["zero_failure_upper_95"] = 1 - math.pow(0.05, 1 / valid)
    result["by_window"] = {}
    for window in ("overlap", "drained"):
        group = [row for row in rows if row.get("window") == window]
        passed = sum(row["outcome"] == "passed" for row in group)
        reproduced = sum(row["outcome"] == "reproduced" for row in group)
        result["by_window"][window] = {
            "attempted": len(group), "passed": passed, "reproduced": reproduced,
            "inconclusive": len(group) - passed - reproduced,
            "observed_reproduction_rate": reproduced / (passed + reproduced) if passed + reproduced else None,
        }
    return result


class Kubectl:
    def __init__(self, context, timeout):
        self.context, self.timeout = context, timeout

    def call(self, args, data=None, namespace=None):
        command = ["kubectl", "--context", self.context, "--request-timeout", str(self.timeout) + "s"]
        if namespace:
            command += ["--namespace", namespace]
        command += args
        try:
            result = subprocess.run(command, input=json.dumps(data) if data is not None else None,
                                    text=True, capture_output=True, timeout=self.timeout + 5)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise TestError(str(error)) from error
        if result.returncode:
            raise TestError(result.stderr.strip() or result.stdout.strip())
        return result.stdout

    def get(self, resource, name=None, namespace=None, selector=None):
        args = ["get", resource]
        if name:
            args += [name, "--ignore-not-found"]
        if selector:
            args += ["--selector", selector]
        args += ["-o", "json"]
        text = self.call(args, namespace=namespace)
        return json.loads(text) if text.strip() else None

    def create(self, obj):
        return json.loads(self.call(["create", "-f", "-", "-o", "json"], obj))

    def delete(self, resource, name, namespace=None, uid=None):
        if uid:
            prefix = "/apis/batch.volcano.sh/v1alpha1" if resource == JOB else "/api/v1"
            plural = {JOB: "jobs", "pod": "pods", "namespace": "namespaces"}[resource]
            path = prefix + ("/namespaces/" + quote(namespace, safe="") if namespace else "")
            path += "/" + plural + "/" + quote(name, safe="")
            self.call(["delete", "--raw", path, "-f", "-"],
                      {"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Background", "preconditions": {"uid": uid}})
        else:
            self.call(["delete", resource, name, "--ignore-not-found", "--wait=false", "--cascade=background"], namespace=namespace)


def wait_for(check, timeout, poll, description):
    deadline = time.monotonic() + timeout
    while True:
        result = check()
        if result:
            return result
        if time.monotonic() >= deadline:
            raise DeadlineExceeded("timeout: " + description)
        time.sleep(min(poll, max(0, deadline - time.monotonic())))


class Experiment:
    def __init__(self, args, kube):
        self.args, self.kube = args, kube
        self.run_id = uuid.uuid4().hex[:12]
        self.namespace = "vcjob-recreate-" + self.run_id
        self.namespace_uid = None
        self.held = {}
        self.selector = None
        self.initial_controller = None

    def controller(self):
        a = self.args
        deployment = self.kube.get("deployment", a.controller_deployment, a.controller_namespace)
        if not deployment:
            raise TestError("controller Deployment not found")
        desired = deployment["spec"].get("replicas", 1)
        status = deployment.get("status", {})
        if (desired < 1 or status.get("readyReplicas", 0) != desired
                or status.get("updatedReplicas", 0) != desired
                or status.get("observedGeneration", 0) < deployment["metadata"]["generation"]):
            raise TestError("controller Deployment is not fully ready")
        selector = deployment["spec"]["selector"]
        if selector.get("matchExpressions") or not selector.get("matchLabels"):
            raise TestError("controller selector must use matchLabels")
        self.selector = ",".join(k + "=" + v for k, v in sorted(selector["matchLabels"].items()))
        pods = self.kube.get("pods", namespace=a.controller_namespace, selector=self.selector)["items"]
        pods = [p for p in pods if not p["metadata"].get("deletionTimestamp")]
        if len(pods) != desired:
            raise TestError("controller rollout is still in progress")
        records = []
        for pod in pods:
            statuses = pod.get("status", {}).get("containerStatuses", [])
            if not statuses or not all(c.get("ready") and c.get("imageID") for c in statuses):
                raise TestError("controller Pod is not ready or has no imageID")
            records.append({"name": pod["metadata"]["name"], "uid": pod["metadata"]["uid"],
                            "containers": [{k: c.get(k) for k in ("name", "image", "imageID", "restartCount")}
                                           for c in sorted(statuses, key=lambda c: c["name"])]})
        spec = deployment["spec"]["template"]["spec"]
        return {"deployment_uid": deployment["metadata"]["uid"], "generation": deployment["metadata"]["generation"],
                "replicas": desired, "containers": [{k: c.get(k) for k in ("name", "image", "command", "args", "resources")}
                                                     for c in spec["containers"]],
                "pods": sorted(records, key=lambda p: p["name"])}

    def unchanged_controller(self):
        if self.controller() != self.initial_controller:
            raise TestError("controller changed/restarted during the experiment; stop and rerun")

    def pods(self):
        return self.kube.get("pods", namespace=self.namespace)["items"]

    def job(self, name):
        a = self.args
        container = {
            "name": "worker", "image": a.image, "imagePullPolicy": "IfNotPresent",
            "command": ["sh", "-c", "trap '' TERM; while :; do sleep 1; done"],
            "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}},
        }
        task = {
            "name": "worker", "replicas": a.replicas,
            "template": {
                "metadata": {"labels": {LABEL: self.run_id}},
                "spec": {"terminationGracePeriodSeconds": a.grace_seconds,
                         "restartPolicy": "Never", "containers": [container]},
            },
        }
        return {
            "apiVersion": "batch.volcano.sh/v1alpha1", "kind": "Job",
            "metadata": {"name": name, "namespace": self.namespace, "labels": {LABEL: self.run_id}},
            "spec": {"schedulerName": a.scheduler, "queue": a.queue, "minAvailable": 1,
                     "policies": [{"event": "PodEvicted", "action": "RestartJob"}], "tasks": [task]},
        }

    def set_hold(self, pod, add):
        name, uid = pod["metadata"]["name"], pod["metadata"]["uid"]
        for attempt in range(5):
            current = self.kube.get("pod", name, self.namespace)
            if not current or current["metadata"]["uid"] != uid:
                if add:
                    raise TestError("hold target was replaced")
                self.held.pop(name, None)
                return
            meta = current["metadata"]
            finalizers = list(meta.get("finalizers", []))
            if add and FINALIZER not in finalizers:
                finalizers.append(FINALIZER)
            if not add:
                finalizers = [f for f in finalizers if f != FINALIZER]
            patch = [{"op": "test", "path": "/metadata/uid", "value": uid},
                     {"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
                     {"op": "add", "path": "/metadata/finalizers", "value": finalizers}]
            # Track before sending: the API may apply a patch even if its response times out.
            if add:
                self.held[name] = pod
            try:
                self.kube.call(["patch", "pod", name, "--type=json", "-p", json.dumps(patch)], namespace=self.namespace)
                if not add:
                    self.held.pop(name, None)
                return
            except TestError:
                if attempt == 4:
                    raise
                time.sleep(self.args.poll_seconds)

    def release_all(self):
        for pod in list(self.held.values()):
            self.set_hold(pod, False)

    def verify_job(self, name, uid):
        job = self.kube.get(JOB, name, self.namespace)
        if not job or job["metadata"]["uid"] != uid or job["metadata"].get("deletionTimestamp"):
            raise TestError("new Job disappeared or was externally replaced")
        return job

    def snapshot(self, directory, since):
        directory.mkdir(exist_ok=True)
        for resource in (JOB, "pods", "podgroups.scheduling.volcano.sh", "events"):
            try:
                save(directory / (resource + ".json"), self.kube.get(resource, namespace=self.namespace))
            except TestError as error:
                (directory / (resource + ".error.txt")).write_text(str(error))
        if self.selector:
            try:
                pods = self.kube.get("pods", namespace=self.args.controller_namespace, selector=self.selector)["items"]
                for pod in pods:
                    name = pod["metadata"]["name"]
                    try:
                        logs = self.kube.call(["logs", name, "--all-containers=true", "--timestamps=true", "--since-time=" + since, "--tail=3000"], namespace=self.args.controller_namespace)
                    except TestError as error:
                        logs = str(error)
                    (directory / (name + ".log")).write_text(logs)
            except TestError as error:
                (directory / "controller.error.txt").write_text(str(error))

    def recovered(self, name, uid, old_pod_uid=None):
        self.verify_job(name, uid)
        pods = live_pods(self.pods(), uid)
        return len(pods) == self.args.replicas and all(p["metadata"]["uid"] != old_pod_uid for p in pods)

    def await_recovery(self, name, uid, row, stage, old_pod_uid=None):
        start = time.monotonic()
        row["stage"] = stage
        try:
            wait_for(lambda: self.recovered(name, uid, old_pod_uid), self.args.recovery_timeout,
                     self.args.poll_seconds, stage)
        except DeadlineExceeded:
            self.unchanged_controller()
            # A fresh Job must still create Pods under the same queue and admission rules.
            control_name = "%s-control-%04d" % (name, row.get("iteration", 0))
            row["control_name"] = control_name
            control = self.kube.create(self.job(control_name))
            control_uid = control["metadata"]["uid"]
            row["control_uid"] = control_uid
            wait_for(lambda: len(live_pods(self.pods(), control_uid)) == self.args.replicas,
                     self.args.startup_timeout, self.args.poll_seconds, "fresh control Job cannot create Pods")
            self.unchanged_controller()
            if self.recovered(name, uid, old_pod_uid):
                raise TestError("recovered after observation deadline; increase recovery-timeout for both runs")
            job = self.verify_job(name, uid)
            row["job_status"] = job.get("status", {})
            row["observed_replicas"] = len(live_pods(self.pods(), uid))
            raise Reproduced(stage + ": missing replacement Pods after deadline, while fresh control Job creates Pods")
        row[stage + "_seconds"] = round(time.monotonic() - start, 3)

    def trial(self, index, row, prior_uid=None):
        a = self.args
        name = "recreate"
        window = window_for(index)
        row.update(job_name=name, namespace=self.namespace, window=window, stage="setup")
        self.unchanged_controller()
        old = self.verify_job(name, prior_uid) if prior_uid else self.kube.create(self.job(name))
        old_uid = old["metadata"]["uid"]
        row["old_uid"] = old_uid
        wait_for(lambda: running(live_pods(self.pods(), old_uid), a.replicas), a.startup_timeout, a.poll_seconds, "old Pods Running")
        old_pods = live_pods(self.pods(), old_uid)
        row["old_pods"] = [{"name": p["metadata"]["name"], "uid": p["metadata"]["uid"]} for p in old_pods]
        row["workload_image_ids"] = sorted({c.get("imageID", "") for p in old_pods for c in p.get("status", {}).get("containerStatuses", [])})
        victim = sorted(old_pods, key=lambda p: p["metadata"]["name"])[0]
        if window == "overlap" and a.mode == "finalizer":
            self.set_hold(victim, True)
        row["stage"] = window
        self.kube.delete(JOB, name, self.namespace, uid=old_uid)
        deleted_at = time.monotonic()
        wait_for(lambda: self.kube.get(JOB, name, self.namespace) is None, a.startup_timeout, a.poll_seconds, "old Job deleted")
        if window == "overlap":
            wait_for(lambda: any(owner_uid(p) == old_uid and p["metadata"].get("deletionTimestamp") for p in self.pods()),
                     a.startup_timeout, a.poll_seconds, "old Pods Terminating")
        else:
            wait_for(lambda: not any(owner_uid(p) == old_uid for p in self.pods()),
                     a.startup_timeout, a.poll_seconds, "all old Pods actually deleted")
            row["old_pods_gone_at"] = utcnow()
            old_pods_gone = time.monotonic()
        delay = row["recreate_delay_seconds"]
        time.sleep(delay)
        if window == "overlap" and not any(owner_uid(p) == old_uid for p in self.pods()):
            raise TestError("no old Pods remain: required overlap was not exercised")
        new = self.kube.create(self.job(name))
        row["delete_to_create_seconds"] = round(time.monotonic() - deleted_at, 3)
        if window == "drained":
            row["old_pods_gone_to_create_seconds"] = round(time.monotonic() - old_pods_gone, 3)
        uid = new["metadata"]["uid"]
        row["new_uid"] = uid
        if uid == old_uid:
            raise TestError("recreated Job has the same UID")
        if window == "overlap" and not any(owner_uid(p) == old_uid for p in self.pods()):
            raise TestError("old Pods disappeared before overlap could be confirmed")
        row["overlap_confirmed"] = window == "overlap"
        if window == "overlap" and a.mode == "finalizer":
            time.sleep(a.hold_seconds)
            held = self.kube.get("pod", victim["metadata"]["name"], self.namespace)
            if not held or held["metadata"]["uid"] != victim["metadata"]["uid"] or FINALIZER not in held["metadata"].get("finalizers", []):
                raise TestError("held Pod changed before release")
            self.release_all()
        if window == "overlap":
            wait_for(lambda: not any(owner_uid(p) == old_uid for p in self.pods()), a.startup_timeout, a.poll_seconds, "all old Pods actually deleted")
            row["old_pods_gone_at"] = utcnow()
        self.await_recovery(name, uid, row, "initial_recovery")
        deadline = time.monotonic() + a.stability_seconds
        while time.monotonic() < deadline:
            if not self.recovered(name, uid):
                raise TestError("replicas changed during stability window; inspect evidence")
            time.sleep(a.poll_seconds)
        # Existing replicas alone cannot show whether delayed cleanup removed the cache.
        victim = sorted(live_pods(self.pods(), uid), key=lambda p: p["metadata"]["name"])[0]
        row["probe_pod_uid"] = victim["metadata"]["uid"]
        self.kube.delete("pod", victim["metadata"]["name"], self.namespace, uid=row["probe_pod_uid"])
        self.await_recovery(name, uid, row, "post_cleanup_recovery", row["probe_pod_uid"])
        wait_for(lambda: running(live_pods(self.pods(), uid), a.replicas),
                 a.startup_timeout, a.poll_seconds, "new Pods Running before next lifecycle")
        self.unchanged_controller()
        row["observed_replicas"] = len(live_pods(self.pods(), uid))
        return uid

    def cleanup_trial(self, name, control_name=None):
        self.release_all()
        for job_name in (name, control_name):
            if not job_name:
                continue
            self.kube.delete(JOB, job_name, self.namespace)
        wait_for(lambda: not self.pods(), self.args.startup_timeout, self.args.poll_seconds, "trial Pods cleaned up")
        wait_for(lambda: not self.kube.get(JOB, namespace=self.namespace)["items"], self.args.startup_timeout, self.args.poll_seconds, "trial Jobs cleaned up")

    def cleanup_namespace(self):
        ns = self.kube.get("namespace", self.namespace)
        if not ns:
            return
        if ns["metadata"]["uid"] != self.namespace_uid or ns["metadata"].get("labels", {}).get(LABEL) != self.run_id:
            raise TestError("namespace identity changed; refusing cleanup")
        self.release_all()
        self.kube.delete("namespace", self.namespace, uid=self.namespace_uid)


def protocol(args):
    return {key: getattr(args, key) for key in ("iterations", "replicas", "image", "scheduler", "queue", "mode",
            "grace_seconds", "hold_seconds", "stability_seconds", "recovery_timeout", "startup_timeout",
            "poll_seconds", "request_timeout", "recreate_jitter_ms", "drained_settle_seconds", "seed")}



def prepare_run(args):
    if args.context is None:
        try:
            result = subprocess.run(["kubectl", "config", "current-context"], text=True,
                                    capture_output=True, timeout=args.request_timeout)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise TestError("cannot read kubectl current context: " + str(error)) from error
        if result.returncode:
            raise TestError(result.stderr.strip() or "kubectl has no current context; use --context")
        args.context = result.stdout.strip()
    if not args.context.strip():
        raise TestError("kubectl context must not be empty; use --context")
    if args.output is None:
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M%S")
        args.output = str(pathlib.Path("_artifacts/vcjob-recreation") / (stamp + "-" + uuid.uuid4().hex[:8]))


def run(args):
    prepare_run(args)
    output = pathlib.Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    kube = Kubectl(args.context, args.request_timeout)
    experiment = Experiment(args, kube)
    rows, errors = [], []
    namespace_requested = False
    metadata = {"protocol_version": PROTOCOL, "script_sha256": hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),
                "run_id": experiment.run_id, "label": args.label, "context": args.context,
                "controller_target": {"namespace": args.controller_namespace, "deployment": args.controller_deployment},
                "started_at": utcnow(), "namespace": experiment.namespace, "parameters": protocol(args)}
    save(output / "metadata.json", metadata)
    rng = random.Random(args.seed)
    print("Context: %s; namespace: %s; iterations: %d; output: %s" %
          (args.context, experiment.namespace, args.iterations, output), flush=True)
    try:
        metadata["kubernetes"] = json.loads(kube.call(["version", "-o", "json"]))
        metadata["cluster_uid"] = kube.get("namespace", "kube-system")["metadata"]["uid"]
        nodes = sorted(kube.get("nodes")["items"], key=lambda n: n["metadata"]["name"])
        metadata["nodes"] = [{"name": n["metadata"]["name"], "capacity": n.get("status", {}).get("capacity"),
                              "allocatable": n.get("status", {}).get("allocatable")} for n in nodes]
        if not kube.get("queues.scheduling.volcano.sh", args.queue):
            raise TestError("configured Queue does not exist")
        experiment.initial_controller = experiment.controller()
        metadata["controller"] = experiment.initial_controller
        save(output / "metadata.json", metadata)
        namespace_requested = True
        namespace = kube.create({"apiVersion": "v1", "kind": "Namespace", "metadata": {
            "name": experiment.namespace, "labels": {LABEL: experiment.run_id}}})
        experiment.namespace_uid = namespace["metadata"]["uid"]
        metadata["namespace_uid"] = experiment.namespace_uid
        save(output / "metadata.json", metadata)
        prior_uid = None
        for index in range(1, args.iterations + 1):
            started = time.monotonic()
            row = {"iteration": index, "started_at": utcnow(), "outcome": "inconclusive", "reason": "interrupted",
                   "recreate_delay_seconds": round(rng.uniform(0, args.recreate_jitter_ms) / 1000
                                                   + (args.drained_settle_seconds if window_for(index) == "drained" else 0), 6)}
            try:
                prior_uid = experiment.trial(index, row, prior_uid)
                row.update(outcome="passed", reason="both replica recovery checks passed")
            except Reproduced as error:
                prior_uid = None
                row.update(outcome="reproduced", reason=str(error))
            except TestError as error:
                prior_uid = None
                row.update(outcome="inconclusive", reason=str(error))
            finally:
                row["duration_seconds"] = round(time.monotonic() - started, 3)
                evidence = output / ("trial-%04d" % index)
                if row["outcome"] != "passed":
                    experiment.snapshot(evidence, row["started_at"])
                evidence.mkdir(exist_ok=True)
                save(evidence / "result.json", row)
                rows.append(row)
                with (output / "results.jsonl").open("a") as journal:
                    journal.write(json.dumps(row, ensure_ascii=False) + "\n")
                print("[%d/%d] %s: %s" % (index, args.iterations, row["outcome"], row["reason"]), flush=True)
                save(output / "summary.json", summarize(rows, args.iterations))
            if prior_uid is None or index == args.iterations:
                experiment.cleanup_trial(row["job_name"], row.get("control_name"))
            # Never continue into a new controller revision/restart and mix samples.
            experiment.unchanged_controller()
    except (TestError, KeyboardInterrupt) as error:
        errors.append(str(error) or "interrupted")
    finally:
        # Recover the identity if namespace creation succeeded but its response timed out.
        if namespace_requested and not experiment.namespace_uid:
            try:
                ns = kube.get("namespace", experiment.namespace)
                if ns and ns["metadata"].get("labels", {}).get(LABEL) == experiment.run_id:
                    experiment.namespace_uid = ns["metadata"]["uid"]
                    metadata["namespace_uid"] = experiment.namespace_uid
            except TestError as error:
                errors.append("cannot verify namespace after create: " + str(error))
        if experiment.namespace_uid:
            try:
                experiment.cleanup_namespace()
            except TestError as error:
                errors.append("cleanup failed: " + str(error) + "; inspect namespace " + experiment.namespace)
        metadata["workload_image_ids"] = sorted({image for row in rows for image in row.get("workload_image_ids", [])})
        metadata["finished_at"] = utcnow()
        save(output / "metadata.json", metadata)
        summary = summarize(rows, args.iterations)
        summary["errors"] = errors
        save(output / "summary.json", summary)
        fields = ["iteration", "window", "outcome", "stage", "reason", "old_uid", "new_uid", "observed_replicas", "delete_to_create_seconds", "old_pods_gone_to_create_seconds", "duration_seconds"]
        with (output / "results.csv").open("w", newline="") as csvfile:
            writer = csv.DictWriter(csvfile, fields, extrasaction="ignore")
            writer.writeheader()
            writer.writerows(rows)
        print(json.dumps(summary, indent=2, ensure_ascii=False))
    if errors or summary["inconclusive"] or summary["not_attempted"]:
        return 2
    return 1 if summary["reproduced"] else 0


def compare(args):
    runs = []
    for path in (args.before, args.after):
        folder = pathlib.Path(path)
        runs.append((json.loads((folder / "metadata.json").read_text()),
                     json.loads((folder / "summary.json").read_text())))
    before, after = runs
    if any(not meta.get("finished_at") for meta, _ in runs):
        raise TestError("a run has not finalized; wait for completion or inspect the interrupted run")
    if (before[0]["protocol_version"] != after[0]["protocol_version"]
            or before[0].get("script_sha256") != after[0].get("script_sha256")
            or before[0]["parameters"] != after[0]["parameters"]):
        raise TestError("experiment parameters differ; run both versions with the same protocol and parameters")
    def image_ids(meta):
        return sorted({c["imageID"] for p in meta.get("controller", {}).get("pods", []) for c in p["containers"]})
    old_images, new_images = image_ids(before[0]), image_ids(after[0])
    if not old_images or not new_images or old_images == new_images:
        raise TestError("controller imageIDs are missing or identical; a before/after comparison requires different binaries")
    print("| Version | Attempted | Passed | Reproduced | Inconclusive | Observed rate (valid trials) |")
    print("| --- | ---: | ---: | ---: | ---: | ---: |")
    for meta, summary in runs:
        rate = summary["observed_reproduction_rate"]
        rate_text = "N/A" if rate is None else "%.2f%% (%d/%d)" % (100 * rate, summary["reproduced"], summary["valid"])
        print("| %s | %d/%d | %d | %d | %d | %s |" % (meta["label"], summary["attempted"], summary["requested"],
              summary["passed"], summary["reproduced"], summary["inconclusive"], rate_text))
        for window in ("overlap", "drained"):
            group = summary.get("by_window", {}).get(window, {})
            if group.get("attempted"):
                print("  %s %s: %d reproduced / %d valid" %
                      (meta["label"], window, group["reproduced"], group["passed"] + group["reproduced"]))
    comparable = True
    if before[0].get("controller_target") != after[0].get("controller_target"):
        print("INCOMPARABLE: experiments targeted different controller Deployments.")
        comparable = False
    if before[0].get("cluster_uid") != after[0].get("cluster_uid"):
        print("INCOMPARABLE: experiments used different clusters.")
        comparable = False
    if before[0].get("workload_image_ids") != after[0].get("workload_image_ids"):
        print("INCOMPARABLE: actual workload imageIDs differ.")
        comparable = False
    if before[0].get("nodes") != after[0].get("nodes") or before[0].get("kubernetes", {}).get("serverVersion") != after[0].get("kubernetes", {}).get("serverVersion"):
        print("INCOMPARABLE: cluster topology/capacity or Kubernetes version differs.")
        comparable = False
    def settings(meta):
        controller = meta["controller"]
        return {"replicas": controller["replicas"], "containers": [{k: v for k, v in c.items() if k != "image"} for c in controller["containers"]]}
    if settings(before[0]) != settings(after[0]):
        print("INCOMPARABLE: controller arguments/resources/replicas differ; isolate the binary change.")
        comparable = False
    if not comparable:
        return 2
    if any(s["errors"] or s["inconclusive"] or s["not_attempted"] for _, s in runs):
        print("INCOMPLETE: inspect inconclusive trials/errors; they are excluded from the rate.")
        return 2
    if not before[1]["reproduced"]:
        print("Baseline did not reproduce; this experiment cannot demonstrate the fix's effect.")
        return 2
    if after[1]["reproduced"]:
        print("The patched run still reproduced missing replicas; inspect the saved evidence.")
        return 1
    print("Baseline reproduced; no failures were observed after the fix in this scenario.")
    print("Zero failures does not prove zero risk. Under independent identical trials, the one-sided 95%% upper bound is %.2f%%." %
          (100 * after[1]["zero_failure_upper_95"]))
    return 0


def cleanup(args):
    metadata = json.loads(pathlib.Path(args.metadata).read_text())
    if args.context != metadata["context"]:
        raise TestError("cleanup context differs from recorded experiment context")
    kube = Kubectl(args.context, 10)
    namespace, run_id = metadata["namespace"], metadata["run_id"]
    ns = kube.get("namespace", namespace)
    if not ns:
        print("Namespace already removed.")
        return 0
    if (ns["metadata"]["uid"] != metadata.get("namespace_uid")
            or ns["metadata"].get("labels", {}).get(LABEL) != run_id):
        raise TestError("namespace identity does not match metadata; refusing cleanup")
    experiment = Experiment(argparse.Namespace(poll_seconds=0.5), kube)
    experiment.namespace, experiment.namespace_uid, experiment.run_id = namespace, ns["metadata"]["uid"], run_id
    for pod in kube.get("pods", namespace=namespace, selector=LABEL + "=" + run_id)["items"]:
        if FINALIZER in pod["metadata"].get("finalizers", []):
            experiment.held[pod["metadata"]["name"]] = pod
    experiment.cleanup_namespace()
    print("Released only the test finalizer and requested deletion of " + namespace)
    return 0


def parser():
    root = argparse.ArgumentParser(description=__doc__)
    subs = root.add_subparsers(dest="command", required=True)
    run_parser = subs.add_parser("run", help="test the currently installed controller (default command)")
    run_parser.add_argument("--context", help="kubectl context; defaults to current-context")
    run_parser.add_argument("--label", default="current", help="report label, e.g. before or after; default: current")
    run_parser.add_argument("--output", help="new output directory; default: _artifacts/vcjob-recreation/<time>-<id>")
    run_parser.add_argument("--iterations", type=int, default=100)
    run_parser.add_argument("--replicas", type=int, default=8)
    run_parser.add_argument("--image", default="busybox:1.36.1", help="must provide sh and sleep; pre-pull on all nodes")
    run_parser.add_argument("--scheduler", default="volcano")
    run_parser.add_argument("--queue", default="default")
    run_parser.add_argument("--controller-namespace", default="volcano-system")
    run_parser.add_argument("--controller-deployment", default="volcano-controllers")
    run_parser.add_argument("--mode", choices=("finalizer", "natural"), default="finalizer")
    run_parser.add_argument("--grace-seconds", type=int, default=5)
    run_parser.add_argument("--hold-seconds", type=float, default=20)
    run_parser.add_argument("--stability-seconds", type=float, default=15)
    run_parser.add_argument("--recovery-timeout", type=float, default=60)
    run_parser.add_argument("--startup-timeout", type=float, default=120)
    run_parser.add_argument("--poll-seconds", type=float, default=0.5)
    run_parser.add_argument("--request-timeout", type=int, default=10)
    run_parser.add_argument("--recreate-jitter-ms", type=float, default=200)
    run_parser.add_argument("--drained-settle-seconds", type=float, default=0.5,
                            help="wait after old Pods disappear in drained rounds (default: 0.5)")
    run_parser.add_argument("--seed", type=int, default=20260930)
    comparison = subs.add_parser("compare", help="compare two saved runs without accessing Kubernetes")
    comparison.add_argument("--before", required=True)
    comparison.add_argument("--after", required=True)
    recovery = subs.add_parser("cleanup", help="clean a run interrupted by SIGKILL or a failed API request")
    recovery.add_argument("--context", required=True)
    recovery.add_argument("--metadata", required=True, help="metadata.json from that run")
    return root


def main():
    cli = parser()
    args = cli.parse_args(sys.argv[1:] or ["run"])
    if args.command == "run":
        if args.context is not None and not args.context.strip():
            cli.error("context must not be empty")
        for key in ("iterations", "replicas", "grace_seconds", "startup_timeout", "recovery_timeout", "poll_seconds", "request_timeout"):
            if not math.isfinite(getattr(args, key)) or getattr(args, key) <= 0:
                cli.error(key.replace("_", "-") + " must be positive")
        for key in ("hold_seconds", "stability_seconds", "recreate_jitter_ms", "drained_settle_seconds"):
            if not math.isfinite(getattr(args, key)) or getattr(args, key) < 0:
                cli.error(key.replace("_", "-") + " must be nonnegative")
        def stop(_signum, _frame):
            raise KeyboardInterrupt()
        signal.signal(signal.SIGTERM, stop)
    try:
        return {"run": run, "compare": compare, "cleanup": cleanup}[args.command](args)
    except (TestError, OSError, ValueError, KeyError) as error:
        print("ERROR: " + str(error), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
