#!/usr/bin/env python3
"""Bounded production-daemon correctness probes; use external memory/runtime limits."""
import argparse
import base64
import concurrent.futures
import hashlib
import http.client
import http.server
import json
import os
import pathlib
import secrets
import signal
import socket
import socketserver
import statistics
import struct
import subprocess
import threading
import time


ROOT = pathlib.Path(__file__).resolve().parents[3]
DATAPLANE = ROOT / "cmd/racer-dataplane"


def emit(root, events, event, **values):
    values.update(event=event, time=time.monotonic())
    events.append(values)
    (root / "results.json").write_text(json.dumps(events, indent=2))
    print(json.dumps(values), flush=True)


def ports(count):
    # Reserve the whole set together so ephemeral allocation cannot repeat a port.
    sockets = [socket.socket() for _ in range(count)]
    try:
        for stream in sockets:
            stream.bind(("127.0.0.1", 0))
        return [stream.getsockname()[1] for stream in sockets]
    finally:
        for stream in sockets:
            stream.close()


def publish(path, snapshot):
    pending = path.with_suffix(".next")
    pending.write_text(json.dumps({"snapshot": snapshot}))
    pending.replace(path)


def client_path(root, port):
    return str(root / f"client-{port}")


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout=8):
        super().__init__("localhost", timeout=timeout)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        stream, _ = super().get_request()
        # Unix clients have no peer IP. Retain a distinct identity for pool assertions.
        return stream, (id(stream), 0)

    def server_close(self):
        super().server_close()
        pathlib.Path(self.server_address).unlink(missing_ok=True)


def local_snapshot(root, listeners, origin_paths):
    return {
        "universe": base64.b64encode(bytes([1]) * 32).decode(),
        "node": base64.b64encode(bytes([2]) * 32).decode(),
        "revision": "1", "epoch": "1",
        "volumes": [{"id": f"v{i}",
                     "clientSocket": client_path(root, port),
                     "originSocket": str(origin_paths[i]),
                     "cacheGeneration": "1", "peerEndpoints": {},
                     "topology": {"epoch": "1", "slotCount": 1, "localSlots": [0]}}
                    for i, port in enumerate(listeners)],
    }


def private_json(path, value):
    with path.open("x") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(value, stream)


class ControlFixture:
    """Loopback CA/enrollment plus protobuf control, using the shared Go schema."""

    def __init__(self, args):
        self.root = args.output / "credentials"
        self.root.mkdir(mode=0o700)
        self.process = None
        binary = args.output / "controlfixture"
        with (args.output / "controlfixture-build.log").open("w") as log:
            subprocess.run(["go", "build", "-o", str(binary),
                            "./e2e/racer/scripts/controlfixture"], cwd=ROOT,
                           stdout=log, stderr=subprocess.STDOUT,
                           timeout=args.build_timeout, check=True)
        with (args.output / "controlfixture.log").open("w") as log:
            self.process = subprocess.Popen([str(binary), "--dir", str(self.root)],
                                            stdout=log, stderr=log, start_new_session=True)
        try:
            deadline = time.monotonic() + 10
            while not (self.root / "url").exists():
                assert self.process.poll() is None, (args.output / "controlfixture.log").read_text()
                assert time.monotonic() < deadline, "control fixture startup timeout"
                time.sleep(.05)
            self.url = (self.root / "url").read_text()
        except BaseException:
            self.close()
            raise

    def register(self, config, universe, node, pod_uid):
        token = secrets.token_hex(32)
        private_json(self.root / f"{node}.json",
                     dict(universe=universe, node=node, podUID=pod_uid,
                          token=token, config=str(config)))
        token_path = self.root / f"{node}.token"
        with token_path.open("x") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(token)
        return dict(RACER_CONTROL_PLANE_URL=f"{self.url}/v4/config",
                    RACER_TLS_TRUST_DIR=str(self.root),
                    RACER_ENROLL_URL=f"{self.url}/v3/enroll",
                    RACER_TRUST_PROOF_URL=f"{self.url}/v3/proof",
                    RACER_CONTROL_SERVER_NAME="localhost",
                    RACER_CONTROL_TOKEN_FILE=str(token_path),
                    RACER_POD_NAMESPACE="probe", RACER_POD_NAME=node, RACER_POD_UID=pod_uid)

    def close(self):
        if self.process:
            assert stop(self.process) == 0, "control fixture failed"


def launch(args, events, config, metrics_port, name="daemon", universe="01" * 32,
           node="02" * 32, cpus=None, pod_uid="probe-daemon", management_host="127.0.0.1"):
    # Do not inherit credential, RDMA, lifecycle or topology settings from a user's shell.
    env = {key: value for key, value in os.environ.items() if not key.startswith("RACER_")}
    overrides = dict(
        RACER_UNIVERSE=universe, RACER_NODE=node,
        RACER_SLAB_PATH=str(args.output / f"{name}.slab"),
        RACER_SLAB_SIZE=str(64 * 1024 * 1024), RACER_SHARDS="1", RACER_IO_WORKERS="1",
        RACER_COMPUTE_WORKERS="1", RACER_BUFFERS_PER_NODE="8",
        RACER_METRICS_ADDR=f"{management_host}:{metrics_port}", RACER_RDMA_MODE="disabled",
    )
    overrides.update(args.control.register(config, universe, node, pod_uid))
    env.update(overrides)
    command = [str(args.binary)]
    if cpus:
        command = ["taskset", "-c", cpus] + command
    with (args.output / f"{name}.log").open("w") as log:
        process = subprocess.Popen(command, env=env, stdout=log, stderr=log,
                                   start_new_session=True)
    try:
        emit(args.output, events, "start", name=name, pid=process.pid, command=command,
             sha256=hashlib.sha256(args.binary.read_bytes()).hexdigest(), env=overrides)
    except BaseException:
        stop(process)
        raise
    return process


def stop(process):
    if process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
    return process.returncode


def wait_listener(process, port, log):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        assert process.poll() is None, log.read_text()
        try:
            with socket.create_connection(("127.0.0.1", port), .1):
                return
        except OSError:
            time.sleep(.1)
    raise AssertionError(f"startup timeout: {log.read_text()}")


def fetch(port, method, target, host="127.0.0.1", timeout=8):
    connection = (UnixConnection(port, timeout) if isinstance(port, (str, pathlib.Path))
                  else http.client.HTTPConnection(host, port, timeout=timeout))
    try:
        connection.request(method, target, headers={"Connection": "close"})
        response = connection.getresponse()
        return response.status, dict(response.getheaders()), response.read()
    finally:
        connection.close()


def metrics(port, output=None, host="127.0.0.1"):
    status, _, data = fetch(port, "GET", "/metrics", host=host, timeout=2)
    assert status == 200, (status, data)
    text = data.decode()
    if output:
        output.write_text(text)
    return {key: float(value) for line in text.splitlines()
            if line and not line.startswith("#") for key, value in [line.rsplit(" ", 1)]}


def wait_ready(args, events, process, port, name="daemon", host="127.0.0.1"):
    deadline = time.monotonic() + 20
    state = None
    while time.monotonic() < deadline:
        assert process.poll() is None, (args.output / f"{name}.log").read_text()
        try:
            status, _, data = fetch(port, "GET", "/status", host=host, timeout=2)
            state = json.loads(data)
            tls = state.get("tls") or {}
            if (status == 200 and state.get("ready") and tls.get("generation") == 1
                    and tls.get("installedWorkers", 0) == tls.get("workers")
                    and tls.get("workers", 0) > 0 and tls.get("error") is None):
                emit(args.output, events, "mtls_ready", name=name, status=state)
                return
        except (OSError, ValueError):
            # Transient startup/readiness errors; sleep below before retrying.
            pass
        time.sleep(.1)
    raise AssertionError(f"mTLS activation timeout: {state}; {(args.output / f'{name}.log').read_text()}")


def resources(process, live, hits):
    result = {"origin_live": len(live), "origin_hits": len(hits)}
    proc = pathlib.Path(f"/proc/{process.pid}")
    stat = (proc / "stat").read_text().rsplit(")", 1)[1].split()
    status = (proc / "status").read_text().splitlines()
    return dict(result, fds=len(list((proc / "fd").iterdir())),
                cpu_seconds=(int(stat[11]) + int(stat[12])) / os.sysconf("SC_CLK_TCK"),
                rss=next(line for line in status if line.startswith("VmRSS:")))


def idle_close(args):
    events, hits, live, errors = [], [], {}, []
    lock, stopping = threading.Lock(), threading.Event()
    payload = bytes(i % 251 for i in range(65536))
    origin_path = args.output / "origin"
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(str(origin_path))
    listener.listen(16)
    listener.settimeout(.1)

    def serve(stream, serial):
        try:
            stream.settimeout(8)
            with lock:
                live[serial] = stream
            while not stopping.is_set():
                data = b""
                while not data.endswith(b"\r\n\r\n"):
                    part = stream.recv(1)
                    if not part:
                        return
                    data += part
                    assert len(data) <= 8192
                lines = data.decode().split("\r\n")
                method, path, _ = lines[0].split()
                headers = dict(line.split(": ", 1) for line in lines[1:] if line)
                assert method in ("HEAD", "GET")
                assert headers["Host"] == "localhost"
                assert headers["Accept-Encoding"] == "identity"
                assert not any(key.lower() == "x-racer-target" for key in headers)
                with lock:
                    hits.append(dict(serial=serial, method=method,
                                     target=path, request=data.decode()))
                if method == "GET":
                    assert headers["Range"] == "bytes=0-65535"
                    assert headers["If-Match"] == '"' + hashlib.sha256(payload).hexdigest() + '"'
                    assert headers["Accept-Encoding"] == "identity"
                stream.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 65536\r\n"
                               + ('ETag: "' + hashlib.sha256(payload).hexdigest()
                                  + '"\r\nCache-Control: max-age=60\r\n\r\n').encode()
                               + (payload if method == "GET" else b""))
        except (OSError, TimeoutError):
            pass  # Explicit idle close or shutdown.
        except Exception as error:
            errors.append(repr(error))
        finally:
            stream.close()
            with lock:
                live.pop(serial, None)

    def accept():
        serial = 0
        while not stopping.is_set():
            try:
                stream, _ = listener.accept()
            except socket.timeout:
                continue
            serial += 1
            threading.Thread(target=serve, args=(stream, serial), daemon=True).start()

    thread = threading.Thread(target=accept)
    thread.start()
    process = None
    try:
        ingress, management = ports(2)
        config = args.output / "config.json"
        publish(config, local_snapshot(args.output, [ingress], [origin_path]))
        process = launch(args, events, config, management, cpus=args.cpus)
        wait_ready(args, events, process, management)
        wait_listener(process, ingress, args.output / "daemon.log")

        def request(method, target):
            start = time.monotonic()
            status, headers, body = fetch(client_path(args.output, ingress), method, target, timeout=5)
            assert status == 200, (method, target, status, body)
            assert headers["Content-Length"] == "65536"
            assert body == (payload if method == "GET" else b"")
            assert process.poll() is None
            return time.monotonic() - start

        for reset in (False, True):
            mode = "rst" if reset else "fin"
            for method in ("HEAD", "GET"):
                target = f"/{mode}-{method}"
                request("HEAD", target if method == "GET" else target + "-warm")
                time.sleep(.025)  # Completed ingress proves the upstream was consumed.
                with lock:
                    assert len(live) == 1, live
                    old, stream = next(iter(live.items()))
                    if reset:
                        stream.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER,
                                          struct.pack("ii", 1, 0))
                        stream.shutdown(socket.SHUT_RD)  # Wake recv; close sends RST.
                    else:
                        stream.shutdown(socket.SHUT_RDWR)
                for _ in range(200):
                    with lock:
                        if old not in live:
                            break
                    time.sleep(.001)
                else:
                    raise AssertionError("origin idle socket did not close")
                time.sleep(.01)
                before = len(hits)
                elapsed = request(method, target)
                assert len(hits) == before + 1, hits[before:]
                assert hits[-1]["method"] == method and hits[-1]["serial"] != old
                request("HEAD", target + "-healthy")
                assert hits[-1]["serial"] == hits[-2]["serial"]
                emit(args.output, events, "idle_close", mode=mode, method=method,
                     seconds=elapsed, old_socket=old, replacement=hits[-1]["serial"],
                     body_bytes=65536 if method == "GET" else 0)
        time.sleep(.3)
        counts = metrics(management, args.output / "metrics.txt")
        prefix = 'racer_dataplane_upstream_requests_total{destination="backend",transport="http",kind='
        assert counts[prefix + '"metadata"}'] == 12, counts
        assert counts[prefix + '"page"}'] == 4, counts
        assert not errors, errors
        emit(args.output, events, "verified", origin_hits=len(hits),
             upstream_attempts=16, requests=hits)
    finally:
        code = stop(process) if process else None
        for _ in range(100):
            if not live:
                break
            time.sleep(.01)
        remaining = len(live)
        stopping.set()
        thread.join(timeout=2)
        listener.close()
        origin_path.unlink(missing_ok=True)
        with lock:
            for stream in live.values():
                stream.shutdown(socket.SHUT_RDWR)
        emit(args.output, events, "shutdown", exit_code=code, live_sockets=remaining)
        assert not remaining and not thread.is_alive(), remaining
        assert not errors, errors
        assert code in (None, 0), code


def idle_pressure(args):
    hits, live, events = [], set(), []
    lock, gate = threading.Lock(), None

    class Origin(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def setup(self):
            super().setup()
            with lock:
                live.add(self.client_address)

        def finish(self):
            try:
                super().finish()
            finally:
                with lock:
                    live.discard(self.client_address)

        def log_message(self, *unused):
            pass

        def do_GET(self):
            assert args.scenario == "hot-cache"
            assert self.headers["If-Match"] == '"' + hashlib.sha256(b"abc").hexdigest() + '"'
            assert self.headers["Range"] == "bytes=0-2"
            self.do_HEAD()
            self.wfile.write(b"abc")

        def do_HEAD(self):
            assert self.headers["Host"] == "localhost"
            assert self.headers["Accept-Encoding"] == "identity"
            assert "X-Racer-Target" not in self.headers
            with lock:
                hits.append((self.requestline.split()[1], self.client_address))
            current_gate = gate
            if current_gate is not None:
                current_gate.wait(timeout=5)
            self.send_response(200)
            self.send_header("Content-Length", "3")
            self.send_header("ETag", '"' + hashlib.sha256(b"abc").hexdigest() + '"')
            self.send_header("Cache-Control", "max-age=3600" if args.scenario == "hot-cache" else "max-age=0")
            self.end_headers()

    *listeners, management = ports(65 if args.scenario == "fanout" else 2)
    origin_paths = [args.output / f"origin-{i}" for i in range(len(listeners))]
    origins = [UnixHTTPServer(str(path), Origin) for path in origin_paths]
    threads = [threading.Thread(target=origin.serve_forever) for origin in origins]
    for thread in threads:
        thread.start()
    process, connection = None, None
    try:
        snapshot = local_snapshot(args.output, listeners, origin_paths)
        config = args.output / "config.json"
        publish(config, snapshot)
        process = launch(args, events, config, management, cpus=args.cpus)
        wait_ready(args, events, process, management)
        wait_listener(process, listeners[-1], args.output / "daemon.log")
        time.sleep(.2)

        def request(index, target, connection=None, method="HEAD"):
            owned = connection is None
            connection = connection or UnixConnection(client_path(args.output, listeners[index]))
            start = time.monotonic()
            try:
                connection.request(method, target)
                response = connection.getresponse()
                body = response.read()
                assert response.status == 200, (target, response.status)
                assert response.getheader("ETag") == '"' + hashlib.sha256(b"abc").hexdigest() + '"'
                assert body == (b"abc" if method == "GET" else b"")
                return time.monotonic() - start
            finally:
                if owned:
                    connection.close()

        def sample():
            return resources(process, live, hits)

        if args.scenario == "fanout":
            # Origin barriers require 255 distinct retained Unix sockets, not just hits.
            for i, count in enumerate([4] * 58 + [8, 8, 4, 2, 1]):
                gate = threading.Barrier(count)
                before = len(hits)
                with concurrent.futures.ThreadPoolExecutor(max_workers=count) as executor:
                    latencies = list(executor.map(lambda j: request(i, f"/fill-{i}-{j}"),
                                                 range(count)))
                gate = None
                assert len(hits) - before == count
                emit(args.output, events, "fill", volume=i, count=count,
                     max_seconds=max(latencies))
            assert len(live) == 255, len(live)
            emit(args.output, events, "filled_255", **sample())
            for delay in (0, 1, 5, 31):
                time.sleep(delay)
                before = len(hits)
                elapsed = request(63, f"/cold-{delay}")
                assert len(hits) == before + 1
                assert process.poll() is None
                emit(args.output, events, "cold_progress", delay=delay, seconds=elapsed, **sample())
            before = set(live)
            elapsed = request(0, "/warm")
            assert set(live) == before, "warm pool must retain its Unix connections"
            emit(args.output, events, "warm_reuse", seconds=elapsed, **sample())
            snapshot["revision"] = snapshot["epoch"] = "2"
            for volume in snapshot["volumes"]:
                volume["cacheGeneration"] = volume["topology"]["epoch"] = "2"
            publish(config, snapshot)
            for _ in range(100):
                if metrics(management).get("racer_dataplane_config_epoch") == 2:
                    break
                time.sleep(.1)
            else:
                raise AssertionError("activation timeout")
            for i in range(64):
                request(i, f"/new-generation-{i}")
            assert len(live) == 320, len(live)
            emit(args.output, events, "retained_generations", **sample())
            time.sleep(33)
            assert len(live) == 64, len(live)
            emit(args.output, events, "retired_cleanup", **sample())
            request(63, "/after-retirement")
        elif args.scenario == "hot-cache":
            connection = UnixConnection(client_path(args.output, listeners[0]))
            request(0, "/hot", connection)
            request(0, "/hot", connection, "GET")
            assert len(hits) == 2, "one origin HEAD and one origin GET"
            for trial in range(2):
                for method in ("HEAD", "GET"):
                    before, start = sample(), time.monotonic()
                    latencies = [request(0, "/hot", connection, method) for _ in range(2000)]
                    elapsed = time.monotonic() - start
                    assert len(hits) == 2, "warm reads must not reach the origin"
                    emit(args.output, events, "hot-cache", method=method, trial=trial,
                         requests=2000, payload_bytes=3, seconds=elapsed,
                         requests_per_second=2000 / elapsed,
                         median_ms=1000 * statistics.median(latencies),
                         p99_ms=1000 * sorted(latencies)[1979], before=before, after=sample())
        else:
            connection = UnixConnection(client_path(args.output, listeners[0]))
            request(0, "/warmup", connection)
            before, peers, start = sample(), set(live), time.monotonic()
            latencies = [request(0, f"/churn-{i}", connection) for i in range(2000)]
            elapsed = time.monotonic() - start
            assert set(live) == peers and len(peers) == 1
            assert len(hits) == 2001
            emit(args.output, events, "churn", requests=2000, seconds=elapsed,
                 requests_per_second=2000 / elapsed,
                 median_ms=1000 * statistics.median(latencies),
                 p99_ms=1000 * sorted(latencies)[1979], before=before, after=sample())
        if args.scenario == "hot-cache":
            # Worker counters publish asynchronously; observe the quiescent total.
            for _ in range(30):
                values = metrics(management, args.output / "metrics.txt")
                if values.get('racer_dataplane_requests_total{source="client",transport="http"}') == 8002:
                    break
                time.sleep(.1)
            assert values['racer_dataplane_cache_lookups_total{kind="metadata",result="memory_hit"}'] == 8001
            assert values['racer_dataplane_cache_lookups_total{kind="page",result="disk_hit"}'] == 4000
            assert values['racer_dataplane_cache_lookups_total{kind="metadata",result="miss"}'] == 1
            assert values['racer_dataplane_cache_lookups_total{kind="page",result="miss"}'] == 1
        else:
            metrics(management, args.output / "metrics.txt")
    finally:
        if connection:
            connection.close()
        code = stop(process) if process else None
        for _ in range(100):
            if not live:
                break
            time.sleep(.01)
        remaining = len(live)
        for origin, thread in zip(origins, threads):
            origin.shutdown()
            origin.server_close()
            thread.join(timeout=2)
        emit(args.output, events, "shutdown", exit_code=code, origin_live=remaining)
        assert not remaining and not any(thread.is_alive() for thread in threads), remaining
        assert code in (None, 0), code


def conformance_targets(text, expected):
    targets = {}
    for line in text.splitlines():
        # libtest can prefix the first stdout line with "test <name> ... ".
        _, marker, value = line.partition("B03_TARGET ")
        if marker:
            label, target = value.split()
            assert label not in targets, f"duplicate target: {line}"
            targets[label] = target
    assert len(targets) == expected, text
    return targets


def physical_owner(args):
    root, events, hits, errors, processes = args.snapshots, [], [], [], []
    manifest = json.loads((root / "manifest.json").read_text())
    interleaved = args.layout == "interleaved"
    configs = [root / (f"{name}.json" if interleaved else f"p8-n2-historical-{n}.json")
               for n, name in enumerate(("a", "b"))]
    env = dict(os.environ, B03_GO_SNAPSHOT=str(configs[1]), B02_EXPORT=str(root))
    command = ["cargo", "test", "--release", "--locked", "--lib", "-j", str(args.jobs),
               "b02_go_placement_conformance" if interleaved else "b03_go_snapshot_consistency",
               "--", "--ignored", "--nocapture", "--test-threads=1"]
    with (args.output / "rust-consistency.log").open("w") as log:
        subprocess.run(command, cwd=DATAPLANE,
                       env=env, stdout=log, stderr=subprocess.STDOUT,
                       timeout=args.build_timeout, check=True)
    text = (args.output / "rust-consistency.log").read_text()
    targets = conformance_targets(text, 8 if interleaved else 4)
    emit(args.output, events, "consistency", command=command, snapshots=str(root), targets=targets)

    class Origin(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *unused):
            pass

        def do_HEAD(self):
            self.respond(False)

        def do_GET(self):
            self.respond(True)

        def respond(self, body):
            try:
                target = self.requestline.split()[1]
                assert target in targets.values()
                assert self.headers["Host"] == "localhost"
                assert self.headers["Accept-Encoding"] == "identity"
                assert "X-Racer-Target" not in self.headers
                if body:
                    assert self.headers["Range"] == "bytes=0-2"
                    assert self.headers["If-Match"] == '"' + hashlib.sha256(b"abc").hexdigest() + '"'
                hits.append((self.command, self.path, target))
                semantic = "semantic" in target
                self.send_response(404 if semantic else 200)
                self.send_header("Content-Length", "0" if semantic else "3")
                if not semantic:
                    self.send_header("ETag", '"' + hashlib.sha256(b"abc").hexdigest() + '"')
                    self.send_header("Cache-Control", "max-age=60")
                self.send_header("Connection", "close")
                self.end_headers()
                if body and not semantic:
                    self.wfile.write(b"abc")
            except Exception as error:
                errors.append(repr(error))
            finally:
                self.close_connection = True

    def count(values, destination):
        return sum(value for key, value in values.items()
                   if key.startswith("racer_dataplane_upstream_requests_total{")
                   and f'destination="{destination}"' in key)

    snapshots = [json.loads((root / f"{name}.json").read_text())["snapshot"]
                 for name in ("a", "b")]
    origin_paths = {volume["originSocket"] for snapshot in snapshots for volume in snapshot["volumes"]}
    origins = [UnixHTTPServer(path, Origin) for path in sorted(origin_paths)]
    threads = [threading.Thread(target=origin.serve_forever) for origin in origins]
    for thread in threads:
        thread.start()
    try:
        for n, name in enumerate(("a", "b")):
            config = configs[n]
            snapshot = json.loads(config.read_text())["snapshot"]
            # Consume Go exports unchanged; missing scopes must be fixed in the producer.
            for volume in snapshot["volumes"]:
                assert "peerEndpoints" in volume, f"{config}: missing required peerEndpoints"
            slots = snapshot["volumes"][0]["topology"]["localSlots"]
            assert slots == (list(range(n, snapshot["volumes"][0]["topology"]["slotCount"], 2)) if interleaved
                             else list(range(n * 4, n * 4 + 4)))
            process = launch(args, events, config, 18890 + n, name=name,
                             universe=manifest["universe"], node=manifest["nodes"][name]["id"],
                             cpus=args.peer_cpus if n and args.peer_cpus else args.cpus,
                             pod_uid=manifest["nodes"][name]["podUID"],
                             management_host=manifest["nodes"][name]["ip"])
            processes.append(process)
            wait_ready(args, events, process, 18890 + n, name=name,
                       host=manifest["nodes"][name]["ip"])
        phases = ("live", "local", "semantic", "stopped") if interleaved else ("live", "stopped")
        for phase in phases:
            if phase == "stopped":
                assert stop(processes[0]) == 0
                emit(args.output, events, "owner_stopped", idle_pool_expiry_wait=False)
            for method in ("HEAD", "GET"):
                if phase != "stopped" or method != "HEAD":
                    time.sleep(1.2)  # Fresh candidate evidence after the cooldown.
                before, first = metrics(18891, host="127.0.0.3"), len(hits)
                target, start = targets[f"{phase}-{method.lower()}"], time.monotonic()
                status, headers, body = fetch(snapshots[1]["volumes"][0]["clientSocket"], method, target, timeout=20)
                elapsed = time.monotonic() - start
                assert status == (404 if phase == "semantic" else 200), (phase, method, status)
                if phase != "semantic":
                    assert headers["Content-Length"] == "3" and headers["ETag"] == '"' + hashlib.sha256(b"abc").hexdigest() + '"'
                    assert body == (b"abc" if method == "GET" else b"")
                time.sleep(.35)
                after = metrics(18891, args.output / "metrics.txt", host="127.0.0.3")
                peer_delta = count(after, "peer") - count(before, "peer")
                backend_delta = count(after, "backend") - count(before, "backend")
                assert peer_delta == 0 if phase == "local" else peer_delta >= 1
                if phase == "stopped" and method == "HEAD":
                    assert peer_delta == 2, "cached TLS attempt plus one fresh reconnect required"
                assert backend_delta == (0 if phase in ("live", "semantic")
                                         else (2 if method == "GET" else 1))
                assert len(hits) - first == (2 if method == "GET" and phase != "semantic" else 1)
                assert not errors, errors
                emit(args.output, events, "request", phase=phase, method=method, target=target,
                     status=status, body=body.decode(), seconds=elapsed, peer_attempts=peer_delta,
                     local_backend_attempts=backend_delta, origin_hits=hits[first:])
    finally:
        exits = [stop(process) for process in processes]
        for origin, thread in zip(origins, threads):
            origin.shutdown()
            origin.server_close()
            thread.join(timeout=2)
        emit(args.output, events, "shutdown", pids=[p.pid for p in processes],
              exits=exits, origin_stopped=not any(thread.is_alive() for thread in threads))
        assert all(code == 0 for code in exits), exits
        assert not errors and not any(thread.is_alive() for thread in threads), errors
        for host, port in (("127.0.0.2", 18881), ("127.0.0.3", 18881),
                           ("127.0.0.2", 18890), ("127.0.0.3", 18891),
                           ("127.0.0.2", 9443), ("127.0.0.3", 9443)):
            with socket.socket() as stream:
                stream.settimeout(1)
                assert stream.connect_ex((host, port)) != 0, (host, port, "listener leaked")
        for snapshot in snapshots:
            for volume in snapshot["volumes"]:
                assert not pathlib.Path(volume["clientSocket"]).exists(), "client socket leaked"
                assert not pathlib.Path(volume["originSocket"]).exists(), "origin socket leaked"


def positive(value):
    number = int(value)
    if number < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return number


def interrupted(signum, unused_frame):
    # External timeout sends SIGTERM; unwind scenario/fixture cleanup even though
    # children own separate process groups for reliable stop().
    raise SystemExit(128 + signum)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="scenario", required=True)
    for name, description in (
        ("idle-close", "backend FIN/RST replay for HEAD and GET"),
        ("fanout", "255 idle upstream sockets, cold progress and generation retirement"),
        ("churn", "2000 HEADs on one reused upstream Unix connection"),
        ("hot-cache", "two trials of 2000 warm HEADs and GETs through the production daemon"),
        ("physical-owner", "two daemons consuming exact Go-produced snapshots"),
    ):
        command = commands.add_parser(name, help=description, description=description)
        command.add_argument("--binary", type=pathlib.Path, required=True)
        command.add_argument("--output", type=pathlib.Path, required=True,
                             help="new workspace-local artifact/slab directory on ext4; never reused")
        command.add_argument("--cpus", help="optional taskset CPU list; needs distinct physical cores")
        command.add_argument("--build-timeout", type=positive, default=600,
                             help="per-build timeout for Go fixture and Cargo conformance, in seconds")
        if name == "physical-owner":
            command.add_argument("--snapshots", type=pathlib.Path, required=True,
                                 help="Go TestB02ProductionSnapshots export (manifest.json, a.json, b.json)")
            command.add_argument("--layout", choices=("interleaved", "blocks"), default="interleaved")
            command.add_argument("--peer-cpus", help="optional CPU list for the second daemon")
            command.add_argument("--jobs", type=positive, default=os.environ.get("CARGO_BUILD_JOBS", "2"))
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, interrupted)
    args.binary = args.binary.resolve(strict=True)
    args.output = args.output.resolve()
    if args.scenario == "physical-owner":
        args.snapshots = args.snapshots.resolve(strict=True)
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    args.control = ControlFixture(args)
    try:
        if args.scenario == "idle-close":
            idle_close(args)
        elif args.scenario == "physical-owner":
            physical_owner(args)
        else:
            idle_pressure(args)
    finally:
        args.control.close()


if __name__ == "__main__":
    main()
