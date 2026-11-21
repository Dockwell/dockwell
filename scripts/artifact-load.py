#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only

"""Fixtures and the HTTP part of the Docker artifact memory gate."""
import concurrent.futures
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import threading
import time
from urllib.parse import urlsplit

FRAME = 128 << 10
PART = 64 << 20


def add(tar, name, data):
    entry = tarfile.TarInfo(name)
    entry.size = len(data)
    tar.addfile(entry, io.BytesIO(data))


def fixture(work, mib):
    payload = work / "payload"
    layer = work / "layer.tar"
    with payload.open("wb") as file:
        file.truncate(mib << 20)
    with tarfile.open(layer, "w", format=tarfile.USTAR_FORMAT) as tar:
        tar.add(payload, arcname="payload")
    with layer.open("rb") as file:
        digest = hashlib.file_digest(file, "sha256").hexdigest()
    config = json.dumps({
        "architecture": "amd64", "os": "linux",
        "rootfs": {"type": "layers", "diff_ids": ["sha256:" + digest]},
    }).encode()
    name = hashlib.sha256(config).hexdigest() + ".json"
    with tarfile.open(work / "artifact.tar", "w", format=tarfile.USTAR_FORMAT) as tar:
        add(tar, name, config)
        add(tar, "manifest.json", json.dumps([{
            "Config": name, "Layers": ["layer.tar"], "RepoTags": [],
        }]).encode())
        tar.add(layer, arcname="layer.tar")
    payload.unlink()
    for valid in (True, False):
        with tarfile.open(work / ("many-image.tar" if valid else "many-files.tar"), "w", format=tarfile.USTAR_FORMAT) as tar:
            if valid:
                small = json.dumps({"architecture": "amd64", "os": "linux", "rootfs": {"type": "layers", "diff_ids": []}}).encode()
                config = hashlib.sha256(small).hexdigest() + ".json"
                add(tar, config, small)
                add(tar, "manifest.json", json.dumps([{"Config": config, "Layers": [], "RepoTags": []}]).encode())
            prefix = "p" * 148 + "/"
            for n in range(49_999):
                add(tar, prefix + "f" * 85 + f"{n:05d}", b"")



def exercise(work, origin, server, cookie, pid, max_mb):
    where = urlsplit(origin)
    path = f"/v1/docker/artifacts/{server}"
    source = work / "artifact.tar"
    progress = []

    def call(method, target, body=None, headers=None, expected=200, stream=False, wait_slot=False):
        conn = http.client.HTTPConnection(where.hostname, where.port, timeout=120)
        request_headers = {"Cookie": cookie, "Origin": origin, "Sec-Fetch-Site": "same-origin"}
        request_headers.update(headers or {})
        if isinstance(body, dict):
            body = json.dumps(body).encode()
            request_headers["Content-Type"] = "application/json"
        try:
            try:
                conn.request(method, target, body=body, headers=request_headers)
            except BrokenPipeError:
                if not wait_slot:
                    raise
            response = conn.getresponse()
            if wait_slot and response.status == 429 and response.getheader("Dockwell-Error") == "artifact_transfer_slots":
                return None
            if response.status != expected:
                raise RuntimeError(f"{method} {target}: {response.status} {response.read(4096)!r}")
            if stream:
                return hashlib.file_digest(response, "sha256").hexdigest()
            return json.loads(response.read() or b"{}")
        finally:
            conn.close()

    def upload(retry=False, archive=source, hold=None):
        size = archive.stat().st_size
        began = call("POST", path, {"kind": "DOCKER_ARTIFACT_KIND_IMAGE_IMPORT", "sizeBytes": str(size)})
        artifact = began["artifactId"]
        offset = 0
        digest = hashlib.sha256()
        with archive.open("rb") as file:
            while offset < size:
                length = min(size - offset, PART)
                url = f"{path}/{artifact}/bytes?offset={offset}"
                if retry and offset == PART:
                    call("PUT", url, b"x", {"Dockwell-Artifact-Prefix-SHA256": "wrong"}, expected=412)
                    status = call("GET", f"{path}/{artifact}")
                    if int(status["receivedBytes"]) != offset:
                        raise RuntimeError("a refused retry changed the committed offset")
                next_digest = digest.copy()
                file.seek(offset)

                def parts():
                    remaining = length
                    while remaining:
                        chunk = file.read(min(FRAME, remaining))
                        if not chunk:
                            raise RuntimeError("fixture ended")
                        remaining -= len(chunk)
                        if hold and offset == 0 and remaining == length - len(chunk):
                            hold[0].set()
                            if not hold[1].wait(45):
                                raise RuntimeError("the overlap gate timed out")
                        next_digest.update(chunk)
                        yield chunk

                out = call("PUT", url, parts(), {
                    "Content-Length": str(length), "Content-Type": "application/octet-stream",
                    "Dockwell-Artifact-Prefix-SHA256": digest.hexdigest(),
                }, wait_slot=True)
                if out is None:
                    time.sleep(1)
                    continue
                offset += length
                digest = next_digest
                if int(out["receivedBytes"]) != offset or out["committedPrefixSha256"] != digest.hexdigest():
                    raise RuntimeError("uploaded prefix did not match")
        return artifact

    def seal(artifact, valid=True):
        verified = call("POST", f"{path}/{artifact}/finish", {})
        if verified["state"] != "DOCKER_ARTIFACT_STATE_CHECKING":
            raise RuntimeError("Finish did not acknowledge the validation queue")
        until = time.monotonic() + 240
        while time.monotonic() < until:
            verified = call("GET", f"{path}/{artifact}")
            if verified["state"] == "DOCKER_ARTIFACT_STATE_READY" and valid:
                return verified
            if verified["state"] == "DOCKER_ARTIFACT_STATE_FAILED" and not valid:
                return verified
            if verified["state"] != "DOCKER_ARTIFACT_STATE_CHECKING":
                raise RuntimeError(f"unexpected validation state: {verified['state']}")
            time.sleep(0.05)
        raise RuntimeError("the archive validation timed out")

    def file_upload(hold):
        to = work / f"file-overlap-{pid}.bin"
        path = f"/v1/servers/{server}/uploads"
        size = min(PART, (work / "source.bin").stat().st_size)
        began = call("POST", path, {"path": str(to), "sizeBytes": str(size)})["upload"]["uploadId"]

        def parts():
            with (work / "source.bin").open("rb") as file:
                available = size
                while available:
                    data = file.read(min(FRAME, available))
                    available -= len(data)
                    yield data
                    if available == size - len(data):
                        hold[0].set()
                        if not hold[1].wait(45):
                            raise RuntimeError("the File overlap gate timed out")

        call("PUT", f"{path}/{began}?offset=0", parts(), {"Content-Length": str(size), "Content-Type": "application/octet-stream"})
        call("POST", f"{path}/{began}:finish", {})
        to.unlink()

    def apply(operation):
        made = call("POST", "/v1/docker/maintenance", {"serverIds": [server], **operation})["change"]
        change = made["id"]
        call("POST", f"/v1/changes/{change}:apply", {"fingerprint": made["plan"]["fingerprint"]})
        until = time.monotonic() + 240
        while time.monotonic() < until:
            got = call("GET", f"/v1/changes/{change}")["change"]
            if got["state"] == "CHANGE_STATE_APPLIED":
                return got
            if got["state"] == "CHANGE_STATE_FAILED":
                raise RuntimeError("reviewed archive operation failed")
            time.sleep(0.2)
        raise RuntimeError("reviewed archive operation timed out")

    def measure(label, run):
        started = time.monotonic()
        result = run()
        status = Path(f"/proc/{pid}/status").read_text()
        peak = int(next(line.split()[1] for line in status.splitlines() if line.startswith("VmHWM:"))) * 1024 / 1e6
        progress.append(peak)
        print(f"{label}, {time.monotonic() - started:.1f}s, VmHWM {peak:.1f} MB", flush=True)
        return result

    def probes(futures):
        while any(not future.done() for future in futures):
            started = time.monotonic()
            overview = call("GET", f"/v1/docker/objects?serverIds={server}&kind=DOCKER_OBJECT_KIND_CONTAINER")
            if not overview.get("objects"):
                raise RuntimeError("the container inventory did not answer during transfer")
            logs = call("GET", "/v1/services/transfer-probe/logs?pageSize=1")
            if not any(chunk.get("lines") for chunk in logs.get("chunks", [])):
                raise RuntimeError("the container log read did not answer during transfer")
            if time.monotonic() - started > 10:
                raise RuntimeError("the inventory and log reads took more than 10 seconds during transfer")
            time.sleep(0.5)
        return [f.result() for f in futures]

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        uploaded = measure("four artifact uploads", lambda: probes([pool.submit(upload) for _ in range(4)]))
        verified = seal(uploaded[0])
        image = verified["images"][0]["imageId"]
        apply({"load": {"artifactId": uploaded[0], "selectedImageIds": [image]}})
        got = apply({"export": {"sourceKey": {"serverId": server, "imageId": image}}})
        receipt = next(r for step in got["steps"] for r in step.get("dockerImageReceipts", []) if r.get("artifactId"))
        export = receipt["artifactId"]
        target = f"{path}/{export}/bytes"

        def download():
            if call("GET", target, stream=True) != receipt["sha256"]:
                raise RuntimeError("sealed export digest differs")

        measure("four artifact downloads", lambda: probes([pool.submit(download) for _ in range(4)]))
        mixed = measure("mixed artifact transfers with a retry", lambda: probes(
            [pool.submit(upload, True), pool.submit(upload), pool.submit(download), pool.submit(download)]
        ))
        def overlap():
            valid = upload(archive=work / "many-image.tar")
            go = threading.Event()
            file_started, artifact_started = threading.Event(), threading.Event()
            file_job = pool.submit(file_upload, (file_started, go))
            transfer_job = pool.submit(upload, False, source, (artifact_started, go))
            try:
                if not file_started.wait(15) or not artifact_started.wait(15):
                    raise RuntimeError("the two transfers did not reach their overlap gate")
                probes([pool.submit(seal, valid)])
            finally:
                go.set()
            _, transfer = probes([file_job, transfer_job])
            for item in (valid, transfer):
                call("POST", f"{path}/{item}:abort", {})

        measure("File upload, artifact upload and 49,999-member image Seal", overlap)

        def non_image():
            artifact = upload(archive=work / "many-files.tar")
            probes([pool.submit(seal, artifact, False)])
            call("POST", f"{path}/{artifact}:abort", {})

        measure("reject 49,999-member non-image tar", non_image)
        for artifact in [*uploaded[1:], *[item for item in mixed if item], export]:
            call("POST", f"{path}/{artifact}:abort", {})
    if max(progress) >= max_mb:
        raise RuntimeError(f"artifact transfers exceeded the {max_mb:g} MB gate")


if sys.argv[1] == "fixture":
    fixture(Path(sys.argv[2]), int(sys.argv[3]))
else:
    exercise(Path(sys.argv[2]), sys.argv[3], sys.argv[4], os.environ["DOCKWELL_LOAD_COOKIE"], int(sys.argv[5]), float(sys.argv[6]))
