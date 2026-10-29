#!/usr/bin/env python3
# Copyright 2026 The Volcano Authors.
# SPDX-License-Identifier: Apache-2.0
"""Read-only Pod status recorder. Requires Python 3.8+ and kubectl, no pip packages."""

import argparse
import datetime
import gzip
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import parse_qs, quote, urlencode, urlsplit


def utcnow():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def json_diff(old, new, path=""):
    """JSON Pointer paths; preserve removals, nulls and type changes."""
    if isinstance(old, dict) and isinstance(new, dict):
        result = []
        for key in sorted(set(old) | set(new)):
            child = path + "/" + key.replace("~", "~0").replace("/", "~1")
            if key not in old:
                result.append({"op": "add", "path": child, "new": new[key]})
            elif key not in new:
                result.append({"op": "remove", "path": child, "old": old[key]})
            else:
                result.extend(json_diff(old[key], new[key], child))
        return result
    if isinstance(old, list) and isinstance(new, list) and len(old) == len(new):
        result = []
        for i, (before, after) in enumerate(zip(old, new)):
            result.extend(json_diff(before, after, path + "/" + str(i)))
        return result
    if type(old) is type(new) and old == new:
        return []
    return [{"op": "replace", "path": path or "/", "old": old, "new": new}]


def scheduled_condition(pod):
    return next((c for c in pod.get("status", {}).get("conditions", [])
                 if c.get("type") == "PodScheduled"), None)


def status_managers(pod):
    # Field ownership is a clue, not the authenticated author of this write.
    return [m for m in pod.get("metadata", {}).get("managedFields", [])
            if m.get("subresource") == "status" or
            "f:status" in m.get("fieldsV1", {})]


def owned_status_paths(entry):
    """FieldsV1 leaves; '.' owns membership, not every descendant's value."""
    def walk(tree, path):
        if not isinstance(tree, dict):
            return
        if not tree:
            yield path
        for token, child in tree.items():
            if token == ".":
                yield path + (".",)
            else:
                yield from walk(child, path + (token,))

    fields = entry.get("fieldsV1", {})
    if "f:status" in fields and entry.get("fieldsType", "FieldsV1") == "FieldsV1":
        yield from walk(fields["f:status"], ("f:status",))


_MISSING = object()
_UNRESOLVED = object()


def owned_value(pod, path):
    """Resolve associative list entries by their keys, never by condition order."""
    value = pod
    try:
        for token in path:
            if token == ".":
                return value is not _MISSING
            if value is _MISSING:
                continue
            if token.startswith("f:"):
                value = value.get(token[2:], _MISSING) if isinstance(value, dict) else _MISSING
            elif token.startswith("k:"):
                keys = json.loads(token[2:])
                if not isinstance(keys, dict):
                    return _UNRESOLVED
                value = next((item for item in value if isinstance(item, dict) and
                              all(k in item and not json_diff(item[k], v) for k, v in keys.items())),
                             _MISSING) if isinstance(value, list) else _MISSING
            elif token.startswith("v:"):
                member = json.loads(token[2:])
                value = next((item for item in value if not json_diff(item, member)),
                             _MISSING) if isinstance(value, list) else _MISSING
            elif token.startswith("i:"):
                position = int(token[2:])
                value = (value[position] if isinstance(value, list) and 0 <= position < len(value)
                         else _MISSING)
            else:
                return _UNRESOLVED
        return value
    except (ValueError, TypeError):
        return _UNRESOLVED


def owned_path_label(path):
    result = ""
    for token in path:
        if token.startswith("f:"):
            result += "/" + token[2:].replace("~", "~0").replace("/", "~1")
        elif token == ".":
            result += "/@membership"
        elif token.startswith("k:"):
            result += "[" + ",".join(k + "=" + json.dumps(v, ensure_ascii=False)
                                     for k, v in sorted(json.loads(token[2:]).items())) + "]"
        else:
            result += "[" + token + "]"
    return result


def changed_owned_paths(entry, before, after):
    paths = []
    for path in owned_status_paths(entry):
        old, new = owned_value(before, path), owned_value(after, path)
        if old is _UNRESOLVED or new is _UNRESOLVED:
            continue
        if old is _MISSING or new is _MISSING:
            changed = old is not new
        else:
            changed = bool(json_diff(old, new))
        if changed:
            paths.append(owned_path_label(path))
    return sorted(set(paths))


def component_hints(before, after, kind):
    """Heuristic field-manager attribution, deliberately independent of audit proof."""
    result = {
        "state": "unknown", "scope": "status", "notProofOfWriter": True,
        "inferredManagers": [], "candidates": [], "previousOwners": [],
        "note": ("manager 是客户端声明/推导的字段管理标识，不是经过认证的组件身份。"
                 "time 和字段归属变化仅作推测依据；同秒更新可能没有时间差。"
                 "schedulerName/ownerReferences 仅表示调度配置和控制关系。"),
    }
    if before is None or kind != "MODIFIED":
        result["state"] = "not_a_comparable_update"
        return result
    if not json_diff(before.get("status", {}), after.get("status", {})):
        result["state"] = "no_status_change"
        return result

    def identity(entry):
        return tuple(entry.get(k, "") for k in ("manager", "operation", "apiVersion", "subresource"))

    previous = {identity(entry): entry for entry in status_managers(before)}
    current = {identity(entry): entry for entry in status_managers(after)}
    for key, entry in current.items():
        paths = changed_owned_paths(entry, before, after)
        if not paths or not entry.get("manager"):
            continue
        old = previous.get(key, {})
        old_paths = changed_owned_paths(old, before, after)
        new_paths = sorted(set(paths) - set(old_paths))
        time_changed = bool(entry.get("time") and entry.get("time") != old.get("time"))
        result["candidates"].append({
            "manager": entry["manager"], "operation": entry.get("operation"),
            "subresource": entry.get("subresource", ""), "apiVersion": entry.get("apiVersion"),
            "time": entry.get("time"), "previousTime": old.get("time"),
            "timeChanged": time_changed, "entryAdded": key not in previous,
            "fieldsChanged": entry.get("fieldsV1") != old.get("fieldsV1"),
            "changedOwnedPaths": paths, "newlyOwnedChangedPaths": new_paths,
            "evidence": "managed_fields_delta" if time_changed or new_paths else "field_ownership_only",
        })
    # Losing ownership is evidence about the previous owner, not about the writer.
    for key, entry in previous.items():
        lost = sorted(set(changed_owned_paths(entry, before, after)) -
                      set(changed_owned_paths(current.get(key, {}), before, after)))
        if lost:
            result["previousOwners"].append({"manager": entry.get("manager"),
                                            "subresource": entry.get("subresource", ""),
                                            "lostChangedPaths": lost})
    strong = [c for c in result["candidates"] if c["evidence"] == "managed_fields_delta"]
    inferred = sorted({c["manager"] for c in strong or result["candidates"]})
    result["inferredManagers"] = inferred
    result["state"] = ("likely" if strong and len(inferred) == 1 else
                       "possible" if len(inferred) == 1 else
                       "ambiguous" if inferred else "unknown")
    return result


def audit_record(event):
    """Extract successful Pod writes; never use request.resourceVersion as response RV."""
    ref = event.get("objectRef", {})
    if (event.get("stage") != "ResponseComplete" or ref.get("resource") != "pods"
            or ref.get("apiGroup", "") != ""
            or event.get("verb") not in ("create", "update", "patch", "delete")
            or ref.get("subresource", "") not in ("", "status")):
        return None
    code = event.get("responseStatus", {}).get("code", 0)
    if not isinstance(code, int) or not 200 <= code < 300:
        return None
    if parse_qs(urlsplit(event.get("requestURI", "")).query).get("dryRun"):
        return None
    response = event.get("responseObject")
    metadata = (response.get("metadata", {}) if isinstance(response, dict)
                and response.get("kind") == "Pod" else {})
    request = event.get("requestObject")
    request_rv = (request.get("metadata", {}).get("resourceVersion")
                  if isinstance(request, dict) else None)
    uid, rv = metadata.get("uid"), metadata.get("resourceVersion")
    # Ignore malformed/mismatched audit objects rather than cross-linking Pods.
    if metadata and any(ref.get(k) and metadata.get(k) != ref[k]
                        for k in ("uid", "name", "namespace")):
        return None
    return {
        "auditID": event.get("auditID"),
        "namespace": ref.get("namespace"), "pod": ref.get("name"),
        "uid": uid, "responseResourceVersion": rv,
        "requestResourceVersion": request_rv,
        "verb": event.get("verb"), "subresource": ref.get("subresource", ""),
        "user": event.get("user", {}),
        "impersonatedUser": event.get("impersonatedUser"),
        "userAgent": event.get("userAgent"), "sourceIPs": event.get("sourceIPs", []),
        "requestURI": event.get("requestURI"),
        "requestReceivedTimestamp": event.get("requestReceivedTimestamp"),
        "stageTimestamp": event.get("stageTimestamp"), "responseCode": code,
        "writeEvidence": ("no_response_pod_version" if not uid or not rv else
                          "no_version_change" if request_rv == rv else
                          "version_advanced" if rv and request_rv else
                          "response_version_only" if uid and rv else
                          "no_response_pod_version"),
    }


def writer_result(candidates, eligible=True):
    if not eligible:
        return {"state": "not_an_observed_write", "candidates": []}
    candidates = [c for c in candidates if c["writeEvidence"] != "no_version_change"]
    advanced = [c for c in candidates if c["writeEvidence"] == "version_advanced"]
    return {
        "state": ("confirmed_version_advanced" if len(advanced) == 1 else
                  "ambiguous" if len(advanced) > 1 or len(candidates) > 1 else
                  "response_version_match_unproven" if candidates else "unknown"),
        "candidates": candidates,
        "confirmedAuditID": advanced[0]["auditID"] if len(advanced) == 1 else None,
        "note": ("API 调用者不等于 Volcano 内部触发分支；后者需对照 scheduler 日志。"
                 "只有响应版本、没有请求版本的 patch 也可能是 no-op，不能唯一归因。"
                 if candidates else
                 "没有匹配的审计写入证据；managedFields/Event 不能确认本次更新者。"),
    }


class Output:
    def __init__(self, directory, pretty=False):
        self.directory = Path(directory)
        self.pretty = pretty
        self.lock = threading.RLock()
        self.files = {}

    def emit(self, name, obj, console=False):
        text = json.dumps(obj, ensure_ascii=False, separators=(",", ":"))
        with self.lock:
            if name not in self.files:
                self.files[name] = (self.directory / name).open("a", encoding="utf-8")
            self.files[name].write(text + "\n")
            self.files[name].flush()
            if console:
                try:
                    print(json.dumps(obj, ensure_ascii=False, indent=2)
                          if self.pretty else text, flush=True)
                except BrokenPipeError:
                    # Keep recording even if a terminal consumer such as head exits.
                    sys.stdout = open(os.devnull, "w")

    def diagnostic(self, message, **extra):
        record = {"type": "DIAGNOSTIC", "observedAt": utcnow(),
                  "message": message, **extra}
        self.emit("diagnostics.jsonl", record)
        print(message, file=sys.stderr, flush=True)

    def close(self):
        with self.lock:
            for handle in self.files.values():
                handle.close()


class AuditIndex:
    """Disk-backed join index so a long capture does not retain every status in RAM."""
    def __init__(self, path):
        self.lock = threading.RLock()
        self.db = sqlite3.connect(str(path), check_same_thread=False)
        self.db.executescript("""
            CREATE TABLE IF NOT EXISTS updates (
              uid TEXT, rv TEXT, event_id TEXT PRIMARY KEY, kind TEXT);
            CREATE INDEX IF NOT EXISTS update_version ON updates(uid,rv);
            CREATE TABLE IF NOT EXISTS audit (
              audit_id TEXT PRIMARY KEY, uid TEXT, rv TEXT, payload TEXT);
            CREATE INDEX IF NOT EXISTS audit_version ON audit(uid,rv);
        """)

    def candidates(self, uid, rv):
        with self.lock:
            return [json.loads(row[0]) for row in self.db.execute(
                "SELECT payload FROM audit WHERE uid=? AND rv=?", (uid, rv))]

    def update(self, uid, rv, event_id, kind):
        with self.lock:
            self.db.execute("INSERT OR IGNORE INTO updates VALUES (?,?,?,?)",
                            (uid, rv, event_id, kind))
            self.db.commit()
            return writer_result(self.candidates(uid, rv),
                                 kind in ("ADDED", "MODIFIED", "DELETED"))

    def add(self, record):
        if not (record.get("auditID") and record.get("uid")
                and record.get("responseResourceVersion")):
            return False, []
        with self.lock:
            changed = self.db.execute("INSERT OR IGNORE INTO audit VALUES (?,?,?,?)",
                                     (record["auditID"], record["uid"],
                                      record["responseResourceVersion"],
                                      json.dumps(record))).rowcount
            self.db.commit()
            rows = self.db.execute("SELECT event_id,kind FROM updates WHERE uid=? AND rv=?",
                                   (record["uid"], record["responseResourceVersion"])).fetchall()
            return bool(changed), rows

    def close(self):
        self.db.close()


class Kubectl:
    def __init__(self, args, stop):
        self.base = ["kubectl"]
        if args.kubeconfig:
            self.base += ["--kubeconfig", args.kubeconfig]
        if args.context:
            self.base += ["--context", args.context]
        self.stop = stop
        self.lock = threading.Lock()
        self.processes = set()

    def spawn(self, args, **kwargs):
        with self.lock:
            if self.stop.is_set():
                raise InterruptedError("capture stopped")
            p = subprocess.Popen(self.base + args, **kwargs)
            self.processes.add(p)
            return p

    def forget(self, process):
        with self.lock:
            self.processes.discard(process)

    def get(self, args):
        p = self.spawn(["--request-timeout=20s"] + args, stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, text=True, encoding="utf-8")
        try:
            stdout, stderr = p.communicate(timeout=25)
            if p.returncode:
                raise RuntimeError(stderr.strip() or "kubectl exited " + str(p.returncode))
            return json.loads(stdout)
        except subprocess.TimeoutExpired:
            p.kill()
            p.communicate()
            raise RuntimeError("kubectl request timed out")
        finally:
            self.forget(p)

    def terminate_all(self):
        with self.lock:
            processes = list(self.processes)
        for p in processes:
            if p.poll() is None:
                p.terminate()
        for p in processes:
            try:
                p.wait(timeout=2)
            except subprocess.TimeoutExpired:
                p.kill()


class Collector:
    def __init__(self, args, output, index, kube, stop):
        self.args, self.output, self.index = args, output, index
        self.kube, self.stop = kube, stop
        self.lock = threading.RLock()
        self.pods = {}
        self.sequence = 0
        self.counts = {"podObservations": 0, "statusChanges": 0, "gaps": 0}

    def selected(self, pod):
        m = pod.get("metadata", {})
        return ((not self.args.pod or m.get("name") in self.args.pod) and
                (not self.args.pod_group or
                 m.get("annotations", {}).get("scheduling.k8s.io/group-name") == self.args.pod_group))

    def pod_event(self, kind, pod, source="watch"):
        m = pod.get("metadata", {})
        uid, rv = m.get("uid"), m.get("resourceVersion")
        if not uid or not rv:
            return
        with self.lock:
            old = self.pods.get(uid)
            if not self.selected(pod) and old is None:
                return
            if old and old["metadata"]["resourceVersion"] == rv and kind != "DELETED":
                return
            self.sequence += 1
            event_id = str(self.sequence)
            before = old.get("status", {}) if old else None
            after = pod.get("status", {})
            changes = json_diff(before, after, "/status") if old else []
            writer = self.index.update(uid, rv, event_id, kind)
            record = {
                "type": "POD", "eventID": event_id, "eventType": kind,
                "observedAt": utcnow(), "source": source,
                "namespace": m.get("namespace"), "pod": m.get("name"),
                "uid": uid, "resourceVersion": rv,
                "previousResourceVersion": old["metadata"]["resourceVersion"] if old else None,
                "creationTimestamp": m.get("creationTimestamp"),
                "deletionTimestamp": m.get("deletionTimestamp"),
                "podGroup": m.get("annotations", {}).get("scheduling.k8s.io/group-name"),
                "schedulerName": pod.get("spec", {}).get("schedulerName", ""),
                "ownerReferences": m.get("ownerReferences", []),
                "nodeName": pod.get("spec", {}).get("nodeName", ""),
                "nominatedNodeName": after.get("nominatedNodeName", ""),
                "statusChanged": bool(changes), "statusDiff": changes,
                "previousStatus": before, "status": after,
                "previousPodScheduled": scheduled_condition(old) if old else None,
                "podScheduled": scheduled_condition(pod),
                "messageChanged": (old is not None and
                                   (scheduled_condition(old) or {}).get("message") !=
                                   (scheduled_condition(pod) or {}).get("message")),
                "managedFieldsHints": {
                    "notProofOfWriter": True,
                    "statusEntries": status_managers(pod),
                    "previousStatusEntries": status_managers(old) if old else [],
                    "changed": old is not None and status_managers(old) != status_managers(pod),
                },
                "componentHints": component_hints(old, pod, kind),
                "writer": writer,
            }
            self.output.emit("pod-updates.jsonl", record, console=True)
            if self.args.raw_pods:
                self.output.emit("raw-pods.jsonl", {"eventType": kind, "object": pod})
            self.counts["podObservations"] += 1
            self.counts["statusChanges"] += bool(changes)
            if kind == "DELETED" or not self.selected(pod):
                self.pods.pop(uid, None)
            else:
                self.pods[uid] = pod

    def snapshot(self, objects, initial):
        ids = {p["metadata"]["uid"] for p in objects}
        for pod in objects:
            self.pod_event("INITIAL" if initial else "RESYNC", pod, "list")
        # A missing Pod after 410 might be deleted or no longer match the selector.
        # Do not manufacture a DELETED event with a made-up deletion time.
        with self.lock:
            for uid in set(self.pods) - ids:
                old = self.pods.pop(uid)
                self.output.emit("pod-updates.jsonl", {
                    "type": "MISSING_AFTER_RESYNC", "observedAt": utcnow(),
                    "uid": uid, "pod": old["metadata"]["name"],
                    "namespace": old["metadata"]["namespace"],
                    "lastStatus": old.get("status", {}),
                    "writer": {"state": "unknown"},
                }, console=True)

    def event(self, kind, event):
        ref = event.get("involvedObject", {})
        if ref.get("kind") != "Pod":
            return
        with self.lock:
            # Preserve namespace Pod Events even if they arrive before the Pod.
            # Restrict by explicit pod names when possible; join by UID afterwards.
            if self.args.pod and ref.get("name") not in self.args.pod:
                return
        self.output.emit("events.jsonl", {
            "observedAt": utcnow(), "eventType": kind, "notProofOfStatusWriter": True,
            "object": event,
        })

    def audit(self, event):
        record = audit_record(event)
        if not record or record["namespace"] != self.args.namespace:
            return
        if self.args.pod and record["pod"] not in self.args.pod:
            return
        if record["writeEvidence"] == "no_response_pod_version":
            self.output.emit("audit-unmatched.jsonl", record)
            return
        changed, updates = self.index.add(record)
        if not changed:
            return
        self.output.emit("audit-writes.jsonl", record)
        for update in updates:
            if update[1] not in ("ADDED", "MODIFIED", "DELETED"):
                continue
            self.output.emit("attributions.jsonl", {
                "type": "ATTRIBUTION", "observedAt": utcnow(),
                "eventID": update[0], "uid": record["uid"],
                "resourceVersion": record["responseResourceVersion"],
                "namespace": record["namespace"], "pod": record["pod"],
                "writer": writer_result(self.index.candidates(
                    record["uid"], record["responseResourceVersion"])),
            }, console=True)


def collection_url(path, parameters):
    return path + ("?" + urlencode(parameters) if parameters else "")


def watch_collection(kube, stop, output, path, parameters, on_snapshot, on_event,
                     initial_list=None, on_gap=None):
    rv = None
    first = True
    backoff = 1
    while not stop.is_set():
        try:
            if rv is None:
                listed = initial_list if initial_list is not None else kube.get(
                    ["get", "--raw", collection_url(path, parameters)])
                initial_list = None
                on_snapshot(listed.get("items", []), first)
                first = False
                rv = listed["metadata"]["resourceVersion"]
            query = dict(parameters, watch="true", resourceVersion=rv,
                         allowWatchBookmarks="true", timeoutSeconds="30")
            expired = False
            with tempfile.TemporaryFile(mode="w+", encoding="utf-8") as errors:
                process = kube.spawn(
                    ["--request-timeout=40s", "get", "--raw", collection_url(path, query)],
                    stdout=subprocess.PIPE, stderr=errors, text=True,
                    encoding="utf-8", errors="replace", bufsize=1)
                try:
                    # Blocking readline in a dedicated thread avoids select/TextIO
                    # buffering losses when many events arrive in one network read.
                    for line in process.stdout:
                        if stop.is_set():
                            break
                        event = json.loads(line)
                        obj = event.get("object", {})
                        typ = event.get("type")
                        if typ == "ERROR":
                            if obj.get("code") == 410 or obj.get("reason") == "Expired":
                                expired = True
                                break
                            raise RuntimeError(json.dumps(obj, ensure_ascii=False))
                        if typ != "BOOKMARK":
                            on_event(typ, obj)
                        rv = obj.get("metadata", {}).get("resourceVersion", rv)
                        backoff = 1
                    if process.poll() is None and (expired or stop.is_set()):
                        process.terminate()
                    process.wait(timeout=5)
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.wait()
                    if process.stdout:
                        process.stdout.close()
                    kube.forget(process)
                errors.seek(0)
                error_text = errors.read()
                expired = expired or "too old resource version" in error_text.lower() or (
                    "410" in error_text and "gone" in error_text.lower())
                if expired:
                    output.diagnostic("WATCH_GAP: resourceVersion 已过期，将重新 LIST；中间变更不可恢复。",
                                      path=path, lastResourceVersion=rv)
                    if on_gap:
                        on_gap()
                    rv = None
                elif process.returncode and not stop.is_set():
                    raise RuntimeError(error_text.strip() or "watch stream exited unexpectedly")
            stop.wait(0.2)
        except InterruptedError:
            return
        except Exception as exc:
            if stop.is_set():
                return
            message = str(exc)
            output.diagnostic("watch 失败，将从已记录的 resourceVersion 重连。",
                              path=path, resourceVersion=rv, error=message)
            if any(word in message.lower() for word in ("forbidden", "unauthorized")):
                output.diagnostic("权限不足，停止此采集流。", path=path)
                return
            stop.wait(backoff)
            backoff = min(backoff * 2, 15)


def consume_audit_line(line, callback, output, path, line_number=None):
    try:
        value = json.loads(line)
        events = value.get("items", []) if value.get("kind") == "EventList" else [value]
        for event in events:
            callback(event)
    except (ValueError, TypeError, AttributeError) as exc:
        output.diagnostic("无法解析审计 JSONL 行，已跳过。",
                          file=str(path), line=line_number, error=str(exc))


def follow_audit(path, stop, output, callback):
    path = Path(path)
    if path.suffix == ".gz":
        with gzip.open(path, "rt", encoding="utf-8") as handle:
            for number, line in enumerate(handle, 1):
                if stop.is_set():
                    return
                consume_audit_line(line, callback, output, path, number)
        return
    handle, inode, pending = None, None, ""
    warned = False
    try:
        while not stop.is_set():
            try:
                stat = path.stat()
                identity = (stat.st_dev, stat.st_ino)
                if handle is None:
                    handle = path.open("r", encoding="utf-8")
                    inode = identity
                    warned = False
                line = handle.readline()
                if line:
                    pending += line
                    if pending.endswith("\n"):
                        consume_audit_line(pending, callback, output, path)
                        pending = ""
                    continue
                if identity != inode or stat.st_size < handle.tell():
                    output.diagnostic("审计文件轮转/截断，重新打开；如有缺失可事后导入归档文件。",
                                      file=str(path), partialLineDiscarded=bool(pending))
                    handle.close()
                    handle, pending = None, ""
                stop.wait(0.2)
            except OSError as exc:
                if not warned:
                    output.diagnostic("审计文件暂不可读，Pod watch 继续运行。",
                                      file=str(path), error=str(exc))
                    warned = True
                stop.wait(1)
    finally:
        if handle:
            handle.close()
        if pending.strip():
            consume_audit_line(pending, callback, output, path)


def scheduler_logs(kube, stop, output, namespace, pod, container):
    name = "scheduler-" + namespace + "-" + pod + ".log"
    args = ["logs", "-n", namespace, pod, "--follow", "--timestamps", "--since=5s"]
    if container:
        args += ["-c", container]
    with (output.directory / name).open("a", encoding="utf-8") as handle:
        process = kube.spawn(args, stdout=handle, stderr=subprocess.STDOUT)
        try:
            process.wait()
            if not stop.is_set():
                output.diagnostic("scheduler 日志流已结束；Pod 重建后需重新启动采集或另行补取日志。",
                                  pod=pod, returnCode=process.returncode)
        finally:
            kube.forget(process)


def export_enriched(directory, index, output_path):
    source = Path(directory) / "pod-updates.jsonl"
    count = 0
    with source.open(encoding="utf-8") as src, Path(output_path).open("w", encoding="utf-8") as dst:
        for line in src:
            record = json.loads(line)
            if record.get("type") == "POD":
                record["writer"] = writer_result(
                    index.candidates(record["uid"], record["resourceVersion"]),
                    record["eventType"] in ("ADDED", "MODIFIED", "DELETED"))
                count += 1
            dst.write(json.dumps(record, ensure_ascii=False) + "\n")
    return count


def run_watch(args):
    directory = Path(args.output)
    if directory.exists() and any(directory.iterdir()):
        raise ValueError("输出目录必须为空，以免混合不同集群/运行的 resourceVersion。")
    directory.mkdir(parents=True, exist_ok=True)
    stop = threading.Event()
    output = Output(directory, args.pretty)
    index = AuditIndex(directory / "audit-index.sqlite")
    kube = Kubectl(args, stop)
    collector = Collector(args, output, index, kube, stop)
    threads = []
    started = time.monotonic()

    def start(target, *params):
        def guarded():
            try:
                target(*params)
            except InterruptedError:
                pass
            except Exception as exc:
                output.diagnostic("采集线程结束。", collector=target.__name__, error=str(exc))
        thread = threading.Thread(target=guarded, name=target.__name__, daemon=True)
        threads.append(thread)
        thread.start()
        return thread

    def gap():
        with collector.lock:
            collector.counts["gaps"] += 1

    old_handlers = {}
    for sig in (signal.SIGINT, signal.SIGTERM):
        old_handlers[sig] = signal.signal(sig, lambda *_: stop.set())
    try:
        namespace_path = "/api/v1/namespaces/" + quote(args.namespace, safe="")
        params = {}
        if args.selector:
            params["labelSelector"] = args.selector
        if len(args.pod) == 1:
            params["fieldSelector"] = "metadata.name=" + args.pod[0]
        initial = kube.get(["get", "--raw", collection_url(namespace_path + "/pods", params)])
        # Record settings without copying kubeconfig credentials.
        output.emit("run.jsonl", {
            "startedAt": utcnow(), "namespace": args.namespace, "pods": args.pod,
            "selector": args.selector, "podGroup": args.pod_group,
            "context": args.context, "durationSeconds": args.duration,
            "auditFiles": args.audit_log, "rawPods": args.raw_pods,
            "attributionNote": "managedFields 与 Event 仅为线索；版本推进的审计记录用于确认 API 调用者。",
        })
        for path in args.audit_log:
            start(follow_audit, path, stop, output, collector.audit)
        main_watch = start(watch_collection, kube, stop, output,
                           namespace_path + "/pods", params, collector.snapshot,
                           collector.pod_event, initial, gap)
        if not args.no_events:
            start(watch_collection, kube, stop, output, namespace_path + "/events", {},
                  lambda items, first: [collector.event("INITIAL" if first else "RESYNC", e)
                                        for e in items], collector.event)
        if args.pod_group:
            pg_path = ("/apis/scheduling.volcano.sh/v1beta1/namespaces/"
                       + quote(args.namespace, safe="") + "/podgroups")
            def pg_event(kind, obj):
                output.emit("podgroups.jsonl", {"observedAt": utcnow(),
                                              "eventType": kind, "object": obj})
            start(watch_collection, kube, stop, output, pg_path,
                  {"fieldSelector": "metadata.name=" + args.pod_group},
                  lambda items, first: [pg_event("INITIAL" if first else "RESYNC", x)
                                        for x in items], pg_event)
        scheduler_pods = set(args.scheduler_pod)
        if args.scheduler_deployment:
            try:
                dep = kube.get(["get", "deployment", args.scheduler_deployment,
                                "-n", args.scheduler_namespace, "-o", "json"])
                match = dep["spec"]["selector"].get("matchLabels", {})
                if not match or dep["spec"]["selector"].get("matchExpressions"):
                    raise ValueError("此 Deployment selector 请改用 --scheduler-pod 指定")
                selector = ",".join(k + "=" + v for k, v in match.items())
                pods = kube.get(["get", "pods", "-n", args.scheduler_namespace,
                                 "-l", selector, "-o", "json"])
                output.emit("scheduler-pods.jsonl", pods)
                scheduler_pods.update(x["metadata"]["name"] for x in pods["items"]
                                      if not x["metadata"].get("deletionTimestamp"))
            except Exception as exc:
                output.diagnostic("无法发现 scheduler Pod；状态采集继续。", error=str(exc))
        for pod in sorted(scheduler_pods):
            start(scheduler_logs, kube, stop, output, args.scheduler_namespace,
                  pod, args.scheduler_container)
        output.diagnostic("开始只读采集；Ctrl-C 停止。componentHints 提供无审计的组件推测。"
                          "如接入审计，ATTRIBUTION 行会补充调用者证据。",
                          output=str(directory.resolve()))
        while not stop.wait(0.2):
            if args.duration and time.monotonic() - started >= args.duration:
                break
            if not main_watch.is_alive():
                raise RuntimeError("Pod watch 已停止；检查 diagnostics.jsonl，采集不再完整。")
    finally:
        stop.set()
        kube.terminate_all()
        for thread in threads:
            # All kubectl processes are stopped; file followers check stop per line.
            # Do not close output/index underneath a still-running callback.
            while thread.is_alive():
                thread.join(timeout=5)
                if thread.is_alive():
                    output.diagnostic("正在等待采集线程退出。", collector=thread.name)
        if (directory / "pod-updates.jsonl").exists():
            export_enriched(directory, index, directory / "pod-updates.enriched.jsonl")
        output.emit("summary.jsonl", {"finishedAt": utcnow(),
                                     "elapsedSeconds": round(time.monotonic() - started, 3),
                                     **collector.counts,
                                     "note": "无 watch 事件不代表无 no-op API 请求；410 后的中间版本不可恢复。"})
        index.close()
        output.close()
        for sig, handler in old_handlers.items():
            signal.signal(sig, handler)


def run_correlate(args):
    directory = Path(args.capture)
    if not (directory / "pod-updates.jsonl").exists():
        raise ValueError("capture 目录缺少 pod-updates.jsonl")
    output_path = Path(args.output) if args.output else directory / "pod-updates.enriched.jsonl"
    if output_path.resolve() == (directory / "pod-updates.jsonl").resolve():
        raise ValueError("不能覆盖原始 pod-updates.jsonl")
    output = Output(directory)
    index = AuditIndex(directory / "audit-index.sqlite")
    try:
        for filename in args.audit_log:
            opener = gzip.open if filename.endswith(".gz") else open
            with opener(filename, "rt", encoding="utf-8") as handle:
                for number, line in enumerate(handle, 1):
                    def add(event):
                        record = audit_record(event)
                        if record:
                            index.add(record)
                    consume_audit_line(line, add, output, filename, number)
        count = export_enriched(directory, index, output_path)
        print(json.dumps({"records": count, "output": str(output_path.resolve())},
                         ensure_ascii=False))
    finally:
        index.close()
        output.close()


def parser():
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(dest="command", required=True)
    watch = commands.add_parser("watch", help="记录每个 Pod 版本及完整 status（只读）")
    watch.add_argument("-n", "--namespace", required=True)
    watch.add_argument("--pod", action="append", default=[], help="可重复；与 selector/pod-group 取交集")
    watch.add_argument("-l", "--selector", help="Kubernetes label selector")
    watch.add_argument("--pod-group", help="按 scheduling.k8s.io/group-name 过滤，并记录此 PG")
    watch.add_argument("--kubeconfig")
    watch.add_argument("--context")
    watch.add_argument("--output", default="pod-status-capture-" + datetime.datetime.now().strftime("%Y%m%d-%H%M%S"))
    watch.add_argument("--duration", type=float, default=0, help="秒；0 表示持续到 Ctrl-C")
    watch.add_argument("--audit-log", action="append", default=[], help="本机审计 JSONL，可重复；持续追踪，.gz 仅读取一次")
    watch.add_argument("--no-events", action="store_true", help="不采集 namespace Pod Events")
    watch.add_argument("--raw-pods", action="store_true", help="额外保存完整 Pod，可能包含 spec/env/annotations")
    watch.add_argument("--pretty", action="store_true", help="终端美化 JSON；文件始终为 JSONL")
    watch.add_argument("--scheduler-pod", action="append", default=[], help="可重复；同步保存该 Pod 日志")
    watch.add_argument("--scheduler-deployment", help="例如 volcano-scheduler；发现当前副本并分别保存日志")
    watch.add_argument("--scheduler-namespace", default="volcano-system")
    watch.add_argument("--scheduler-container")
    correlate = commands.add_parser("correlate", help="结束采集后用审计文件补全更新者")
    correlate.add_argument("--capture", required=True)
    correlate.add_argument("--audit-log", required=True, action="append")
    correlate.add_argument("--output")
    policy = commands.add_parser("audit-policy", help="输出审计 policy 示例；不会修改集群")
    policy.add_argument("-n", "--namespace", required=True)
    return root


def main():
    args = parser().parse_args()
    try:
        if args.command == "watch":
            if args.duration < 0:
                raise ValueError("duration 不能为负数")
            run_watch(args)
        elif args.command == "correlate":
            run_correlate(args)
        else:
            print("# 将此规则合并到现有 policy 的宽泛匹配规则之前；不要直接覆盖已有审计配置。\n"
                  "apiVersion: audit.k8s.io/v1\nkind: Policy\nrules:\n"
                  "- level: RequestResponse\n"
                  "  namespaces: [" + json.dumps(args.namespace) + "]\n"
                  "  verbs: [create, update, patch, delete]\n"
                  "  resources:\n  - group: \"\"\n    resources: [pods, pods/status]\n"
                  "  omitStages: [RequestReceived]\n")
        return 0
    except (ValueError, RuntimeError, OSError) as exc:
        print("ERROR: " + str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
