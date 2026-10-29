#!/usr/bin/env python3
# Copyright 2026 The Volcano Authors.
# SPDX-License-Identifier: Apache-2.0
"""Offline regression tests: python3 -m unittest discover -s hack/diagnostics -v."""

import copy
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from urllib.parse import parse_qs, urlsplit

import watch_pod_status as recorder


def pod(rv="10", uid="pod-uid", message="HyperNode: 0/7"):
    return {"kind": "Pod", "metadata": {"name": "pending", "namespace": "test",
            "uid": uid, "resourceVersion": rv}, "spec": {"nodeName": ""},
            "status": {"phase": "Pending", "conditions": [{"type": "PodScheduled",
                       "status": "False", "message": message, "reason": "Unschedulable"}]}}


def audit(request_rv="10", response_rv="11", audit_id="request-1", uid="pod-uid"):
    return {"kind": "Event", "stage": "ResponseComplete", "verb": "update",
            "auditID": audit_id, "objectRef": {"resource": "pods", "subresource": "status",
            "namespace": "test", "name": "pending", "uid": uid},
            "responseStatus": {"code": 200}, "user": {"username": "system:serviceaccount:volcano-system:volcano-scheduler"},
            "userAgent": "volcano-scheduler/test", "requestObject": {"metadata": {"resourceVersion": request_rv}},
            "responseObject": pod(response_rv, uid)}


class MemoryOutput:
    def __init__(self):
        self.records = []
        self.diagnostics = []

    def emit(self, name, obj, console=False):
        self.records.append((name, obj))

    def diagnostic(self, message, **extra):
        self.diagnostics.append((message, extra))


def status_manager(manager="volcano-scheduler", condition="PodScheduled", stamp="2026-10-29T00:00:00Z"):
    return {"manager": manager, "operation": "Update", "apiVersion": "v1", "subresource": "status",
            "time": stamp, "fieldsType": "FieldsV1", "fieldsV1": {"f:status": {"f:conditions": {
                ".": {}, 'k:{"type":"' + condition + '"}': {".": {}, "f:message": {}, "f:status": {}}
            }}}}


class ComponentHintTests(unittest.TestCase):
    def setUp(self):
        self.before = pod()
        self.before["metadata"]["managedFields"] = [status_manager()]
        self.after = copy.deepcopy(self.before)
        self.after["metadata"]["resourceVersion"] = "11"
        self.after["status"]["conditions"][0]["message"] = "pod group is not ready"

    def hints(self, kind="MODIFIED"):
        return recorder.component_hints(self.before, self.after, kind)

    def test_changed_message_and_manager_time_give_likely_component(self):
        self.after["metadata"]["managedFields"][0]["time"] = "2026-10-29T00:00:01Z"
        result = self.hints()
        self.assertEqual(result["state"], "likely")
        self.assertEqual(result["inferredManagers"], ["volcano-scheduler"])
        self.assertEqual(result["candidates"][0]["changedOwnedPaths"], ['/status/conditions[type="PodScheduled"]/message'])
        self.assertTrue(result["notProofOfWriter"])

    def test_same_second_message_changes_still_have_ownership_hint(self):
        result = self.hints()
        self.assertEqual(result["state"], "possible")
        self.assertFalse(result["candidates"][0]["timeChanged"])
        self.assertEqual(result["candidates"][0]["evidence"], "field_ownership_only")

    def test_ownership_transfer_does_not_blame_previous_owner(self):
        self.after["metadata"]["managedFields"] = [status_manager("other-status-writer")]
        result = self.hints()
        self.assertEqual(result["inferredManagers"], ["other-status-writer"])
        self.assertEqual(result["previousOwners"][0]["manager"], "volcano-scheduler")

    def test_latest_timestamp_without_matching_changed_field_is_excluded(self):
        self.after["metadata"]["managedFields"].append(status_manager("kubelet", "Ready", "2026-10-29T12:00:00Z"))
        result = self.hints()
        self.assertEqual(result["inferredManagers"], ["volcano-scheduler"])
        self.assertEqual(len(result["candidates"]), 1)

    def test_new_ready_condition_does_not_implicate_scheduled_condition_owner(self):
        self.after["status"] = copy.deepcopy(self.before["status"])
        self.after["status"]["conditions"].append({"type": "Ready", "status": "False"})
        self.after["metadata"]["managedFields"].append(status_manager("kubelet", "Ready"))
        self.assertEqual(self.hints()["inferredManagers"], ["kubelet"])

    def test_condition_order_does_not_change_field_identity(self):
        self.before["status"]["conditions"].append({"type": "Ready", "status": "False"})
        self.after["status"]["conditions"].insert(0, {"type": "Ready", "status": "False"})
        result = self.hints()
        self.assertEqual(result["inferredManagers"], ["volcano-scheduler"])
        self.assertEqual(len(result["candidates"][0]["changedOwnedPaths"]), 1)

    def test_condition_order_only_does_not_infer_a_writer(self):
        self.before["status"]["conditions"].append({"type": "Ready", "status": "False"})
        self.after["status"]["conditions"] = list(reversed(self.before["status"]["conditions"]))
        self.assertEqual(self.hints()["state"], "unknown")

    def test_field_removal_preserves_previous_owner_without_claiming_writer(self):
        del self.after["status"]["conditions"][0]["message"]
        del self.after["metadata"]["managedFields"][0]["fieldsV1"]["f:status"]["f:conditions"]['k:{"type":"PodScheduled"}']["f:message"]
        result = self.hints()
        self.assertEqual(result["state"], "unknown")
        self.assertEqual(result["previousOwners"][0]["manager"], "volcano-scheduler")

    def test_multiple_matching_managers_remain_ambiguous(self):
        for item in (self.before, self.after):
            item["metadata"]["managedFields"].append(status_manager("shared-owner"))
        self.assertEqual(self.hints()["state"], "ambiguous")

    def test_scheduler_and_owner_reference_are_not_writer_evidence(self):
        for item in (self.before, self.after):
            item["metadata"]["managedFields"] = []
            item["spec"]["schedulerName"] = "volcano"
            item["metadata"]["ownerReferences"] = [{"kind": "ModelServing", "name": "kthena-model"}]
        self.assertEqual(self.hints()["state"], "unknown")
        self.assertEqual(self.hints()["inferredManagers"], [])

    def test_metadata_only_and_snapshots_not_attributed(self):
        self.assertEqual(self.hints("RESYNC")["state"], "not_a_comparable_update")
        self.after["status"] = copy.deepcopy(self.before["status"])
        self.after["metadata"]["managedFields"][0]["time"] = "2026-10-29T12:00:00Z"
        self.assertEqual(self.hints()["state"], "no_status_change")

    def test_legacy_main_resource_status_owner_is_supported(self):
        for item in (self.before, self.after):
            del item["metadata"]["managedFields"][0]["subresource"]
        self.assertEqual(self.hints()["inferredManagers"], ["volcano-scheduler"])


class AuditTests(unittest.TestCase):
    def test_diff_preserves_null_remove_and_nested_boolean_types(self):
        diff = recorder.json_diff({"a/b": True, "remove": 1}, {"a/b": 1, "null": None})
        self.assertEqual([d["op"] for d in diff], ["replace", "add", "remove"])
        self.assertEqual(diff[0]["path"], "/a~1b")
        self.assertEqual(recorder.json_diff({"a": [True]}, {"a": [1]})[0]["path"], "/a/0")

    def test_advanced_version_confirms_caller_and_noop_does_not(self):
        advanced = recorder.audit_record(audit())
        noop = recorder.audit_record(audit("11", "11", "noop"))
        result = recorder.writer_result([advanced, noop])
        self.assertEqual(result["state"], "confirmed_version_advanced")
        self.assertEqual(result["confirmedAuditID"], "request-1")
        self.assertEqual(len(result["candidates"]), 1)
        self.assertEqual(recorder.writer_result([noop])["state"], "unknown")

    def test_response_version_only_is_unproven(self):
        event = audit()
        event["verb"] = "patch"
        event["requestObject"] = {"status": {"phase": "Pending"}}
        record = recorder.audit_record(event)
        self.assertEqual(recorder.writer_result([record])["state"], "response_version_match_unproven")
        event["auditID"] = "second"
        self.assertEqual(recorder.writer_result([record, recorder.audit_record(event)])["state"], "ambiguous")

    def test_metadata_only_never_uses_request_rv_as_written_rv(self):
        event = audit()
        del event["responseObject"]
        record = recorder.audit_record(event)
        self.assertIsNone(record["responseResourceVersion"])
        self.assertEqual(record["writeEvidence"], "no_response_pod_version")

    def test_impersonated_user_kept_separately(self):
        event = audit()
        event["impersonatedUser"] = {"username": "test-user"}
        record = recorder.audit_record(event)
        self.assertEqual(record["user"], event["user"])
        self.assertEqual(record["impersonatedUser"]["username"], "test-user")

    def test_failed_incomplete_mismatched_and_dry_run_requests_ignored(self):
        changes = [lambda e: e.update(stage="RequestReceived"),
                   lambda e: e["responseStatus"].update(code=409),
                   lambda e: e["responseObject"]["metadata"].update(uid="different"),
                   lambda e: e.update(requestURI="/api/v1/pods?dryRun=All")]
        for change in changes:
            event = audit()
            change(event)
            self.assertIsNone(recorder.audit_record(event))


class CollectorTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.index = recorder.AuditIndex(Path(self.tmp.name) / "index.sqlite")
        self.output = MemoryOutput()
        args = recorder.parser().parse_args(["watch", "-n", "test", "--pod", "pending"])
        self.collector = recorder.Collector(args, self.output, self.index, None, threading.Event())

    def tearDown(self):
        self.index.close()
        self.tmp.cleanup()

    def test_late_audit_same_uid_rv_and_duplicate_delivery(self):
        self.collector.pod_event("INITIAL", pod())
        self.collector.pod_event("MODIFIED", pod("11", message="pod group is not ready"))
        self.collector.pod_event("MODIFIED", pod("11"))
        self.collector.audit(audit())
        self.collector.audit(audit())
        updates = [r for n, r in self.output.records if n == "pod-updates.jsonl"]
        attrs = [r for n, r in self.output.records if n == "attributions.jsonl"]
        self.assertEqual(len(updates), 2)
        self.assertTrue(updates[1]["messageChanged"])
        self.assertEqual(updates[1]["previousStatus"], pod()["status"])
        self.assertEqual(updates[1]["componentHints"]["state"], "unknown")
        self.assertEqual(len(attrs), 1)
        self.assertEqual(attrs[0]["eventID"], updates[1]["eventID"])
        self.assertEqual(attrs[0]["writer"]["state"], "confirmed_version_advanced")

    def test_metadata_update_not_status_change_and_recreated_uid_isolated(self):
        self.collector.pod_event("INITIAL", pod())
        self.collector.pod_event("MODIFIED", pod("11"))
        self.collector.pod_event("DELETED", pod("12"))
        self.collector.pod_event("ADDED", pod("13", uid="replacement"))
        self.collector.audit(audit("12", "13"))  # Wrong UID, same name and version.
        updates = [r for n, r in self.output.records if n == "pod-updates.jsonl"]
        self.assertFalse(updates[1]["statusChanged"])
        self.assertIsNone(updates[-1]["previousStatus"])
        self.assertFalse(any(n == "attributions.jsonl" for n, _ in self.output.records))

    def test_initial_snapshot_not_attributed_as_new_write(self):
        self.collector.audit(audit())
        self.collector.pod_event("INITIAL", pod("11"))
        self.assertEqual(self.output.records[-1][1]["writer"]["state"], "not_an_observed_write")

    def test_missing_after_resync_is_not_a_fabricated_deletion(self):
        self.collector.snapshot([pod()], True)
        self.collector.snapshot([], False)
        self.assertEqual(self.output.records[-1][1]["type"], "MISSING_AFTER_RESYNC")
        self.assertFalse(self.collector.pods)

    def test_enriched_export_joins_late_audit(self):
        output = recorder.Output(self.tmp.name)
        output.emit("pod-updates.jsonl", {"type": "POD", "uid": "pod-uid", "resourceVersion": "11", "eventType": "MODIFIED"})
        output.close()
        self.index.add(recorder.audit_record(audit()))
        enriched = Path(self.tmp.name) / "enriched.jsonl"
        recorder.export_enriched(self.tmp.name, self.index, enriched)
        self.assertEqual(json.loads(enriched.read_text())["writer"]["state"], "confirmed_version_advanced")

    def test_offline_correlate_command_preserves_original(self):
        directory = Path(self.tmp.name)
        source = directory / "pod-updates.jsonl"
        original = json.dumps({"type": "POD", "uid": "pod-uid", "resourceVersion": "11",
                               "eventType": "MODIFIED", "writer": {"state": "unknown"}}) + "\n"
        source.write_text(original)
        audit_path = directory / "audit.jsonl"
        audit_path.write_text(json.dumps(audit()) + "\n")
        subprocess.run([sys.executable, recorder.__file__, "correlate", "--capture", str(directory),
                        "--audit-log", str(audit_path)], check=True, capture_output=True, text=True)
        enriched = json.loads((directory / "pod-updates.enriched.jsonl").read_text())
        self.assertEqual(enriched["writer"]["state"], "confirmed_version_advanced")
        self.assertEqual(source.read_text(), original)


class WatchTests(unittest.TestCase):
    def test_batch_reconnect_bookmark_and_expired_version(self):
        stop = threading.Event()
        output = MemoryOutput()
        snapshots, events, gaps, urls = [], [], [], []

        class Process:
            def __init__(self, items, last):
                self.stdout = io.StringIO("".join(json.dumps(x) + "\n" for x in items))
                self.returncode = 0
                self.last = last

            def poll(self):
                return 0

            def wait(self, timeout=None):
                if self.last:
                    stop.set()
                return 0

        class Kube:
            def get(self, args):
                return {"items": [pod("20")], "metadata": {"resourceVersion": "20"}}

            def spawn(self, args, **kwargs):
                urls.append(args[-1])
                batches = [
                    [{"type": "MODIFIED", "object": pod("11")}, {"type": "MODIFIED", "object": pod("12")}],
                    [{"type": "ERROR", "object": {"code": 410, "reason": "Expired"}}],
                    [{"type": "MODIFIED", "object": pod("21")}, {"type": "BOOKMARK", "object": {"metadata": {"resourceVersion": "22"}}}],
                ]
                if len(urls) > 3:
                    stop.set()
                    raise AssertionError("unexpected reconnect")
                return Process(batches[len(urls) - 1], len(urls) == 3)

            def forget(self, process):
                pass

        recorder.watch_collection(Kube(), stop, output, "/pods", {},
            lambda items, first: snapshots.append(first),
            lambda kind, obj: events.append(obj["metadata"]["resourceVersion"]),
            {"items": [pod()], "metadata": {"resourceVersion": "10"}}, lambda: gaps.append(1))
        self.assertEqual(events, ["11", "12", "21"])
        self.assertEqual(snapshots, [True, False])
        self.assertEqual(gaps, [1])
        self.assertEqual([parse_qs(urlsplit(x).query)["resourceVersion"][0] for x in urls], ["10", "12", "20"])

    def test_audit_follower_partial_line_and_rotation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "audit.log"
            event = json.dumps(audit())
            path.write_text(event[:len(event) // 2])
            stop, received = threading.Event(), threading.Event()
            records, output = [], MemoryOutput()

            def callback(item):
                records.append(item)
                if len(records) == 2:
                    received.set()

            thread = threading.Thread(target=recorder.follow_audit, args=(path, stop, output, callback))
            thread.start()
            try:
                with path.open("a") as handle:
                    handle.write(event[len(event) // 2:] + "\n")
                deadline = time.monotonic() + 3
                while not records and time.monotonic() < deadline:
                    stop.wait(0.01)
                self.assertEqual(len(records), 1)
                path.rename(path.with_suffix(".old"))
                path.write_text(json.dumps(audit(audit_id="second")) + "\n")
                self.assertTrue(received.wait(3))
                self.assertEqual([x["auditID"] for x in records], ["request-1", "second"])
            finally:
                stop.set()
                thread.join(3)
            self.assertFalse(thread.is_alive())


if __name__ == "__main__":
    unittest.main()
