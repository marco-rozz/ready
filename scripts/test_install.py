"""Installer integration tests using temporary homes and offline release fixtures."""

import hashlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile

REPO = Path(__file__).resolve().parents[1]


class InstallerFixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ready-install-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.user_home = self.root / "home with spaces"
        self.user_home.mkdir()
        self.fixtures = self.root / "fixtures"
        self.fixtures.mkdir()
        self.write_bundle("first")

    def write_bundle(self, revision):
        self.reference = f"# reference {revision}\nconfig_version: 1\n"
        self.skill = f"# ready-guide {revision}\n"
        members = {
            "config.example.yaml": self.reference.encode(),
            "skills/ready-guide/SKILL.md": self.skill.encode(),
            "skills/ready-guide/assets/help.md": b"fixture asset\n",
        }
        unix = self.fixtures / "ready_darwin_arm64.tar.gz"
        with tarfile.open(unix, "w:gz") as archive:
            for name, data in {"ready": b"#!/bin/sh\nprintf 'ready fixture\\n'\n", **members}.items():
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = 0o755 if name == "ready" else 0o644
                archive.addfile(info, io.BytesIO(data))
        windows = self.fixtures / "ready_windows_amd64.zip"
        with zipfile.ZipFile(windows, "w") as archive:
            archive.writestr("ready.exe", b"installer fixture; not an executable")
            for name, data in members.items():
                archive.writestr(name, data)
        (self.fixtures / "checksums.txt").write_text("".join(
            f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
            for path in (unix, windows)
        ))


@unittest.skipIf(os.name == "nt", "POSIX installer")
class UnixInstallerTest(InstallerFixture):
    def setUp(self):
        super().setUp()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.stub("id", "#!/bin/sh\nprintf '1000\\n'\n")
        self.stub("uname", "#!/bin/sh\ncase \"$1\" in -s) echo Darwin;; -m) echo arm64;; esac\n")
        self.stub("sysctl", "#!/bin/sh\necho 1\n")
        self.stub("curl", f"#!{sys.executable}\n" + """
import os
from pathlib import Path
import shutil
import sys
args = sys.argv[1:]
name = args[-1].rsplit('/', 1)[-1]
shutil.copyfile(Path(os.environ['READY_INSTALL_FIXTURES']) / name, args[args.index('--output') + 1])
""")
        self.env = dict(os.environ, HOME=str(self.user_home), SHELL="/bin/zsh",
                        PATH=str(self.tools) + os.pathsep + os.environ.get("PATH", ""),
                        READY_INSTALL_FIXTURES=str(self.fixtures))
        self.env.pop("VERSION", None)

    def stub(self, name, content):
        path = self.tools / name
        path.write_text(content)
        path.chmod(0o700)

    def install(self, *args):
        result = subprocess.run(["/bin/sh", str(REPO / "scripts/install.sh"), *args],
                                env=self.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_first_install_leaves_agent_to_init_and_exposes_path(self):
        output = self.install()
        shared = self.user_home / ".local/share/ready"
        self.assertFalse((self.user_home / ".ready").exists())
        self.assertEqual((shared / "config.example.yaml").read_text(), self.reference)
        self.assertEqual((shared / "library/skills/ready-guide/SKILL.md").read_text(), self.skill)
        self.assertTrue((self.user_home / ".local/bin/ready").is_symlink())
        self.assertIn("ready init", output)
        rc = self.user_home / ".zshrc"
        shell = subprocess.run(["/bin/sh", "-c", '. "$1"; command -v ready; ready --version',
                                "installer-test", str(rc)], env=self.env,
                               capture_output=True, text=True, timeout=10)
        self.assertEqual(shell.returncode, 0, shell.stderr)
        self.assertIn(str(self.user_home / ".local/bin/ready"), shell.stdout)
        self.assertIn("ready fixture", shell.stdout)

    def test_upgrade_preserves_agent_and_refreshes_program_content(self):
        self.install()
        agent = self.user_home / ".ready"
        agent.mkdir()
        config = agent / "config.yaml"
        config.write_text("# my configured agent\n")
        skills = agent / "skills"
        skills.mkdir()
        (skills / "personal.md").write_text("my skill edits\n")
        self.write_bundle("updated")
        output = self.install()
        self.assertEqual(config.read_text(), "# my configured agent\n")
        self.assertEqual((skills / "personal.md").read_text(), "my skill edits\n")
        shared = self.user_home / ".local/share/ready"
        self.assertEqual((shared / "config.example.yaml").read_text(), self.reference)
        self.assertEqual((shared / "library/skills/ready-guide/SKILL.md").read_text(), self.skill)
        self.assertEqual((self.user_home / ".zshrc").read_text().count("# >>> ready >>>"), 1)
        self.assertTrue(output.rstrip().endswith("restart ready serve to use the new version"))

    def test_purge_forgets_the_store_key_before_deleting_the_agent(self):
        self.install()
        body = self.user_home / ".local/share/ready/bin/ready"
        log = self.root / "calls.log"
        # The fixture binary records every call and emulates the maintenance holder.
        body.write_text(f"""#!/bin/sh
echo "$@" >> '{log}'
case "$1" in
  maintenance) echo MAINTENANCE-LOCKED; while :; do sleep 1; done ;;
  secrets) [ -f "$3/config.yaml" ] || exit 3; exit 0 ;;
esac
""")
        body.chmod(0o755)
        agent = self.user_home / ".ready"
        agent.mkdir()
        (agent / "config.yaml").write_text("config_version: 2\n")
        result = subprocess.run(["/bin/sh", str(REPO / "scripts/install.sh"), "--uninstall", "--purge", "--yes"],
                                env=self.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = log.read_text().splitlines()
        forget = [c for c in calls if c.startswith("secrets forget-key")]
        self.assertEqual(len(forget), 1, calls)
        self.assertIn("--yes", forget[0])
        self.assertIn(str(agent), forget[0])
        # forget-key ran while the exclusive gate was held and before the deletion.
        self.assertLess(calls.index(next(c for c in calls if c.startswith("maintenance"))), calls.index(forget[0]))
        self.assertFalse(agent.exists())
        self.assertFalse(body.exists())

    def test_purge_still_completes_when_forgetting_the_key_fails(self):
        self.install()
        body = self.user_home / ".local/share/ready/bin/ready"
        body.write_text("""#!/bin/sh
case "$1" in
  maintenance) echo MAINTENANCE-LOCKED; while :; do sleep 1; done ;;
  secrets) exit 1 ;;
esac
""")
        body.chmod(0o755)
        agent = self.user_home / ".ready"
        agent.mkdir()
        (agent / "config.yaml").write_text("config_version: 2\n")
        result = subprocess.run(["/bin/sh", str(REPO / "scripts/install.sh"), "--uninstall", "--purge", "--yes"],
                                env=self.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("by hand", result.stderr)
        self.assertFalse(agent.exists())

    def test_path_opt_out_does_not_create_shell_configuration(self):
        self.assertIn("skipped (--no-modify-path)", self.install("--no-modify-path"))
        self.assertFalse((self.user_home / ".zshrc").exists())
        self.assertFalse((self.user_home / ".ready").exists())


@unittest.skipUnless(os.name == "nt", "Windows installer")
class WindowsInstallerTest(InstallerFixture):
    def test_install_and_upgrade_leave_agent_configuration_untouched(self):
        powershell = shutil.which("pwsh") or shutil.which("powershell")
        self.assertIsNotNone(powershell, "PowerShell is required on the Windows test host")
        source = (REPO / "scripts/install.ps1").read_text(encoding="utf-8")
        definitions = source.split("# Dispatch: the piped invocation", 1)[0]
        harness = definitions + r'''
$env:USERPROFILE = $env:READY_TEST_HOME
$env:PROCESSOR_ARCHITEW6432 = 'AMD64'
$script:FixturePath = 'C:\existing-bin'
function Test-Elevated { return $false }
function Get-RawUserPath { return $script:FixturePath }
function Get-UserPathKind { return [Microsoft.Win32.RegistryValueKind]::String }
function Set-RawUserPath { param([string]$Value, $Kind); $script:FixturePath = $Value }
function Invoke-WebRequest {
    param($Uri, $OutFile, [switch]$UseBasicParsing)
    Copy-Item (Join-Path $env:READY_INSTALL_FIXTURES (([Uri]$Uri).Segments[-1])) $OutFile
}
Main
$agent = Get-AgentDirPath
if (Test-Path $agent) { throw 'installer created agent data before init' }
if (-not (Test-Path (Get-LauncherPath))) { throw 'ready.exe missing' }
if (-not (Test-Path (Join-Path (Get-LibraryPath) 'ready-guide\SKILL.md'))) { throw 'skills missing' }
if (-not (Test-Path (Join-Path (Get-ShareRootPath) 'config.example.yaml'))) { throw 'reference missing' }
$bin = Split-Path (Get-LauncherPath) -Parent
if (-not ($script:FixturePath.EndsWith($bin))) { throw 'user PATH not configured' }
New-Item -ItemType Directory -Path $agent | Out-Null
Set-Content (Join-Path $agent 'config.yaml') 'my configured agent'
$before = [IO.File]::ReadAllBytes((Join-Path $agent 'config.yaml'))
$oldPath = $script:FixturePath
Main
$after = [IO.File]::ReadAllBytes((Join-Path $agent 'config.yaml'))
if ([Convert]::ToBase64String($before) -ne [Convert]::ToBase64String($after)) { throw 'config overwritten' }
if ($script:FixturePath -ne $oldPath) { throw 'PATH duplicated' }
'''
        script = self.root / "windows-installer-test.ps1"
        script.write_text(harness, encoding="utf-8-sig")
        env = dict(os.environ, READY_TEST_HOME=str(self.user_home),
                   READY_INSTALL_FIXTURES=str(self.fixtures))
        env.pop("VERSION", None)
        result = subprocess.run([powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(script)],
                                env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("ready.exe init", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
