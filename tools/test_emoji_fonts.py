"""Offline rootfs staging/first-boot checks; no device or package build."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
OVERLAY = ROOT / 'tools/linux/rootfs'
ASSETS = OVERLAY / 'usr/share/techo5/fonts'
SEED = OVERLAY / 'etc/techo5/seed-fonts.sh'
DEPLOY = ROOT / 'tools/linux/deploy-rootfs.sh'
FONT = 'zz-noto-emoji.ttf'
LICENSE = 'zz-noto-emoji.OFL.txt'
FONT_SHA = 'de6c18832938afc99caf132b39d6a30a19bac7f2e812e28db2535b4608d27551'


@unittest.skipUnless(os.name == 'posix', 'native POSIX shell checks')
class EmojiPackaging(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='techo5-emoji-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def seed(self, source, destination):
        return subprocess.run(['sh', str(SEED), str(source), str(destination)],
                              capture_output=True, text=True)

    def test_font_bytes_and_copyright(self):
        self.assertEqual(hashlib.sha256((ASSETS / FONT).read_bytes()).hexdigest(), FONT_SHA)
        license_text = (ASSETS / LICENSE).read_text()
        self.assertIn('Copyright 2013 Google LLC', license_text)
        self.assertIn('SIL OPEN FONT LICENSE Version 1.1', license_text)
        self.assertIn('5) The Font Software', license_text)
        self.assertIn('OTHER DEALINGS IN THE FONT SOFTWARE.', license_text)

    def test_actual_overlay_normalization_preserves_binary_fonts(self):
        stage = self.root / 'stage'
        tools = stage / 'tools'
        tools.mkdir(parents=True)
        for name in ('probe.sh', 'slotctl', 'packages.txt'):
            (tools / name).write_bytes(b'fixture\r\n')
        fonts = stage / 'overlay/usr/share/techo5/fonts'
        shutil.copytree(ASSETS, fonts)
        (fonts / 'fixture.otf').write_bytes(b'font\r\ndata')
        self.assertIn(b'\r\n', (fonts / FONT).read_bytes())
        script = DEPLOY.read_text()
        start = script.index('for f in "$STAGE"/tools/*.sh')
        end = script.index('# The kernel goes in after the line endings are fixed too', start)
        env = os.environ.copy()
        env.update(STAGE=str(stage), INPUTS=str(self.root / 'inputs'))
        subprocess.run(['bash', '-eu', '-c', script[start:end]], env=env,
                       check=True, capture_output=True)
        self.assertEqual((tools / 'probe.sh').read_bytes(), b'fixture\n')
        self.assertEqual(hashlib.sha256((fonts / FONT).read_bytes()).hexdigest(), FONT_SHA)
        self.assertEqual((fonts / 'fixture.otf').read_bytes(), b'font\r\ndata')

    def test_fresh_install_seeds_font_and_license(self):
        destination = self.root / 'state/fonts'
        self.assertEqual(self.seed(ASSETS, destination).returncode, 0)
        for name in (FONT, LICENSE):
            self.assertEqual((destination / name).read_bytes(), (ASSETS / name).read_bytes())
            self.assertEqual((destination / name).stat().st_mode & 0o777, 0o644)
        self.assertEqual(set(p.name for p in destination.iterdir()), {FONT, LICENSE})

    def test_upgrade_keeps_owner_font_and_is_repeatable(self):
        destination = self.root / 'fonts'
        destination.mkdir()
        (destination / FONT).write_bytes(b'owner replacement')
        for _ in range(2):
            self.assertEqual(self.seed(ASSETS, destination).returncode, 0)
            self.assertEqual((destination / FONT).read_bytes(), b'owner replacement')
        self.assertEqual((destination / LICENSE).read_bytes(), (ASSETS / LICENSE).read_bytes())
        self.assertEqual(set(p.name for p in destination.iterdir()), {FONT, LICENSE})

    def test_dangling_owner_symlink_is_not_replaced(self):
        destination = self.root / 'fonts'
        destination.mkdir()
        (destination / FONT).symlink_to(self.root / 'owner-missing-font')
        self.assertEqual(self.seed(ASSETS, destination).returncode, 0)
        self.assertTrue((destination / FONT).is_symlink())
        self.assertFalse((self.root / 'owner-missing-font').exists())

    def test_incomplete_bundle_does_not_install_an_unlicensed_font(self):
        source = self.root / 'incomplete'
        source.mkdir()
        shutil.copyfile(ASSETS / FONT, source / FONT)
        destination = self.root / 'fonts'
        self.assertNotEqual(self.seed(source, destination).returncode, 0)
        self.assertFalse(destination.exists())

    def test_image_without_bundle_needs_no_network_or_state_change(self):
        destination = self.root / 'fonts'
        self.assertEqual(self.seed(self.root / 'absent', destination).returncode, 0)
        self.assertFalse(destination.exists())


if __name__ == '__main__':
    unittest.main()
