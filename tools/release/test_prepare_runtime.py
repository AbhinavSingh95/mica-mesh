"""Local fixture checks. No native build, model, GPU, or public network."""

import json
import os
import signal
import socket
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from tools.release.prepare_runtime import CommandCleanupError, collect_notices, inspect_native, inventory, prepare, run_command, verify_source


COMMIT = "7fe450e19305b828c199d602c23a8337aaa1f03b"


class PayloadTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.payload = self.root / "payload"
        for name in ("runtime/arm64/llama-server", "licenses/llama.cpp.txt",
                     "licenses/third-party.txt"):
            target = self.payload / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(b"fixture")
        (self.payload / "runtime/arm64/llama-server").chmod(0o755)

    def inspect(self, binary):
        return {"architecture": "arm64", "libraries": ["/usr/lib/libSystem.B.dylib"],
                "rpaths": [], "metal_bytes": 7}

    def test_complete_inventory_has_exact_sizes_hashes_and_purposes(self):
        files = inventory(self.payload, "arm64", self.inspect)
        self.assertEqual([entry["path"] for entry in files],
                         ["licenses/llama.cpp.txt", "licenses/third-party.txt",
                          "runtime/arm64/llama-server"])
        self.assertEqual({entry["purpose"] for entry in files}, {"runtime", "license"})
        for entry in files:
            self.assertEqual(entry["size_bytes"], 7)
            self.assertEqual(entry["sha256"],
                             "f16d05ec6b29248d2c61adb1e9263f78e4f7bace1b955014a2d17872cfe4064d")

    def test_missing_file_fails(self):
        for name in ("runtime/arm64/llama-server", "licenses/llama.cpp.txt",
                     "licenses/third-party.txt"):
            with self.subTest(name=name):
                target = self.payload / name
                target.unlink()
                with self.assertRaises(ValueError):
                    inventory(self.payload, "arm64", self.inspect)
                target.write_bytes(b"fixture")
                target.chmod(0o755)

    def test_unexpected_files_and_links_fail(self):
        extra = self.payload / "runtime/arm64/extra.dylib"
        extra.write_bytes(b"extra")
        with self.assertRaises(ValueError):
            inventory(self.payload, "arm64", self.inspect)
        extra.unlink()
        target = self.payload / "runtime/arm64/llama-server"
        target.unlink()
        target.symlink_to(self.payload / "licenses/llama.cpp.txt")
        with self.assertRaises(ValueError):
            inventory(self.payload, "arm64", self.inspect)

    def test_dependency_paths_are_system_only(self):
        for library in ("/opt/homebrew/lib/libggml.dylib", "/usr/local/lib/libggml.dylib",
                        "/tmp/build/libggml.dylib", "@rpath/libggml.dylib",
                        "/usr/lib/../../opt/homebrew/libggml.dylib"):
            with self.subTest(library=library):
                info = self.inspect(None)
                info["libraries"] = [library]
                with self.assertRaises(ValueError):
                    inventory(self.payload, "arm64", lambda binary: info)

    def test_build_directory_rpath_fails(self):
        info = self.inspect(None)
        info["rpaths"] = ["/tmp/build/bin"]
        with self.assertRaises(ValueError):
            inventory(self.payload, "arm64", lambda binary: info)

    def test_missing_metal_and_wrong_architecture_fail(self):
        for update in ({"metal_bytes": 0}, {"architecture": "x86_64"}):
            info = self.inspect(None)
            info.update(update)
            with self.assertRaises(ValueError):
                inventory(self.payload, "arm64", lambda binary: info)

    def test_intel_cpu_needs_no_metal(self):
        old = self.payload / "runtime/arm64"
        old.rename(self.payload / "runtime/amd64")
        info = {"architecture": "x86_64", "libraries": ["/System/Library/Frameworks/Accelerate.framework/Versions/A/Accelerate"], "rpaths": [], "metal_bytes": 0}
        self.assertEqual(len(inventory(self.payload, "amd64", lambda binary: info)), 3)

    def test_source_commit_and_local_changes_are_rejected(self):
        for commit, status in (("0" * 40, ""), (COMMIT, " M CMakeLists.txt"),
                               (COMMIT, "?? extra")):
            def run(args):
                return status if "status" in args else commit
            with self.assertRaises(ValueError):
                verify_source(self.root, run)

    def test_conflicting_output_is_preserved(self):
        output = self.root / "existing"
        output.mkdir()
        sentinel = output / "keep"
        sentinel.write_bytes(b"preserve")
        with self.assertRaises(ValueError):
            prepare(self.root, output, Path("/cmake"), "13.7", run=lambda args: COMMIT)
        self.assertEqual(sentinel.read_bytes(), b"preserve")

    def test_native_inspection_reads_architecture_dependencies_and_resources(self):
        def run(args):
            if "-archs" in args:
                return "arm64"
            if "-L" in args:
                return "binary:\n\t/usr/lib/libSystem.B.dylib (compatibility version 1.0.0, current version 1.0.0)"
            return "Load command 0\n  cmd LC_SEGMENT_64\nSection\n  sectname __ggml_metallib\n  segname __DATA\n  size 0x00000007\n"
        self.assertEqual(inspect_native(self.payload / "runtime/arm64/llama-server", run),
                         {"architecture": "arm64", "libraries": ["/usr/lib/libSystem.B.dylib"], "rpaths": [], "metal_bytes": 7})

    def test_prepare_records_build_settings_and_does_not_install(self):
        source = self.root / "source"
        source.mkdir()
        (source / "LICENSE").write_text("MIT fixture")
        (source / "licenses").mkdir()
        (source / "licenses/LICENSE-jsonhpp").write_text("JSON fixture")
        (source / "vendor/cpp-httplib").mkdir(parents=True)
        (source / "vendor/cpp-httplib/LICENSE").write_text("HTTP fixture")
        for relative in ("vendor/hash/xxhash/LICENSE", "vendor/hash/sha256/LICENSE", "vendor/hash/rotate-bits/LICENSE.md"):
            target = source / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text("hash fixture " + relative)
        for relative in ("vendor/hash/sha1/sha1.c", "vendor/stb/stb_image.h", "vendor/miniaudio/miniaudio.h", "vendor/sheredom/subprocess.h", "ggml/src/ggml-cpu/llamafile/sgemm.cpp"):
            target = source / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text("/* Public Domain fixture " + relative + " */\nint fixture;")
        output = self.root / "prepared"
        def run(args):
            if "rev-parse" in args:
                return COMMIT
            if "status" in args:
                return ""
            if "--find" in args:
                return "/usr/bin/" + args[-1]
            if "proc_translated" in " ".join(args):
                return "0"
            if "-B" in args:
                build = Path(args[args.index("-B") + 1])
                (build / "bin").mkdir(parents=True)
                binary = build / "bin/llama-server"
                binary.write_bytes(b"fixture")
                binary.chmod(0o755)
                settings = [arg[2:] for arg in args if arg.startswith("-D")]
                (build / "CMakeCache.txt").write_text("\n".join(key + ":STRING=" + value for key, value in (item.split("=", 1) for item in settings)))
                return ""
            if "--version" in args:
                return "version: 0.5.0 (build 1, commit 7fe450e)"
            if "--license" in args:
                raise AssertionError("Pinned llama-server has no --license command")
            if "-archs" in args:
                return "arm64"
            if "-L" in args:
                return "binary:\n\t/usr/lib/libSystem.B.dylib (compatibility version 1.0.0, current version 1.0.0)"
            if "-l" in args:
                return "Section\n sectname __ggml_metallib\n segname __DATA\n size 0x7\n"
            return ""
        with patch("tools.release.prepare_runtime.platform.system", return_value="Darwin"), patch("tools.release.prepare_runtime.platform.machine", return_value="arm64"):
            prepare(source, output, Path("/cmake"), "13.7", run=run)
        self.assertTrue((output / "build-record.json").exists(), "preparation must produce the build record")
        record = json.loads((output / "build-record.json").read_text())
        self.assertEqual(record["source_commit"], COMMIT)
        self.assertEqual(record["architecture"], "arm64")
        self.assertEqual(record["runtime_version"], "0.5.0")
        self.assertEqual(record["deployment_target"], "13.7")
        self.assertEqual(record["cmake_settings"]["BUILD_SHARED_LIBS"], "OFF")
        self.assertEqual(record["cmake_settings"].get("CMAKE_SKIP_RPATH"), "ON")
        self.assertEqual(record["cmake_settings"]["GGML_METAL_EMBED_LIBRARY"], "ON")
        self.assertEqual(record["metal_resources"], {"embedded_bytes": 7, "external_files": []})
        files = json.loads((output / "runtime-inventory.json").read_text())["files"]
        self.assertEqual(len(files), 3)
        self.assertTrue((output / "payload/runtime/arm64/llama-server").is_file())
        notices = (output / "payload/licenses/third-party.txt").read_text()
        self.assertIn("MIT fixture", notices)
        self.assertIn("JSON fixture", notices)
        self.assertIn("HTTP fixture", notices)

    def test_unfinished_command_cleanup_preserves_owned_output(self):
        output = self.root / "preserve-preparation"
        def run(args):
            if "rev-parse" in args:
                return COMMIT
            if "status" in args or "proc_translated" in " ".join(args):
                return ""
            if "--find" in args:
                return "/usr/bin/" + args[-1]
            if "-B" in args:
                build = Path(args[args.index("-B") + 1])
                build.mkdir()
                (build / "keep").write_text("owned active input")
                raise CommandCleanupError("Owned child has not stopped")
            self.fail("unexpected fixture operation")
        with patch("tools.release.prepare_runtime.platform.system", return_value="Darwin"), patch("tools.release.prepare_runtime.platform.machine", return_value="arm64"):
            with self.assertRaises(CommandCleanupError):
                prepare(self.root, output, Path("/cmake"), "13.7", run=run)
        self.assertTrue((output / "build/keep").exists(), "cleanup must preserve files until child completion is confirmed")
        self.assertEqual((output / "build/keep").read_text(), "owned active input")

    def test_missing_source_notice_fails(self):
        with self.assertRaises((ValueError, OSError)):
            collect_notices(self.root)


class CommandOwnershipTests(unittest.TestCase):
    def test_error_keeps_bounded_child_diagnostic(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory) / "failure.py"
            fixture.write_text("import sys; print('x' * 100000); print('compiler cause', file=sys.stderr); sys.exit(2)")
            with self.assertRaises(subprocess.SubprocessError) as caught:
                run_command([sys.executable, str(fixture)])
            self.assertIn("compiler cause", str(caught.exception))
            self.assertLess(len(str(caught.exception)), 66 * 1024)

    def test_entrypoint_sigterm_reports_failure_and_restores_handlers(self):
        code = """
import os, signal, sys
from tools.release import prepare_runtime
before = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
def preparation(*args):
    os.kill(os.getpid(), signal.SIGTERM)
prepare_runtime.prepare = preparation
sys.argv = ['prepare_runtime', '--source', '/source', '--output', '/output', '--cmake', '/cmake', '--deployment-target', '13.7']
result = prepare_runtime.main()
after = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
assert result == 1
assert after == before
"""
        environment = dict(os.environ, PYTHONPATH=os.pathsep.join(sys.path))
        result = subprocess.run([sys.executable, "-c", code], stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=environment, timeout=3)
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
        self.assertIn(b"Received SIGTERM", result.stderr)

    def command_fixture(self, interruption, graceful=False):
        with tempfile.TemporaryDirectory() as directory:
            pidfile = Path(directory) / "processes.json"
            resultfile = Path(directory) / "result"
            fixture = Path(directory) / "fixture.py"
            fixture.write_text(r"""
import json, os, signal, subprocess, sys
signal.signal(signal.SIGTERM, signal.SIG_IGN)
child = subprocess.Popen([sys.executable, '-c', "import json,signal,socket,sys; handler = (lambda number,frame: (open(sys.argv[1], 'w').write('terminated'), sys.exit(0))) if sys.argv[2]=='graceful' else signal.SIG_IGN; signal.signal(signal.SIGTERM, handler); endpoint=socket.socket(); endpoint.bind(('127.0.0.1',0)); print(json.dumps(endpoint.getsockname()), flush=True); signal.pause()", sys.argv[1]+'.term', sys.argv[3]], stdout=subprocess.PIPE)
address = json.loads(child.stdout.readline())
with open(sys.argv[1], 'w') as stream:
    json.dump({'root': os.getpid(), 'child': child.pid, 'group': os.getpgrp(), 'address': address}, stream)
print('fixture ready', flush=True)
if sys.argv[2] != 'timeout':
    os.kill(os.getppid(), getattr(signal, sys.argv[2]))
signal.pause()
""")
            # Run the real command owner in another process. The outer test owns
            # a separate group and an eight-second guard, including for RED runs.
            code = """
import signal, sys
from pathlib import Path
from tools.release.prepare_runtime import run_command
before = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
try:
    run_command([sys.executable, sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[5]], timeout=1, terminate_grace=0.1)
except BaseException:
    after = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
    Path(sys.argv[4]).write_text('stopped and restored' if after == before else 'handlers changed')
"""
            environment = dict(os.environ, PYTHONPATH=os.pathsep.join(sys.path))
            owner = subprocess.Popen([sys.executable, "-c", code, str(fixture),
                                      str(pidfile), interruption, str(resultfile), "graceful" if graceful else "resistant"],
                                     start_new_session=True, stdout=subprocess.PIPE,
                                     stderr=subprocess.STDOUT, env=environment)
            processes = None
            try:
                try:
                    output, _ = owner.communicate(timeout=8)
                except subprocess.TimeoutExpired:
                    self.fail("command owner did not join descendants and return within its cleanup bound")
                self.assertEqual(owner.returncode, 0, output.decode(errors="replace"))
                self.assertTrue(pidfile.exists(), "fixture must establish a running descendant")
                processes = json.loads(pidfile.read_text())
                self.assertTrue(resultfile.exists(), "interruption must run owned cleanup")
                self.assertEqual(resultfile.read_text(), "stopped and restored")
                if graceful:
                    self.assertTrue(Path(str(pidfile) + ".term").exists(), "descendant must receive SIGTERM before escalation")
                    self.assertEqual(Path(str(pidfile) + ".term").read_text(), "terminated")
                # The group can have a reparented zombie briefly, but no live
                # descendant can retain stdout: communicate observed pipe EOF.
                # Direct child reaping is required separately at the boundary.
                with self.assertRaises(ProcessLookupError):
                    os.kill(processes['root'], 0)
                with socket.socket() as endpoint:
                    try:
                        endpoint.bind(tuple(processes['address']))
                    except OSError as error:
                        self.fail("descendant still owns its socket after cleanup: " + str(error))
            finally:
                if pidfile.exists():
                    processes = json.loads(pidfile.read_text())
                    for pid in (processes['root'], processes['child']):
                        try:
                            os.kill(pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                try:
                    os.killpg(owner.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                owner.communicate(timeout=3)

    def test_timeout_stops_descendants_and_reaps_direct_child(self):
        self.command_fixture('timeout')

    def test_descendant_receives_graceful_termination_before_escalation(self):
        self.command_fixture('timeout', graceful=True)

    def test_sigint_stops_descendants_and_restores_handlers(self):
        self.command_fixture('SIGINT')

    def test_sigterm_stops_descendants_and_restores_handlers(self):
        self.command_fixture('SIGTERM')


if __name__ == "__main__":
    unittest.main()
