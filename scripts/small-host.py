# SPDX-License-Identifier: AGPL-3.0-only
"""Single-host fixture and sampler. The only required services run on loopback."""

from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import math
import mmap
import os
from pathlib import Path
import re
import signal
import socket
import statistics
import subprocess
import sys
import threading
import time
from urllib.parse import urlencode, urlsplit


MIB = 1 << 20
LIMIT = 1024 * MIB
DOCKWELL_LIMIT = 250_000_000
ROOT = Path(__file__).resolve().parent.parent
APP_MIB = 104
HOST_MIB = 160
BASELINE_SECONDS = 60
APPS = ("storefront", "inventory", "reporting")


def allocate(mib):
    memory = mmap.mmap(-1, mib * MIB, flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS)
    for offset in range(0, mib * MIB, mmap.PAGESIZE):
        memory[offset] = 1
    return memory


class App(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        time.sleep(0.004)
        body = b"small-host app response\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


def fixture(role, mib):
    with allocate(mib):
        if role == "hold":
            print("ready", flush=True)
            signal.pause()
        else:
            with HTTPServer(("127.0.0.1", 0), App) as server:
                print(server.server_port, flush=True)
                server.serve_forever()


def until(what, read, seconds=35):
    stop = time.monotonic() + seconds
    while time.monotonic() < stop:
        value = read()
        if value:
            return value
        time.sleep(0.12)
    raise RuntimeError(f"timed out waiting for {what}")


def fields(path):
    try:
        pairs = (line.split() for line in path.read_text().splitlines())
        return {words[0].rstrip(":"): int(words[1]) for words in pairs
                if len(words) >= 2 and words[1].isdecimal()}
    except (FileNotFoundError, ProcessLookupError):
        return {}


def number(path):
    try:
        return int(path.read_text().strip())
    except (FileNotFoundError, ValueError):
        return None


def address(log, pattern, process):
    def read():
        if process.poll() is not None:
            raise RuntimeError(f"{log.stem} exited with {process.returncode}")
        match = re.search(pattern, log.read_text())
        return match.group(1) if match else None

    return until(log.stem, read)


def request(origin, method, path, cookie=None, data=None):
    where = urlsplit(origin)
    headers = {}
    if cookie:
        headers = {"Cookie": cookie, "Origin": origin, "Sec-Fetch-Site": "same-origin"}
    if isinstance(data, dict):
        headers["Content-Type"] = "application/json"
        data = json.dumps(data).encode()
    conn = HTTPConnection(where.hostname, where.port, timeout=35)
    try:
        conn.request(method, path, data, headers)
        response = conn.getresponse()
        body = response.read()
        if response.status >= 300:
            raise RuntimeError(f"{method} {path}: HTTP {response.status} {body[:500]!r}")
        return body
    finally:
        conn.close()


class UnixHTTP(HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(5)
        self.sock.connect(self.host)


def engine(sock, path):
    conn = UnixHTTP(str(sock))
    try:
        conn.request("GET", path, headers={"Host": "localhost"})
        response = conn.getresponse()
        if response.status != 200:
            raise RuntimeError(f"fake Engine {path}: HTTP {response.status}")
        return json.load(response)
    finally:
        conn.close()


def descendants(pid):
    yield pid
    for task in Path(f"/proc/{pid}/task").glob("*/children"):
        try:
            children = task.read_text().split()
        except (FileNotFoundError, ProcessLookupError):
            continue
        for child in children:
            yield from descendants(int(child))


def smaps(pid):
    return fields(Path(f"/proc/{pid}/smaps_rollup"))


def tally(values, key):
    return max(values.get(key, 0), 0) * 1024


class Meter:
    def __init__(self, mode, work):
        self.mode = mode
        self.path = None
        if mode == "cgroup":
            group = next(line[4:] for line in Path("/proc/self/cgroup").read_text().splitlines()
                         if line.startswith("0::/"))
            self.group = "/" + group
            self.path = Path("/sys/fs/cgroup") / group
            if number(self.path / "memory.max") != LIMIT or number(self.path / "memory.swap.max") != 0:
                raise RuntimeError("workload did not enter the 1 GiB cgroup with swap disabled")
        self.proc = work / "build-proc"
        self.proc.mkdir()
        self.ctrl = None
        self.samples = []
        self.stop = threading.Event()
        self.error = None
        self.host_oom_start = fields(Path("/proc/vmstat")).get("oom_kill")
        self.thread = threading.Thread(target=self.measure, daemon=True)

    def sample(self):
        ctrl = self.ctrl
        proc = smaps(ctrl) if ctrl else {}
        memory = {
            "rss_bytes": tally(proc, "Rss"),
            "pss_bytes": tally(proc, "Pss"),
            "anonymous_rss_bytes": tally(proc, "Anonymous"),
            "file_backed_rss_bytes": tally(proc, "Rss") - tally(proc, "Anonymous"),
            "anonymous_pss_bytes": tally(proc, "Pss_Anon"),
            "file_pss_bytes": tally(proc, "Pss_File"),
            "shmem_pss_bytes": tally(proc, "Pss_Shmem"),
            "swap_pss_bytes": tally(proc, "SwapPss"),
        }
        processes = [smaps(pid) for pid in set(descendants(os.getpid()))]
        resident = sum(tally(item, "Pss") for item in processes)
        swap = sum(tally(item, "SwapPss") for item in processes)
        if self.path:
            swap = max(swap, number(self.path / "memory.swap.current") or 0)
        else:
            # Match the cgroup admission model by leaving mapped file pages
            # reclaimable. The separate peak gate counts all resident PSS.
            working = sum(tally(item, "Pss_Anon") + tally(item, "Pss_Shmem")
                          for item in processes)
            temp = self.proc / "sample"
            temp.write_text(f"MemTotal: {LIMIT // 1024} kB\n"
                            f"MemAvailable: {max(0, LIMIT - working) // 1024} kB\n")
            temp.replace(self.proc / "meminfo")
        scope = number(self.path / "memory.current") if self.path else None
        self.samples.append((memory, resident, swap, scope))

    def measure(self):
        try:
            while not self.stop.is_set():
                self.sample()
                self.stop.wait(0.25)
        except Exception as error:
            self.error = str(error)

    def result(self):
        self.stop.set()
        if self.thread.is_alive():
            self.thread.join()
        try:
            self.sample()
        except Exception as error:
            self.error = str(error)
        units = [item[0] for item in self.samples if item[0]["rss_bytes"]]
        memory = {}
        if units:
            memory = {
                "peak": {key: max(sample[key] for sample in units) for key in units[0]},
                "settled": units[-1],
                "kernel_rss_peak_bytes": tally(fields(Path(f"/proc/{self.ctrl}/status")), "VmHWM"),
            }
            memory["rss_peak_bytes"] = max(memory["kernel_rss_peak_bytes"], memory["peak"]["rss_bytes"])
        resident = max((sample[1] or 0 for sample in self.samples), default=0)
        swap = [sample[2] for sample in self.samples if sample[2] is not None]
        events, peak, swap_peak = {}, None, None
        if self.path:
            events = fields(self.path / "memory.events")
            peak = max(number(self.path / "memory.peak") or 0,
                       *(item[3] or 0 for item in self.samples))
            swap_peak = number(self.path / "memory.swap.peak")
        host_oom_end = fields(Path("/proc/vmstat")).get("oom_kill")
        return {
            "mode": self.mode, "limit_bytes": LIMIT,
            "peak_bytes": resident,
            "peak_source": "sampled process-tree PSS",
            "cgroup_peak_bytes": peak,
            "cgroup_memory_events": events,
            "swap_peak_bytes": max(swap_peak or 0, *swap) if swap else swap_peak,
            "oom_kills": events.get("oom_kill"),
            "host_oom_kill_delta": host_oom_end - self.host_oom_start
            if host_oom_end is not None and self.host_oom_start is not None else None,
            "control_with_embedded_agent": memory,
        }


class Stand:
    def __init__(self, work, full, caddy, meter):
        self.work = work
        self.full = full
        self.caddy = caddy
        self.meter = meter
        self.procs = []
        self.logs = {}
        self.sock = work / "docker/docker.sock"
        self.env = {k: v for k, v in os.environ.items()
                    if not k.startswith(("DOCKWELL_", "DOCKER_", "FAKEDOCKER_"))}
        memory = re.search(r"^MEMORY_LIMIT=(\S+)", (ROOT / "scripts/install.sh").read_text(), re.M)[1]
        self.env.update(
            DOCKWELL_TEST="1", DOCKWELL_DATA_DIR=str(work / "data"),
            DOCKWELL_TEST_HOSTNAME="small-host-fixture",
            DOCKER_HOST=f"unix://{self.sock}", FAKEDOCKER_ROOT=str(self.sock.parent),
            XDG_CONFIG_HOME=str(work / "config"), XDG_DATA_HOME=str(work / "local"),
            TMPDIR=str(work),
            GOMEMLIMIT=memory, GOMAXPROCS="2", NO_PROXY="localhost,127.0.0.1",
            no_proxy="localhost,127.0.0.1",
        )
        if meter.mode == "sampled":
            self.env["DOCKWELL_TEST_BUILD_PROC"] = str(meter.proc)
        else:
            self.env["DOCKWELL_TEST_BUILD_CGROUP"] = meter.group

    def start(self, name, *args, stdout_log=False, **env):
        dest = self.work / f"{name}.log"
        with dest.open("wb") as log:
            proc = subprocess.Popen(args, env=self.env | env, stderr=log,
                                    stdout=log if stdout_log else subprocess.DEVNULL, start_new_session=True)
        self.procs.append(proc)
        self.logs[name] = dest
        return proc

    def run(self, *args, **env):
        result = subprocess.run(args, env=self.env | env, capture_output=True, timeout=100)
        out = result.stdout.decode()
        if result.returncode:
            raise RuntimeError(f"{Path(args[0]).name} {args[1]} failed: "
                               f"{result.stderr.decode()[-1200:]} {out[-600:]}")
        return out

    def cli(self, *args):
        return self.run(self.full, *args)

    def get(self, path):
        return json.loads(request(self.origin, "GET", path, self.cookie))

    def post(self, path, data):
        return json.loads(request(self.origin, "POST", path, self.cookie, data))

    def panel(self, service, method, data=None):
        query = urlencode({"connect": "v1", "encoding": "json",
                           "message": json.dumps(data or {}, separators=(",", ":"))})
        return self.get(f"/dockwell.v1.{service}/{method}?{query}")

    def app_state(self):
        containers = engine(self.sock, "/v1.43/containers/json?all=1")
        ids = {}
        for item in containers:
            label = item["Labels"].get("com.docker.compose.service")
            if label not in APPS:
                continue
            data = engine(self.sock, f"/v1.43/containers/{item['Id']}/json")
            state = data["State"]
            if label in ids or not state["Running"] or state.get("OOMKilled", False):
                raise RuntimeError(f"{label} did not keep one running container")
            ids[label] = (item["Id"], state["StartedAt"], data.get("RestartCount", 0))
        if sorted(ids) != sorted(APPS):
            raise RuntimeError(f"expected the three app containers, saw {sorted(ids)}")
        return ids

    def fixture(self, name, role, mib):
        proc = self.start(name, sys.executable, __file__, role, str(mib), stdout_log=True)
        line = address(self.logs[name], r"^(\d+|ready)\s*$", proc)
        return proc, line

    def serve_caddy(self):
        routes = []
        self.apps = {}
        for name in APPS:
            proc, port = self.fixture(name, "app", APP_MIB)
            self.apps[name] = proc
            routes.append(f"reverse_proxy /{name} 127.0.0.1:{port}")
        # Caddy's :0 appears in its logs as :0. Bind a loopback port, then hand it
        # to Caddy and fail if another process won the race for it.
        with socket.socket() as reserved:
            reserved.bind(("127.0.0.1", 0))
            port = reserved.getsockname()[1]
        config = self.work / "Caddyfile"
        config.write_text("{\nadmin off\nauto_https off\n}\n"
                          f"http://127.0.0.1:{port} {{\nbind 127.0.0.1\n" + "\n".join(routes) + "\n}\n")
        self.proxy = f"http://127.0.0.1:{port}"
        proxy = self.start("caddy", self.caddy, "run", "--config", config, "--adapter", "caddyfile")
        address(self.logs["caddy"], r'(serving initial configuration)', proxy)

    def git_fixture(self):
        # Reuse gitserve's real Git HTTP backend with a tiny pre-seeded repository.
        base = self.work / "git"
        base.mkdir()
        repo = base / "web.git"
        source = base / "source"
        self.run("git", "init", "-q", "-b", "main", source)
        self.run("git", "init", "-q", "--bare", "-b", "main", repo)
        (source / "Dockerfile").write_text("FROM scratch\nRUN echo building && sleep 45\nCMD [\"serve\"]\n")
        self.run("git", "-C", source, "add", "Dockerfile")
        identity = dict(GIT_AUTHOR_NAME="Fixture", GIT_AUTHOR_EMAIL="fixture@example.invalid",
                        GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL="fixture@example.invalid")
        self.run("git", "-C", source, "commit", "-qm", "Small-host build fixture", **identity)
        self.run("git", "-C", source, "push", "-q", repo, "main")
        git = self.start("gitserve", self.work / "gitserve", base, stdout_log=True)
        self.git_url = address(self.logs["gitserve"], r"(http://127\.0\.0\.1:\d+)", git) + "/web.git"

    def setup(self):
        self.fixture("host-working-set", "hold", HOST_MIB)
        fake = self.start("fake-engine", self.work / "fakedocker", self.sock.parent,
                          stdout_log=True, FAKEDOCKER_LOG_LOAD="127.0.0.1:0")
        self.log_origin = "http://" + address(self.logs["fake-engine"], r"log fixture listening on (127\.0\.0\.1:\d+)", fake)
        self.git_fixture()
        self.serve_caddy()
        ctrl = self.start("dockwell", self.full, "run", "--listen", "127.0.0.1:0",
                          "--agent-listen", "127.0.0.1:0", "--log-format", "json")
        self.meter.ctrl = ctrl.pid
        address(self.logs["dockwell"], r'"listener":"control plane","addr":"(127\.0\.0\.1:\d+)"', ctrl)
        claim = re.search(r"http://localhost:\d*/claim#[A-Za-z0-9_-]+", self.cli("claim"))[0]
        cookie_path = self.work / "cookie"
        self.run(self.work / "claim", claim, cookie_path)
        self.cookie = cookie_path.read_text().strip()
        self.origin = claim.split("/claim#", 1)[0]
        self.cli("login", "--local")
        own = until("the embedded agent", lambda: next((s["id"] for s in
                    self.get("/v1/servers").get("servers", []) if s.get("controlServer")), None))
        self.server = own
        for volume in ("archive-data", "db-data"):
            self.cli("volumes", "create", volume, "--on", own, "--yes")
        for volume in engine(self.sock, "/v1.43/volumes")["Volumes"]:
            mount = Path(volume["Mountpoint"])
            name = volume["Name"].split("_")[-1]
            if name not in ("archive-data", "db-data"):
                continue
            dest = mount / ("data.sql" if name == "db-data" else "orders.bin")
            with dest.open("wb") as data:
                for _ in range(8):
                    data.write(os.urandom(MIB))
        for name in APPS:
            image = "postgres:17" if name == "reporting" else "fixture/small-host:prints"
            args = ("--app-mounts", '{"volume":"db-data","target":"/var/lib/postgresql/data"}') if name == "reporting" else ()
            self.cli("services", "create", name, "--on", own, "--app-image", image, *args, "--yes")
        self.before = self.app_state()
        volumes = self.get("/v1/resources?kind=volume")["resources"]
        self.volumes = {v["resource"]["name"]: v["id"] for v in volumes}
        services = self.get("/v1/services")["services"]
        self.service = next(s["id"] for s in services if s["resource"]["name"] == APPS[0])
        self.cli("services", "create", "pressure-build", "--on", own,
                 "--app-image", "fixture/small-host:prints", "--yes")
        self.source = self.work / "payload.bin"
        with self.source.open("wb") as payload:
            for _ in range(8):
                payload.write(os.urandom(MIB).replace(b":", b"_"))

    def build_plan(self, name, action="create"):
        planned = json.loads(self.cli("services", action, name, "--on", self.server,
                                     "--build-git-url", self.git_url, "--build-git-ref", "main",
                                     "--build-builder", "dockerfile", "--json", "--plan-only"))
        return planned["change"]["id"]

    def start_build(self, change):
        return self.post(f"/v1/changes/{change}:apply", {})

    def build_record(self, name):
        return next(iter(self.get(f"/v1/services/{name}/builds").get("builds", [])), None)

    def backup(self, name):
        result = self.post("/v1/backups", {"volumeId": self.volumes[name], "server": self.server})
        path = f"/v1/backups/{result['backup']['id']}"
        def read():
            backup = self.get(path)["backup"]
            return backup if backup["state"] != "BACKUP_STATE_RUNNING" else None
        backup = until(f"{name} backup", read, 65)
        if backup["state"] != "BACKUP_STATE_DONE":
            raise RuntimeError(f"{name} backup failed: {backup}")
        return backup

    def stop_all(self):
        for proc in reversed(self.procs):
            if proc.poll() is None:
                try:
                    os.killpg(proc.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
        for proc in reversed(self.procs):
            try:
                proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()


def traffic(origin, app, seconds, stop):
    where = urlsplit(origin)
    conn = HTTPConnection(where.hostname, where.port, timeout=3)
    samples = []
    finish = time.monotonic() + seconds
    try:
        while time.monotonic() < finish and not stop.is_set():
            started = time.monotonic()
            conn.request("GET", "/" + app)
            response = conn.getresponse()
            if response.status != 200 or response.read() != b"small-host app response\n":
                raise RuntimeError(f"Caddy could not reach {app}: HTTP {response.status}")
            samples.append((time.monotonic() - started) * 1000)
            stop.wait(max(0, 0.1 - (time.monotonic() - started)))
    finally:
        conn.close()
    if not samples:
        raise RuntimeError(f"no latency samples for {app}")
    return {"requests": len(samples), "p50_ms": statistics.median(samples),
            "p99_ms": sorted(samples)[math.ceil(len(samples) * .99) - 1]}


def panel_reads(stand, stop):
    rounds = 0
    searches = 0
    while not stop.is_set():
        stand.panel("ServerService", "ListServers")
        stand.panel("ServiceService", "ListServices")
        stand.panel("BackupService", "ListBackups")
        stand.panel("ServerService", "GetServer", {"id": stand.server})
        page = stand.panel("LogService", "ReadLogs",
                           {"id": stand.service, "query": "products", "pageSize": 200})
        if int(page.get("searchedLines", 0)) > 0:
            searches += 1
        rounds += 1
        stop.wait(2)
    return {"panel_rounds": rounds, "log_searches": searches}


def wait_time(record):
    if not record.get("startTime"):
        return None
    start, queued = (datetime.fromisoformat(record[key]) for key in ("startTime", "createTime"))
    return max(0, (start - queued).total_seconds())


def mixed(stand, seconds, report):
    good_change = stand.build_plan("mixed-build")
    pressure_change = stand.build_plan("pressure-build", "update")
    stop = threading.Event()
    with ThreadPoolExecutor(max_workers=6) as pool:
        baseline = [pool.submit(traffic, stand.proxy, app, BASELINE_SECONDS, stop) for app in APPS]
        report["before"] = {app: job.result() for app, job in zip(APPS, baseline)}
        during = [pool.submit(traffic, stand.proxy, app, seconds, stop) for app in APPS]
        try:
            stand.start_build(good_change)
            def started():
                build = stand.build_record("mixed-build")
                if build and build["state"] == "BUILD_STATE_FAILED":
                    raise RuntimeError(f"build failed before mixed load: {build.get('error')}")
                return build if build and build.get("startTime") else None
            first = until("the image build to start", started, 75)
            report["build"] = {"wait_seconds": wait_time(first), "state": first["state"]}
            builder = stand.fixture("build-working-set", "hold", 128)[0]
            follow = stand.start("follow", stand.full, "logs", "follow", stand.service,
                                 "--tail", "-1", "--json", stdout_log=True)
            until("a live log follow", lambda: int(request(stand.log_origin, "GET", "/followers")))
            request(stand.log_origin, "POST", "/rate?lines=60&size=160")
            panel = pool.submit(panel_reads, stand, stop)
            transfers = [(stand.work / f"upload-{i}.bin") for i in range(4)]
            uploads = [stand.start(f"upload-{i}", ROOT / "scripts/load-files.sh", stand.origin,
                                   stand.work / "cookie", stand.server, stand.source, target,
                                   FILE_LOAD_RATE="512k") for i, target in enumerate(transfers)]
            def uploading():
                if any(job.poll() is not None for job in uploads):
                    raise RuntimeError("a file transfer ended before the backup load started")
                return all(target.with_suffix(".bin.started").exists() for target in transfers)
            until("four file transfers to start", uploading)
            backups = [pool.submit(stand.backup, name) for name in ("archive-data", "db-data")]
            report["backups"] = [{k: backup[k] for k in ("state", "method", "database", "sizeBytes") if k in backup}
                                 for job in backups for backup in [job.result()]]
            if [item.get("method") for item in report["backups"]] != ["BACKUP_METHOD_COPY", "BACKUP_METHOD_DUMP"]:
                raise RuntimeError(f"the two backups used unexpected methods: {report['backups']}")
            if any(int(item.get("sizeBytes", 0)) < 4 * MIB for item in report["backups"]):
                raise RuntimeError("a backup did not include its seeded data")
            report["uploads"] = sum(job.wait(timeout=45) == 0 for job in uploads)
            if report["uploads"] != 4:
                raise RuntimeError("a file transfer did not finish with matching bytes")
            def ended():
                build = stand.build_record("mixed-build")
                return build if build and build.get("endTime") else None
            completed = until("the mixed build to finish", ended, 65)
            report["build"]["state"] = completed["state"]
            builder.terminate()
            builder.wait(timeout=5)
            if completed["state"] != "BUILD_STATE_DONE":
                raise RuntimeError(f"mixed build failed: {completed.get('error')}")

            # A second build must refuse before asking the Engine to run a builder.
            # This leaves less than 256 MiB free through the whole admission wait.
            lines = stand.get(f"/v1/builds/{completed['id']}/log").get("lines", [])
            if not any("Memory is tight" in line["text"] for line in lines):
                raise RuntimeError("the admitted build did not report tight memory")
            pressure = stand.fixture("pressure", "hold", 200)[0]
            try:
                time.sleep(.6)  # allow the fallback to publish this sample
                start = time.monotonic()
                stand.start_build(pressure_change)
                saw_wait = False

                def refused():
                    nonlocal saw_wait
                    record = stand.build_record("pressure-build")
                    if record and not record.get("endTime") and "memoryAvailableMib" in record:
                        saw_wait = True
                    return record if record and (record.get("startTime") or record.get("endTime")) else None
                record = until("the pressure build's admission decision", refused, 75)
                elapsed = time.monotonic() - start
                available = int(record.get("memoryAvailableMib", 256))
                clean = (saw_wait and elapsed >= 59 and record["state"] == "BUILD_STATE_FAILED"
                         and not record.get("startTime") and record.get("errorReason") == "build_memory"
                         and 0 <= available < 256)
                report["refusal"] = {"clean": clean, "seconds": elapsed, "wait_state_seen": saw_wait,
                                     "available_mib": available, "reason": record.get("errorReason"),
                                     "error": record.get("error"), "state": record["state"]}
                if not clean:
                    raise RuntimeError(f"the pressure build did not refuse cleanly: {record}")
            finally:
                pressure.terminate()
                pressure.wait(timeout=5)
            report["during"] = {app: job.result() for app, job in zip(APPS, during)}
        finally:
            stop.set()
        report.update(panel.result())
    with stand.logs["follow"].open("rb") as stream:
        if follow.poll() is not None or not any(b"products" in line for line in stream):
            raise RuntimeError("the log follower did not stay connected and receive live lines")
    report["log_follow"] = True
    follow.terminate()
    follow.wait(timeout=5)
    request(stand.log_origin, "POST", "/rate?lines=0")
    time.sleep(10)


def show(report):
    def size(value):
        return f"{value / MIB:.1f}" if value is not None else "unavailable"

    memory = report.get("memory", {})
    dockwell = memory.get("control_with_embedded_agent", {})
    print(f"| {report['mode']} | Result |")
    print("| --- | --- |")
    print(f"| Process-tree PSS peak / swap peak (MiB) | {size(memory.get('peak_bytes'))} / {size(memory.get('swap_peak_bytes'))} |")
    if report["mode"] == "cgroup":
        events = memory.get("cgroup_memory_events", {})
        print(f"| Kernel cgroup peak (MiB) / max / OOM events | {size(memory.get('cgroup_peak_bytes'))} / "
              f"{events.get('max', 'unavailable')} / {events.get('oom', 'unavailable')} |")
    print(f"| Scope OOM kills | {memory.get('oom_kills') if memory.get('oom_kills') is not None else 'unavailable'} |")
    print(f"| Control + embedded agent peak / settled RSS (MiB) | "
          f"{size(dockwell.get('rss_peak_bytes'))} / {size(dockwell.get('settled', {}).get('rss_bytes'))} |")
    for phase in ("peak", "settled"):
        row = dockwell.get(phase, {})
        if row:
            values = [size(row.get(key + "_bytes")) for key in
                      ("pss", "anonymous_rss", "file_backed_rss")]
            print(f"| {phase.capitalize()} PSS / anonymous RSS / file-backed RSS (MiB) | {' / '.join(values)} |")
    if report.get("build"):
        build = report["build"]
        refusal = report.get("refusal", {})
        print(f"| Build / wait for admission / clean refusal | {build['state']} / "
              f"{build['wait_seconds']:.3f} s / {refusal.get('clean', 'unavailable')} |")
    for app, before in report.get("before", {}).items():
        during = report.get("during", {}).get(app)
        if during:
            print(f"| {app} p50 / p99 before → during (ms) | "
                  f"{before['p50_ms']:.2f} / {before['p99_ms']:.2f} → "
                  f"{during['p50_ms']:.2f} / {during['p99_ms']:.2f} |")
    print(f"| App restarts | {report.get('app_restarts', 'unavailable')} |")
    print(f"| Result | {'FAIL' if report['failures'] else 'PASS'} |")


def run(work, full, caddy, out, duration, mode):
    report = {"mode": mode, "load_seconds": duration, "app_resident_target_mib": APP_MIB * len(APPS),
              "host_resident_target_mib": HOST_MIB, "baseline_seconds": BASELINE_SECONDS,
              "admission_source": "harness meminfo" if mode == "sampled" else "fake Engine cgroup",
              "docker": "fake", "starting_load_average": os.getloadavg(), "failures": []}
    meter = Meter(mode, work)
    stand = Stand(work, full, caddy, meter)
    meter.thread.start()
    try:
        stand.setup()
        mixed(stand, duration, report)
        after = stand.app_state()
        report["app_restarts"] = sum(stand.before[name] != after[name] or
                                     stand.apps[name].poll() is not None for name in APPS)
    except KeyboardInterrupt:
        report["failures"].append("run interrupted")
    except Exception as error:
        report["failures"].append(f"{type(error).__name__}: {error}")
    finally:
        try:
            memory = meter.result()
            report["memory"] = memory
            exceeded = {
                "the sampler failed": meter.error,
                "the 1 GiB memory limit was exceeded": memory["peak_bytes"] > LIMIT,
                "the run used swap": memory["swap_peak_bytes"] is None or memory["swap_peak_bytes"] > 0,
                "the cgroup hit its memory limit, saw an OOM, or lost its counters":
                    mode == "cgroup" and any(memory["cgroup_memory_events"].get(event) != 0
                                             for event in ("max", "oom", "oom_kill")),
                "Dockwell's RSS reached 250 MB or its process disappeared":
                    not 0 < memory["control_with_embedded_agent"].get("rss_peak_bytes", 0) < DOCKWELL_LIMIT or
                    not fields(Path(f"/proc/{meter.ctrl}/status")),
                "an app restarted": report.get("app_restarts", 0) != 0,
                "the panel read or log search did not complete":
                    "during" in report and not (report.get("panel_rounds") and report.get("log_searches")),
            }
            report["failures"].extend(why for why, failed in exceeded.items() if failed)
            for app, result in report.get("during", {}).items():
                baseline = report["before"][app]["p99_ms"]
                if result["p99_ms"] > max(3 * baseline, baseline + 25):
                    report["failures"].append(f"{app} p99 during load exceeded its latency budget")
        finally:
            stand.stop_all()
        show(report)
        out.write_text(json.dumps(report, indent=2) + "\n")
        if report["failures"]:
            for error in report["failures"]:
                print(f"small-host: {error}", file=sys.stderr)
            for name, log in stand.logs.items():
                if name in ("dockwell", "fake-engine", "gitserve", "caddy", "follow") or name.startswith("upload-"):
                    with log.open("rb") as file:
                        file.seek(max(0, log.stat().st_size - 2500))
                        print(file.read().decode(errors="replace").strip(), file=sys.stderr)
    return int(bool(report["failures"]))


if __name__ == "__main__":
    if len(sys.argv) == 3:
        fixture(sys.argv[1], int(sys.argv[2]))
    else:
        signal.signal(signal.SIGTERM, signal.default_int_handler)
        directory, binary, caddy_binary, output = map(Path, sys.argv[1:5])
        sys.exit(run(directory, binary, caddy_binary, output, int(sys.argv[5]), sys.argv[6]))
