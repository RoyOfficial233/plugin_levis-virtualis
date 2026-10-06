"""Regression tests for release packaging (stdlib only)."""
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest
import zipfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRATCH = pathlib.Path.home() / 'AppData/Local/hermes/cache/scratch' if os.name == 'nt' else pathlib.Path(os.environ.get('TMPDIR', '/tmp'))
SCRATCH.mkdir(parents=True, exist_ok=True)


class PackageTests(unittest.TestCase):
    def test_installer_layout_and_unix_permissions(self):
        with tempfile.TemporaryDirectory(dir=SCRATCH) as temporary:
            root = pathlib.Path(temporary)
            stage = root / 'virtualis'
            (stage / 'frontend').mkdir(parents=True)
            (stage / 'plugin').write_bytes(b'test-binary')
            (stage / 'frontend/index.html').write_text('<!doctype html>', encoding='utf-8')
            archive = root / 'release.zip'
            result = subprocess.run([sys.executable, str(ROOT / 'scripts/package.py'), str(stage), str(archive)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            with zipfile.ZipFile(archive) as release:
                self.assertEqual(set(release.namelist()), {'virtualis/plugin', 'virtualis/frontend/index.html'})
                self.assertEqual(release.read('virtualis/plugin'), b'test-binary')
                self.assertEqual((release.getinfo('virtualis/plugin').external_attr >> 16) & 0o777, 0o755)
                self.assertEqual((release.getinfo('virtualis/frontend/index.html').external_attr >> 16) & 0o777, 0o644)

    def test_missing_binary_cannot_replace_existing_archive(self):
        with tempfile.TemporaryDirectory(dir=SCRATCH) as temporary:
            root = pathlib.Path(temporary)
            archive = root / 'release.zip'
            archive.write_bytes(b'previous-release')
            result = subprocess.run([sys.executable, str(ROOT / 'scripts/package.py'), str(root / 'virtualis'), str(archive)], capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(archive.read_bytes(), b'previous-release')

    @unittest.skipUnless(os.name == 'nt', 'Windows batch script')
    def test_build_cmd_aborts_on_failed_go_build(self):
        with tempfile.TemporaryDirectory(dir=SCRATCH) as temporary:
            fake = pathlib.Path(temporary)
            (fake / 'go.cmd').write_text('@exit /b 73\n', encoding='ascii')
            env = dict(os.environ, PATH=str(fake) + os.pathsep + os.environ['PATH'])
            result = subprocess.run(['cmd.exe', '/d', '/c', 'build.cmd'], cwd=ROOT, env=env, capture_output=True)
            self.assertNotEqual(result.returncode, 0, result.stdout.decode(errors='replace'))


if __name__ == '__main__':
    unittest.main()
