"""Run NetPatrol acceptance on a Debian/Ubuntu test machine."""

import argparse
import asyncio
import base64
import concurrent.futures
import contextlib
import csv
import hashlib
import io
import ipaddress
import json
import os
from pathlib import Path
import pwd
import re
import secrets
import socket
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


def command(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def emit(label, **values):
    print(json.dumps(dict(check=label, **values), ensure_ascii=False), flush=True)


def wait_until(check, seconds):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.25)
    raise RuntimeError(f"check did not finish within {seconds} seconds")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, url):
        return None


class Client:
    def __init__(self, address, password):
        self.base = f"http://{address}"
        basic = base64.b64encode(f"admin:{password}".encode()).decode()
        self.headers = {"Authorization": f"Basic {basic}"}

    def request(self, path, form=None, expected=200, fragment=False):
        headers = self.headers.copy()
        if fragment:
            headers["HX-Request"] = "true"
        data = None
        if form is not None:
            if isinstance(form, bytes):
                boundary = "netpatrol-acceptance"
                headers["Content-Type"] = f"multipart/form-data; boundary={boundary}"
                data = (
                    f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"assets.csv\"\r\n"
                    "Content-Type: text/csv\r\n\r\n"
                ).encode() + form + f"\r\n--{boundary}--\r\n".encode()
            else:
                data = urllib.parse.urlencode(form, doseq=True).encode()
        request = urllib.request.Request(self.base + path, data, headers)
        try:
            response = urllib.request.build_opener(NoRedirect).open(request, timeout=20)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            content = response.read()
            if response.code != expected:
                raise RuntimeError(f"{path}: expected {expected}, got {response.code}")
            return response.headers, content

    def metrics(self):
        return json.loads(self.request("/diagnostics.json")[1])


@contextlib.contextmanager
def service(stage, mode, listen, allowed, namespace=None):
    token = hashlib.sha256(str(stage).encode()).hexdigest()[:8]
    unit = f"netpatrol-check-{mode}-{token}"
    data = stage / mode
    data.mkdir(mode=0o700)
    user = pwd.getpwnam("nobody")
    os.chown(data, user.pw_uid, user.pw_gid)
    password = secrets.token_urlsafe()
    envfile = stage / f"{mode}.env"
    envfile.touch(mode=0o600, exist_ok=False)
    envfile.write_text(f"NETPATROL_ADMIN_PASSWORD={password}\nNETPATROL_DISCOVERY_CIDRS={allowed}\n")
    database = data / "netpatrol.db"
    binary = stage / "netpatrol-linux-amd64" / "netpatrol"
    properties = [
        "User=nobody", "Group=nogroup", "UMask=0027", "NoNewPrivileges=yes",
        "AmbientCapabilities=CAP_NET_RAW", "CapabilityBoundingSet=CAP_NET_RAW",
        "ProtectHome=yes", "ProtectSystem=strict", "PrivateTmp=yes", "LimitNOFILE=16384",
        "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
        f"EnvironmentFile={envfile}", f"WorkingDirectory={data}", f"ReadWritePaths={data}",
    ]
    if namespace:
        properties.append(f"NetworkNamespacePath=/run/netns/{namespace}")
    props = [f"--property={property_}" for property_ in properties]
    command("systemd-run", "--quiet", "--collect", f"--unit={unit}", *props,
            str(binary), "-listen", listen, "-db", str(database))
    try:
        yield password, database, binary, unit
    finally:
        subprocess.run(["systemctl", "stop", unit], check=False)
        envfile.unlink(missing_ok=True)


def readiness(client):
    try:
        return client.metrics()
    except (urllib.error.URLError, ConnectionError):
        return None


def job_ready(database, job_id):
    with contextlib.closing(sqlite3.connect(f"file:{database}?mode=ro", uri=True)) as connection:
        connection.row_factory = sqlite3.Row
        row = connection.execute(
            "SELECT state, completed, total, local_errors, "
            "(SELECT COUNT(*) FROM discovery_candidates WHERE job_id = ?) AS candidates "
            "FROM discovery_jobs WHERE id = ?", (job_id, job_id),
        ).fetchone()
    return dict(row) if row and row["state"] != "running" else None


def field(stage, prefix, port):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as ipv4, socket.socket(socket.AF_INET6, socket.SOCK_STREAM) as ipv6:
        ipv4.bind(("127.0.0.1", 0))
        ipv6.bind(("::1", 0))
        ipv4.listen()
        ipv6.listen()
        with socket.socket() as address:
            address.bind(("127.0.0.1", 0))
            listen = f"127.0.0.1:{address.getsockname()[1]}"
        with service(stage, "field", listen, str(prefix)) as (password, database, binary, unit):
            client = Client(listen, password)
            wait_until(lambda: readiness(client), 10)
            client.request("/areas", {"name":"Loopback", "networks":"127.0.0.0/8,::1/128"}, 303)
            for index, (target, listener) in enumerate((("127.0.0.1", ipv4),("::1", ipv6)), 1):
                client.request("/devices", {"name":f"Loopback {index}", "area":1, "address":target,
                    "port":listener.getsockname()[1], "enabled":"on"}, 303)
            for index in (1, 2):
                wait_until(lambda: b"status healthy" in client.request(f"/devices/{index}")[1], 15)
            pid = int(command("systemctl","show","-p","MainPID","--value",unit,capture_output=True).stdout)
            process = Path(f"/proc/{pid}/status").read_text()
            assert "CapEff:\t0000000000002000" in process
            assert "Uid:\t0\t" not in process
            emit("dual-stack-systemd", pid=pid, ipv4="healthy", ipv6="healthy", raw_capability=True)
            client.request("/areas", {"name":"Field test", "networks":str(prefix)}, 303)
            form = {"area":2,"prefix":str(prefix),"port":port}
            client.request("/discovery/preview", form)
            headers, _ = client.request("/discovery", form, 303)
            job_id = int(headers["Location"].rsplit("/", 1)[1])
            total = prefix.num_addresses - (2 if prefix.prefixlen <= 30 else 0)
            summary = wait_until(lambda: job_ready(database, job_id), total//5+25)
            assert summary["state"] == "done" and summary["completed"] == total
            assert summary["local_errors"] == 0
            emit("field-discovery", prefix=str(prefix), port=port, **summary)
            backup = stage / "field-backup.db"
            command(str(binary), "-db", str(database), "-backup-to", str(backup))
            verify_backup(backup, 2)


def verify_backup(path, count):
    with contextlib.closing(sqlite3.connect(f"file:{path}?mode=ro", uri=True)) as conn:
        assert conn.execute("PRAGMA quick_check").fetchone() == ("ok",)
        for table in ("devices", "latest_results", "state_events", "ping_minutes"):
            assert conn.execute("SELECT COUNT(*) FROM "+table).fetchone()[0] >= count
    emit("backup-restored", integrity="ok", devices=count)


def namespace(name, *args, **kwargs):
    return command("ip", "netns", "exec", name, *args, **kwargs)


async def camera_server():
    async def connection(reader, writer):
        writer.close()
        await writer.wait_closed()

    server = await asyncio.start_server(connection, "0.0.0.0", 8554, backlog=2048)
    print("ready", flush=True)
    async with server:
        await server.serve_forever()


def fleet(stage):
    token = hashlib.sha256(str(stage).encode()).hexdigest()[:6]
    client, peer = f"np-{token}-client", f"np-{token}-peer"
    test_client, test_peer = f"np{token}a", f"np{token}b"
    with contextlib.ExitStack() as stack:
        for name in (client, peer):
            command("ip", "netns", "add", name)
            stack.callback(command, "ip", "netns", "delete", name)
            command("ip", "-n", name, "link", "set", "lo", "up")
        command("ip", "link", "add", test_client, "netns", client, "type", "veth",
                "peer", "name", test_peer, "netns", peer)
        for name, interface, address in ((client,test_client,"172.31.252.1/30"),(peer,test_peer,"172.31.252.2/30")):
            command("ip", "-n", name, "addr", "add", address, "dev", interface)
            command("ip", "-n", name, "link", "set", interface, "up")
        command("ip", "-n", peer, "route", "add", "local", "198.18.0.0/19", "dev", "lo")
        command("ip", "-n", client, "route", "add", "198.18.0.0/19", "via", "172.31.252.2")
        camera = subprocess.Popen(
            ["ip", "netns", "exec", peer, sys.executable, __file__, str(stage), "camera"],
            stdout=subprocess.PIPE, text=True,
        )

        def stop_camera():
            camera.terminate()
            camera.wait(timeout=10)

        stack.callback(stop_camera)
        if camera.stdout.readline().strip() != "ready":
            raise RuntimeError("test camera listener did not start")
        camera.stdout.close()
        listen = "127.0.0.1:18080"
        _, database, binary, unit = stack.enter_context(
            service(stage, "fleet", listen, "198.18.0.0/19", client)
        )
        namespace(client, sys.executable, __file__, str(stage), "load", listen,
                  str(database), str(binary), unit, peer)


def seed_fleet(client):
    client.request("/areas", {"name":"Fleet", "networks":"198.18.0.0/19"}, 303)
    client.request("/areas", {"name":"Discovery", "networks":"198.18.31.0/26"}, 303)
    asset = io.StringIO()
    writer = csv.writer(asset)
    writer.writerow(["设备编号", "名称", "区域", "IP", "TCP端口"])
    for i in range(1, 5001):
        writer.writerow([f"CAM-{i:05}",f"Camera {i:05}","Fleet",f"198.18.{i//256}.{i%256}",8554])
    _, body = client.request("/imports/csv/preview", asset.getvalue().encode())
    match = re.search(rb'name="token" value="([^"]+)"', body)
    assert match and "确认导入 5000 台".encode() in body
    _, result = client.request("/imports/csv/confirm", {"token":match[1].decode()})
    assert "已提交本次导入".encode() in result
    emit("fleet-assets", imported=5000)


def record_fleet(database, pid, previous):
    states = {}
    with contextlib.closing(sqlite3.connect(f"file:{database}?mode=ro", uri=True)) as conn:
        for state, count in conn.execute("SELECT state, COUNT(*) FROM latest_results GROUP BY state"):
            states[state] = count
        slow = conn.execute(
            "SELECT COUNT(*) FROM latest_results WHERE json_extract(snapshot, '$.interval_ns') = 60000000000"
        ).fetchone()[0]
    process = Path(f"/proc/{pid}")
    fields = (process / "stat").read_text().split(") ", 1)[1].split()
    ticks = int(fields[11]) + int(fields[12])
    now = time.monotonic()
    rss = int((process / "statm").read_text().split()[1]) * os.sysconf("SC_PAGE_SIZE")
    db_bytes = database.stat().st_size
    wal = database.with_name(database.name+"-wal")
    return (ticks,now), {
        "states":states, "slow_devices":slow, "cpu_pct_one_core":
        round(100*(ticks-previous[0])/os.sysconf("SC_CLK_TCK")/(now-previous[1]),1),
        "rss_bytes":rss, "fds":len(list((process/"fd").iterdir())),
        "db_bytes":db_bytes, "wal_bytes":wal.stat().st_size if wal.exists() else 0,
    }


def load(stage, listen, database, binary, unit, peer):
    database = Path(database)
    password_line = (stage / "fleet.env").read_text().splitlines()[0]
    client = Client(listen, password_line.split("=", 1)[1])
    wait_until(lambda: readiness(client), 10)
    seed_fleet(client)
    pid = int(command("systemctl","show","-p","MainPID","--value",unit,capture_output=True).stdout)
    done = threading.Event()
    loads = {"fragment":0, "export":0, "discovery":0}

    def discovery_task():
        client.request("/discovery", {"area":2, "prefix":"198.18.31.0/26", "port":8554}, 303)

    def periodic(label, interval, task):
        while not done.wait(interval):
            task()
            loads[label] += 1

    previous, _ = record_fleet(database,pid,(0,time.monotonic()-1))
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        jobs = [
            pool.submit(periodic,"fragment",.3,lambda: client.request("/devices?area=1&page=2", fragment=True)),
            pool.submit(periodic,"export",5,lambda: client.request("/exports/status.csv?area=1")),
            pool.submit(periodic,"discovery",17,discovery_task),
        ]

        def phase(name, seconds, expected):
            nonlocal previous
            started = time.monotonic()
            while time.monotonic()-started < seconds:
                time.sleep(min(5,max(0,seconds-(time.monotonic()-started))))
                for future in jobs:
                    if future.done():
                        future.result()
                metrics = client.metrics()
                previous, resources = record_fleet(database,pid,previous)
                elapsed = round(time.monotonic()-started,1)
                emit("fleet-sample", phase=name, elapsed_s=elapsed, resources=resources, metrics=metrics)
            assert resources["states"].get(expected) == 5000, resources["states"]
            assert metrics["scheduled_devices"] == 5000 and metrics["missing_rounds"] == 0, metrics
            return resources, metrics

        try:
            phase("healthy",25,"healthy")
            rules = (
                "add table inet netpatrol_test\n"
                "add chain inet netpatrol_test drop_probes { type filter hook input priority 0; }\n"
                "add rule inet netpatrol_test drop_probes ip daddr 198.18.0.0/19 drop\n"
            )
            namespace(peer,"nft","-f","-",input=rules)
            resources, metrics = phase("timeout",12*60,"down")
            assert resources["slow_devices"] == 5000 and metrics["actual_probe_interval"]["p95_ms"] > 50000
            namespace(peer,"nft","delete","table","inet","netpatrol_test")
            resources, metrics = phase("recovery",80,"healthy")
            assert resources["slow_devices"] == 0 and metrics["actual_probe_interval"]["p95_ms"] < 11000
            emit("fleet-complete", devices=5000, loads=loads, missing=metrics["missing_rounds"])
            backup = stage / "fleet-backup.db"
            command(binary,"-db",str(database),"-backup-to",str(backup))
            verify_backup(backup,5000)
        finally:
            done.set()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", type=Path)
    parser.add_argument("check", choices=("field", "fleet", "camera", "load"))
    parser.add_argument("internal", nargs="*")
    parser.add_argument("--field-cidr", type=ipaddress.IPv4Network)
    parser.add_argument("--field-port", type=int, default=554)
    args = parser.parse_args()
    stage = args.stage.resolve(strict=True)
    if not stage.is_dir() or not stage.name.startswith("netpatrol-accept-") or stage.parent != Path("/var/lib"):
        parser.error("stage must be a dedicated /var/lib/netpatrol-accept-* directory")
    if args.check == "field" and (args.field_cidr is None or args.field_cidr.num_addresses > 1024 or
            not 1 <= args.field_port <= 65535):
        parser.error("field mode requires an authorized IPv4 CIDR up to /22 and valid TCP port")
    stage.chmod(0o755)
    match args.check:
        case "field":
            field(stage, args.field_cidr, args.field_port)
        case "fleet":
            fleet(stage)
        case "camera":
            asyncio.run(camera_server())
        case "load":
            load(stage,*args.internal)


if __name__ == "__main__":
    main()
