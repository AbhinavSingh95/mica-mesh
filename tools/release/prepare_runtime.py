"""Build the pinned native runtime. This tool does not install or publish files.

The output contains payload/, runtime-inventory.json, build-record.json, and
build/. P5 adds the Bazel CLI and hashes the final signed files for release.json.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import selectors
import signal
import shutil
import stat
import subprocess
import sys
import time


COMMIT = "7fe450e19305b828c199d602c23a8337aaa1f03b"
RUNTIME_VERSION = "0.5.0"
MAX_MANIFEST_BYTES = 64 * 1024


class CommandCleanupError(subprocess.SubprocessError):
    """An owned command could not be reaped; its preparation files must remain."""


def interrupted(signum, frame):
    raise InterruptedError("Received " + signal.Signals(signum).name)


def run_command(args, timeout=30 * 60, terminate_grace=5):
    """Own a process group and its output. Call only from the main thread.

    No output-reader thread is needed. Drain one merged pipe synchronously and
    retain its last 64 KiB. On interruption or failure, stop the whole group and
    reap the direct child before the caller may remove its preparation files.
    """
    signals = {signal.SIGINT, signal.SIGTERM}
    previous_mask = signal.pthread_sigmask(signal.SIG_BLOCK, signals)
    previous_handlers = {number: signal.getsignal(number) for number in signals}
    process = None
    events = selectors.DefaultSelector()
    tail = bytearray()
    truncated = False
    launching = True
    launch_signal = None
    cleanup_started = False

    def handle_signal(number, frame):
        nonlocal launch_signal
        if launching:
            # Defer until Popen returns an owned handle. Do not block signals
            # across fork/exec: descendants must inherit the original mask.
            launch_signal = number
            return
        interrupted(number, frame)

    def drain(deadline):
        nonlocal truncated
        while events.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return False
            for key, _ in events.select(remaining):
                chunk = os.read(key.fd, 8192)
                if not chunk:
                    events.unregister(key.fileobj)
                    continue
                tail.extend(chunk)
                if len(tail) > 64 * 1024:
                    truncated = True
                    del tail[:-64 * 1024]
        try:
            process.wait(timeout=max(0, deadline - time.monotonic()))
            return True
        except subprocess.TimeoutExpired:
            return False

    def stop_group():
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        # Drain during the grace period; descendants can retain the pipe after
        # the direct child exits. Escalate the group even if its leader is gone.
        drain(time.monotonic() + terminate_grace)
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        if not drain(time.monotonic() + 5):
            raise CommandCleanupError("Command cleanup did not finish. Preserve the output directory and check owned processes.")

    try:
        for number in signals:
            signal.signal(number, handle_signal)
        signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
        # Defer termination until the process handle and pipe have an owner.
        process = subprocess.Popen([str(arg) for arg in args], start_new_session=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                   bufsize=0)
        events.register(process.stdout, selectors.EVENT_READ)
        launching = False
        if launch_signal is not None:
            interrupted(launch_signal, None)
        if not drain(time.monotonic() + timeout):
            raise subprocess.TimeoutExpired(args, timeout)
        if process.returncode != 0:
            raise subprocess.CalledProcessError(process.returncode, args)
        # All of these commands are finite. Stop any descendant that detached
        # its output before a successful leader exit as well.
        signal.pthread_sigmask(signal.SIG_BLOCK, signals)
        cleanup_started = True
        stop_group()
        return tail.decode("utf-8", errors="replace")
    except BaseException as error:
        signal.pthread_sigmask(signal.SIG_BLOCK, signals)
        if process is not None and not cleanup_started:
            cleanup_started = True
            try:
                stop_group()
            except BaseException as cleanup:
                raise CommandCleanupError(str(cleanup) + " Primary failure: " + str(error)) from error
        if isinstance(error, CommandCleanupError):
            raise
        diagnostic = tail.decode("utf-8", errors="replace")
        if truncated:
            diagnostic = "[earlier command output omitted]\n" + diagnostic
        raise subprocess.SubprocessError(str(error) + ("\n" + diagnostic if diagnostic else "")) from error
    finally:
        events.close()
        if process is not None:
            process.stdout.close()
        for number, handler in previous_handlers.items():
            signal.signal(number, handler)
        signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)


def verify_source(source, run):
    commit = run(["git", "-C", str(source), "rev-parse", "HEAD"]).strip()
    if commit != COMMIT:
        raise ValueError("Source commit differs from the pin. Use a fresh checkout.")
    if run(["git", "-C", str(source), "status", "--porcelain", "--untracked-files=all"]).strip():
        raise ValueError("Source has local changes. Preserve it and use a fresh checkout.")


def system_path(name):
    return (str(PurePosixPath(name)) == name and ".." not in PurePosixPath(name).parts
            and name.startswith(("/usr/lib/", "/System/Library/")))


def inspect_native(binary, run):
    architecture = run(["/usr/bin/lipo", "-archs", str(binary)]).strip()
    libraries = []
    lines = run(["/usr/bin/otool", "-L", str(binary)]).splitlines()
    if not lines or not lines[0].endswith(":"):
        raise ValueError("Cannot read runtime dependencies. Check the native build.")
    for line in lines[1:]:
        match = re.fullmatch(r"\s+(.+) \(compatibility version .+, current version .+\)", line)
        if not match:
            raise ValueError("Cannot parse a runtime dependency. Check the native build.")
        libraries.append(match.group(1))
    commands = run(["/usr/bin/otool", "-l", str(binary)])
    rpaths = re.findall(r"\bpath (.+) \(offset \d+\)", commands)
    metal_bytes = 0
    for section in commands.split("Section")[1:]:
        if re.search(r"\bsectname __ggml_metallib\b", section):
            match = re.search(r"\bsegname __DATA\s+.*?\bsize (0x[0-9a-fA-F]+)", section, re.DOTALL)
            if match:
                metal_bytes += int(match.group(1), 16)
    return {"architecture": architecture, "libraries": libraries,
            "rpaths": rpaths, "metal_bytes": metal_bytes}


def inventory(payload, architecture, inspect):
    """Verify the static embedded payload and inventory its exact regular files."""
    if architecture not in ("arm64", "amd64"):
        raise ValueError("Unsupported architecture. Use a native target Mac.")
    expected = {"runtime/" + architecture + "/llama-server": "runtime",
                "licenses/llama.cpp.txt": "license",
                "licenses/third-party.txt": "license"}
    allowed_directories = {"runtime", "runtime/" + architecture, "licenses"}
    if payload.is_symlink() or not payload.is_dir():
        raise ValueError("Payload root must be a regular directory.")
    found = set()
    for directory, dirs, files in os.walk(str(payload), followlinks=False):
        for name in dirs + files:
            target = Path(directory) / name
            relative = target.relative_to(payload).as_posix()
            mode = target.lstat().st_mode
            if stat.S_ISDIR(mode) and relative in allowed_directories:
                continue
            if not stat.S_ISREG(mode) or relative not in expected:
                raise ValueError("Unexpected or nonregular payload file: " + relative)
            found.add(relative)
    if found != set(expected):
        raise ValueError("Required runtime files are missing: " + ", ".join(sorted(set(expected) - found)))
    binary = payload / ("runtime/" + architecture + "/llama-server")
    if not binary.stat().st_mode & 0o111:
        raise ValueError("Runtime is not executable. Check the native build.")
    info = inspect(binary)
    native_arch = "arm64" if architecture == "arm64" else "x86_64"
    if info["architecture"] != native_arch:
        raise ValueError("Runtime architecture differs from the native target.")
    if architecture == "arm64" and info["metal_bytes"] <= 0:
        raise ValueError("Embedded Metal resources are missing. Check the build settings.")
    for name in info["libraries"] + info["rpaths"]:
        if not system_path(name):
            raise ValueError("Dependency path is outside system locations: " + name)
    result = []
    for relative in sorted(expected):
        target = payload / relative
        digest = hashlib.sha256()
        with target.open("rb") as stream:
            for chunk in iter(lambda: stream.read(64 * 1024), b""):
                digest.update(chunk)
        size = target.stat().st_size
        if size <= 0:
            raise ValueError("Payload file is empty: " + relative)
        result.append({"path": relative, "size_bytes": size,
                       "sha256": digest.hexdigest(), "purpose": expected[relative]})
    return result


def cache_settings(cache):
    result = {}
    for line in cache.read_text().splitlines():
        if line and not line.startswith(("#", "//")) and ":" in line and "=" in line:
            key, value = line.split("=", 1)
            result[key.split(":", 1)[0]] = value
    return result


def write_json(target, value):
    data = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")
    if len(data) > MAX_MANIFEST_BYTES:
        raise ValueError("Release metadata exceeds 64 KiB. Reduce the file inventory.")
    target.write_bytes(data)


def prepare(source, output, cmake, deployment_target, run=None):
    """Produce a fresh runtime preparation directory. Existing paths are preserved."""
    run = run or run_command
    source, output, cmake = Path(source).absolute(), Path(output).absolute(), Path(cmake).absolute()
    if output.exists() or output.is_symlink():
        raise ValueError("Output already exists. Preserve it and choose a fresh directory.")
    if platform.system() != "Darwin" or platform.machine() not in ("arm64", "x86_64"):
        raise ValueError("Build on a native arm64 or Intel Mac.")
    # sysctl -i ignores the absent translation key on a native Intel system.
    if run(["/usr/sbin/sysctl", "-in", "sysctl.proc_translated"]).strip() == "1":
        raise ValueError("This process uses Rosetta. Open a native terminal.")
    if not re.fullmatch(r"\d+\.\d+(?:\.\d+)?", deployment_target):
        raise ValueError("Set an explicit macOS deployment target, such as 13.7.")
    verify_source(source, run)
    native_arch = platform.machine()
    architecture = "arm64" if native_arch == "arm64" else "amd64"
    metal = "ON" if architecture == "arm64" else "OFF"
    settings = {
        "CMAKE_BUILD_TYPE": "Release", "CMAKE_SKIP_RPATH": "ON",
        "CMAKE_C_COMPILER": run(["/usr/bin/xcrun", "--find", "clang"]).strip(),
        "CMAKE_CXX_COMPILER": run(["/usr/bin/xcrun", "--find", "clang++"]).strip(),
        "CMAKE_OSX_ARCHITECTURES": native_arch,
        "CMAKE_OSX_DEPLOYMENT_TARGET": deployment_target,
        "BUILD_SHARED_LIBS": "OFF", "GGML_METAL": metal,
        "GGML_METAL_EMBED_LIBRARY": metal, "GGML_OPENMP": "OFF",
        "LLAMA_OPENSSL": "OFF", "LLAMA_SUBPROCESS": "OFF",
        "LLAMA_BUILD_TESTS": "OFF", "LLAMA_BUILD_EXAMPLES": "OFF",
        "LLAMA_BUILD_APP": "OFF", "LLAMA_BUILD_UI": "OFF",
        "LLAMA_USE_PREBUILT_UI": "OFF", "LLAMA_BUILD_IS_DEV": "OFF",
    }
    if not settings["CMAKE_C_COMPILER"] or not settings["CMAKE_CXX_COMPILER"]:
        raise ValueError("Apple compilers are missing. Install the Command Line Tools.")
    # mkdir owns this fresh output exclusively. No old preparation is overwritten.
    output.mkdir()
    try:
        build = output / "build"
        run([str(cmake), "-S", str(source), "-B", str(build)] +
            ["-D" + key + "=" + value for key, value in settings.items()])
        run([str(cmake), "--build", str(build), "--target", "llama-server",
             "--parallel", "8" if architecture == "arm64" else "4"])
        actual = cache_settings(build / "CMakeCache.txt")
        for key, expected in settings.items():
            if actual.get(key) != expected:
                raise ValueError("CMake setting differs from the release contract: " + key)
        binary = build / "bin/llama-server"
        version = run([str(binary), "--version"])
        if not re.search(r"\b0\.5\.0\s+\(build \d+, commit 7fe450e\)", version):
            raise ValueError("Runtime reports an unexpected version. Check the pinned source.")
        notices = collect_notices(source)
        if not notices.strip():
            raise ValueError("Runtime license notices are missing. Check the native build.")
        payload = output / "payload"
        runtime_root = payload / ("runtime/" + architecture)
        runtime_root.mkdir(parents=True)
        licenses = payload / "licenses"
        licenses.mkdir()
        shutil.copy2(str(binary), str(runtime_root / "llama-server"))
        shutil.copyfile(str(source / "LICENSE"), str(licenses / "llama.cpp.txt"))
        (licenses / "third-party.txt").write_text(notices)
        info = inspect_native(runtime_root / "llama-server", run)
        files = inventory(payload, architecture, lambda binary: info)
        verify_source(source, run)
        write_json(output / "runtime-inventory.json", {"schema": 1, "architecture": architecture,
                   "runtime_version": RUNTIME_VERSION, "runtime_commit": COMMIT,
                   "backend": "metal" if architecture == "arm64" else "cpu", "files": files})
        write_json(output / "build-record.json", {"schema": 1, "source_commit": COMMIT,
                   "source_directory": str(source), "architecture": architecture,
                   "runtime_version": RUNTIME_VERSION, "reported_version": version.strip(),
                   "build_os": platform.mac_ver()[0], "deployment_target": deployment_target,
                   "cmake_settings": {key: actual[key] for key in settings},
                   "dynamic_libraries": info["libraries"], "rpaths": info["rpaths"],
                   "metal_resources": {"embedded_bytes": info["metal_bytes"], "external_files": []}})
    except CommandCleanupError:
        # Do not remove files while an owned descendant might still use them.
        raise
    except BaseException:
        # This output was created by this attempt. Preserve cleanup errors as a
        # chained exception if cleanup itself fails, including on interruption.
        shutil.rmtree(str(output))
        raise


def collect_notices(source):
    """Collect notices from the pinned enabled components, without llama-app."""
    files = ["LICENSE", "vendor/cpp-httplib/LICENSE", "vendor/hash/xxhash/LICENSE",
             "vendor/hash/sha256/LICENSE", "vendor/hash/rotate-bits/LICENSE.md"]
    extra = sorted(source.glob("licenses/LICENSE-*"))
    if not extra:
        raise ValueError("Pinned source license notices are missing. Use a fresh checkout.")
    files.extend(target.relative_to(source).as_posix() for target in extra)
    notices = []
    for relative in files:
        text = (source / relative).read_text()
        if not text.strip():
            raise ValueError("Source license notice is empty: " + relative)
        notices.append("Notice from " + relative + "\n\n" + text.rstrip())
    # These enabled libraries keep their notices within source/header comments.
    for relative in ("vendor/hash/sha1/sha1.c", "vendor/stb/stb_image.h",
                     "vendor/miniaudio/miniaudio.h", "vendor/sheredom/subprocess.h",
                     "ggml/src/ggml-cpu/llamafile/sgemm.cpp"):
        text = (source / relative).read_text()
        comments = re.findall(r"/\*.*?\*/|(?m:(?://[^\n]*(?:\n|$))+)", text, re.DOTALL)
        licensed = [comment for comment in comments if re.search(r"copyright|public domain|SPDX-License", comment, re.IGNORECASE)]
        if not licensed:
            raise ValueError("Enabled component notice is missing: " + relative)
        notices.append("Notice from " + relative + "\n\n" + "\n\n".join(licensed))
    return "\n\n".join(notices) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--cmake", type=Path, required=True)
    parser.add_argument("--deployment-target", required=True)
    args = parser.parse_args()
    previous_handlers = {number: signal.getsignal(number) for number in (signal.SIGINT, signal.SIGTERM)}
    for number in previous_handlers:
        signal.signal(number, interrupted)
    try:
        prepare(args.source, args.output, args.cmake, args.deployment_target)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("Runtime preparation failed: " + str(error), file=sys.stderr)
        return 1
    finally:
        for number, handler in previous_handlers.items():
            signal.signal(number, handler)
    print("Runtime payload prepared at " + str(args.output.absolute()))
    print("Build and fixture checks do not prove native package acceptance.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
