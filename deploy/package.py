"""Build a NetPatrol release with configuration and dependency licenses."""

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import zipfile


def build_kylin_installer(root, staging, output):
    payload_path = staging.parent / "payload"
    extract_commands = []
    with payload_path.open("wb") as payload:
        for member in sorted(staging.rglob("*")):
            if not member.is_file():
                continue
            offset = payload.tell()
            with member.open("rb") as source:
                shutil.copyfileobj(source, payload)
            relative = member.relative_to(staging).as_posix()
            size = payload.tell() - offset
            mode = "0755" if relative in ("netpatrol", "install.sh") else "0644"
            extract_commands.append(f"unpack {shlex.quote(relative)} {offset} {size} {mode}")
    with payload_path.open("rb") as payload:
        payload_hash = hashlib.file_digest(payload, "sha256").hexdigest()
    launcher = (root / "deploy/offline-run.sh").read_text(encoding="utf-8").rstrip() + "\n"
    launcher = launcher.replace("@PAYLOAD_SHA256@", payload_hash)
    launcher = launcher.replace("@FILES@", "\n".join(extract_commands))
    placeholder = "@HEADER_BYTES@"
    placeholder_width = len(placeholder)
    header_size = len(launcher.encode("utf-8"))
    header = launcher.replace(placeholder, f"{header_size:0{placeholder_width}d}").encode("utf-8")
    if len(header) != header_size:
        raise ValueError("unexpected self-extracting header size")
    with payload_path.open("rb") as payload, output.open("xb") as release:
        release.write(header)
        shutil.copyfileobj(payload, release)


def write_checksum(output, checksum):
    with output.open("rb") as release:
        release_hash = hashlib.file_digest(release, "sha256").hexdigest()
    with checksum.open("x", encoding="utf-8", newline="\n") as target:
        target.write(f"{release_hash}  {output.name}\n")


def release_version(value):
    match = re.fullmatch(r"v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", value)
    if match is None:
        raise argparse.ArgumentTypeError("version must use vMAJOR.MINOR.PATCH, for example v1.0.0")
    return "v" + ".".join(match.groups())


def release_date(value):
    if re.fullmatch(r"[0-9]{8}", value):
        try:
            datetime.strptime(value, "%Y%m%d")
            return value
        except ValueError:
            pass
    raise argparse.ArgumentTypeError("release date must be a valid YYYYMMDD date")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go", help="Go executable")
    parser.add_argument("--arch", default="amd64", choices=("amd64", "arm64"))
    platform = parser.add_mutually_exclusive_group()
    platform.add_argument("--kylin-offline", action="store_true", help="build the Kylin V10 x86_64 self-extracting installer")
    platform.add_argument("--windows-offline", action="store_true", help="build the Windows x64 offline installation ZIP")
    parser.add_argument("--version", type=release_version, help="release version, for example v1.0.0")
    parser.add_argument("--release-date", type=release_date, help="YYYYMMDD, defaults to the current date in Asia/Shanghai")
    parser.add_argument("--output", type=Path, help="new release file path")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    offline = args.kylin_offline or args.windows_offline
    if offline and args.arch != "amd64":
        parser.error("offline installers require amd64")
    if args.windows_offline:
        name, extension = "netpatrol-windows-x64", ".zip"
    elif args.kylin_offline:
        name, extension = "netpatrol-kylin-v10-x86_64", ".run"
    else:
        name, extension = f"netpatrol-linux-{args.arch}", ".tar.gz"
    if args.release_date and not args.version:
        parser.error("--release-date requires --version")
    if offline and not args.version and args.output is None:
        parser.error("provide --version to generate a dated offline installer filename")
    filename = name
    if args.version:
        date = args.release_date or datetime.now(timezone(timedelta(hours=8))).strftime("%Y%m%d")
        filename = f"{name}-{date}-{args.version}"
    output = (args.output or root / "dist" / f"{filename}{extension}").resolve()
    checksum = output.with_name(output.name + ".sha256")
    if output.exists():
        parser.error(f"archive already exists: {output}")
    if offline and checksum.exists():
        parser.error(f"checksum already exists: {checksum}")
    output.parent.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, GOOS="windows" if args.windows_offline else "linux", GOARCH=args.arch, CGO_ENABLED="0")
    if args.arch == "amd64":
        env["GOAMD64"] = "v1"

    def go(*command, capture=False):
        result = subprocess.run(
            [args.go, *command], cwd=root, env=env, check=True,
            capture_output=capture, text=True, encoding="utf-8",
        )
        return result.stdout if capture else None

    with tempfile.TemporaryDirectory(prefix=".netpatrol-release-", dir=output.parent) as temporary:
        staging = Path(temporary) / name
        staging.mkdir()
        build = ["build", "-trimpath"]
        if offline:
            build.append("-ldflags=-s -w")
        binary = "netpatrol.exe" if args.windows_offline else "netpatrol"
        go(*build, "-o", str(staging / binary), "./cmd/netpatrol")
        content = ["README.md", "netpatrol-design.md"]
        if not args.windows_offline:
            content.extend(("deploy/netpatrol.service", "deploy/netpatrol.env.example"))
        for relative in content:
            shutil.copyfile(root / relative, staging / Path(relative).name)
        if args.windows_offline:
            for filename in ("install.cmd", "install.ps1", "run-local.cmd"):
                script = (root / "deploy/windows" / filename).read_text(encoding="utf-8")
                (staging / filename).write_text(script, encoding="utf-8", newline="\r\n")
        if args.kylin_offline:
            installer = (root / "deploy/install-kylin.sh").read_text(encoding="utf-8")
            (staging / "install.sh").write_text(installer, encoding="utf-8", newline="\n")
        licenses = staging / "licenses"
        licenses.mkdir()
        go_root = Path(go("env", "GOROOT", capture=True).strip())
        shutil.copyfile(go_root / "LICENSE", licenses / "go-LICENSE")
        shutil.copyfile(root / "internal/web/static/htmx-LICENSE.txt", licenses / "htmx-LICENSE.txt")
        for filename in ("echarts-LICENSE.txt", "echarts-NOTICE.txt"):
            shutil.copyfile(root / "internal/web/static" / filename, licenses / filename)

        dependency_listing = go(
            "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Dir}}{{end}}{{end}}",
            "./cmd/netpatrol", capture=True,
        )
        for line in sorted(set(dependency_listing.splitlines()) - {""}):
            module, directory = line.split("\t", 1)
            module_root = Path(directory)
            files = {
                file for pattern in ("LICENSE*", "LICENCE*", "COPYING*", "NOTICE*")
                for file in module_root.glob(pattern) if file.is_file()
            }
            if not files:
                raise RuntimeError(f"no license found for {module} in {module_root}")
            destination = licenses.joinpath(*module.split("/"))
            destination.mkdir(parents=True, exist_ok=True)
            for file in files:
                shutil.copyfile(file, destination / file.name)

        if args.kylin_offline:
            build_kylin_installer(root, staging, output)
        elif args.windows_offline:
            build_windows_archive(staging, output)
        else:
            build_linux_archive(staging, output)
        if offline:
            write_checksum(output, checksum)
    print(output)


def build_linux_archive(staging, output):
    archive_path = staging.parent / "payload.tar.gz"

    def archive_entry(item):
        item.mode = 0o755 if item.isdir() or item.name == f"{staging.name}/netpatrol" else 0o644
        item.uid = item.gid = 0
        item.uname = item.gname = ""
        return item

    with tarfile.open(archive_path, "w:gz") as archive:
        archive.add(staging, arcname=staging.name, filter=archive_entry)
    with archive_path.open("rb") as source, output.open("xb") as target:
        shutil.copyfileobj(source, target)


def build_windows_archive(staging, output):
    with zipfile.ZipFile(output, "x", compression=zipfile.ZIP_DEFLATED) as archive:
        for member in sorted(staging.rglob("*")):
            if member.is_file():
                archive.write(member, member.relative_to(staging).as_posix())


if __name__ == "__main__":
    main()
