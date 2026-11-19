import json
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
import citation_tool as installer


def completed(stdout="", code=0):
    return subprocess.CompletedProcess([], code, stdout, "")


class InstallerReadinessTests(unittest.TestCase):
    def install_until_probe(self, replies, expected):
        statuses = iter(replies)

        def run(command, **kwargs):
            if "/status" in command[-1]:
                reply = next(statuses)
                if isinstance(reply, Exception):
                    raise reply
                return reply
            if command[-1].endswith("citationvoice check"):
                return completed('{"live_setup":true}')
            return completed()

        probes = []

        def wait(_, check, **kwargs):
            probes.extend(bool(check()) for _ in expected)
            return False

        with (
            patch.object(installer, "check_prerequisites", return_value=True),
            patch.object(installer, "verify_target_compatibility", return_value=True),
            patch.object(installer, "create_install_backup", return_value=True),
            patch.object(installer, "input", return_value="Test City"),
            patch.object(installer, "deploy_run", side_effect=run),
            patch.object(installer.subprocess, "run", side_effect=run),
            patch.object(installer, "spinner_wait", side_effect=wait),
            patch.object(installer, "log"),
        ):
            installer.step4_install_gemini_assistant("192.0.2.1:5555", api_key="test")
        self.assertEqual(probes, expected)

    def test_install_rejects_unhealthy_capture_and_invalid_status(self):
        self.install_until_probe(
            [
                completed('{"native":true,"capture_health":{"running":false},"other":{"running":true}}\nMIXER_OK'),
                completed('garbage capture_health "running":true\nMIXER_OK'),
                completed('{"native":true,"capture_health":{"running":true}}\nMIXER_OK_EXTRA'),
                completed('{"native":true,"capture_health":{"running":true}}\nMIXER_OK', code=1),
            ],
            [False, False, False, False],
        )

    def test_optional_spotify_receiver_does_not_block_gemini_readiness(self):
        self.install_until_probe(
            [completed('{"native": true, "capture_health": {"running": true}}\nMIXER_OK')], [True]
        )

    def test_spotify_pairing_survives_timeout_and_nonobject_json(self):
        statuses = iter([
            subprocess.TimeoutExpired("adb", 5),
            completed("null"),
            completed('{"connected":true,"receiver_ready":true}', code=1),
            completed('{"connected":true,"receiver_ready":true}'),
        ])
        probes = []

        def run(command, **kwargs):
            if command[-1].endswith("/spotify/status"):
                reply = next(statuses)
                if isinstance(reply, Exception):
                    raise reply
                return reply
            return completed()

        def wait(_, check, **kwargs):
            probes.extend(bool(check()) for _ in range(4))
            return probes[-1]

        with (
            patch.object(installer.subprocess, "run", side_effect=run),
            patch.object(installer, "spinner_wait", side_effect=wait),
            patch.object(installer, "open_page"),
            patch.object(installer, "log"),
        ):
            installer.setup_spotify("192.0.2.1:5555")
        self.assertEqual(probes, [False, False, False, True])


class AutoUpdateReadinessTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.shell = shutil.which("sh")
        if not cls.shell and sys.platform == "win32":
            git_shell = Path("C:/Program Files/Git/bin/sh.exe")
            if git_shell.is_file():
                cls.shell = str(git_shell)
        if not cls.shell:
            raise unittest.SkipTest("a POSIX shell is needed")
        # Include the function definitions without dispatching the device commands.
        script = (ROOT / "runtime" / "auto_update.sh").read_text(encoding="utf-8")
        cls.functions = script.split('case "${1:-}" in\n', 1)[0]

    def run_shell(self, stubs):
        with tempfile.TemporaryDirectory() as tmp:
            root = shlex.quote(Path(tmp).as_posix())
            sandbox = f"\napp={root}\nstate=$app\nbin=$app\npid_file=$app/pid\n"
            return subprocess.run(
                [self.shell, "-s"], input=self.functions + sandbox + stubs,
                capture_output=True, text=True, timeout=10,
            )

    def call(self, stubs):
        return self.run_shell(stubs).returncode

    def test_updater_requires_healthy_microphone(self):
        command = """
daemon_pid() { printf '%s\\n' 2; }
sleep() { :; }
curl() { printf '%s' $RESPONSE; }
wait_ready /test/daemon 1
"""
        for running in (False, True):
            with self.subTest(running=running):
                response = json.dumps({
                    "native": True, "capture_health": {"running": running}, "other": {"running": True},
                })
                self.assertEqual(self.call(command.replace("$RESPONSE", shlex.quote(response))), 0 if running else 1)

    def test_spotify_idle_requires_no_content_or_paused_playback(self):
        stubs = """
assistant_idle() { :; }
systemctl() { :; }
curl() {
    printf '%s' $RESPONSE
    case " $* " in
        *" -w "*) printf '\\n%s' '$CODE';;
    esac
}
speaker_idle
"""
        for code, body, expected in (
            (200, "", 1),
            (204, "", 0),
            (200, '{"paused":true}', 0),
            (200, '{"stopped":true}', 0),
            (200, '{"paused":true,"buffering":true}', 1),
            (200, '{"paused":false}', 1),
            (302, '{"paused":true}', 1),
        ):
            with self.subTest(code=code, body=body):
                script = stubs.replace("$CODE", str(code)).replace("$RESPONSE", shlex.quote(body))
                self.assertEqual(self.call(script), expected)

    def test_recovery_clears_pending_on_either_side_of_binary_replacement(self):
        stubs = """
state=$ROOT
bin=$ROOT
daemon_pid() { return 1; }
recover_update
"""
        for replaced in (False, True):
            with self.subTest(replaced=replaced), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                target = root / "citationvoice"
                previous = root / "previous"
                previous.write_bytes(b"known working version")
                if replaced:
                    target.write_bytes(b"broken candidate")
                else:
                    target.hardlink_to(previous)
                (root / "pending").write_bytes(b"citationvoice\n")
                result = self.run_shell(stubs.replace("$ROOT", shlex.quote(root.as_posix())))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(target.read_bytes(), b"known working version")
                self.assertFalse((root / "pending").exists())
                self.assertFalse(previous.exists())

    def test_bad_metadata_is_rejected_before_downloading(self):
        stubs = """
enabled() { :; }
speaker_idle() { :; }
lock_update() { :; }
recover_update() { :; }
head() { :; }
daemon_pid() { echo 2; }
blob_sha() { echo old; }
github_get() {
    case "$*" in
        *contents*) printf '%s\\n' $METADATA ;;
        *) printf '%s\\n' 'candidate download attempted'; return 25 ;;
    esac
}
check
"""
        for metadata in ({}, {"sha": "a" * 40}, {"sha": "a" * 40, "size": 33554433}):
            with self.subTest(metadata=metadata):
                script = stubs.replace("$METADATA", shlex.quote(json.dumps(metadata, indent=2)))
                result = self.run_shell(script)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Invalid CitationLive metadata", result.stderr)
                self.assertNotIn("candidate download attempted", result.stdout)


if __name__ == "__main__":
    unittest.main()
