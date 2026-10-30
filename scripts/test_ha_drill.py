"""Safety checks that execute only the drill's preflight rejection path."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class DrillWorkDirectoryTests(unittest.TestCase):
    def test_existing_files_directories_and_links_are_preserved(self):
        script = Path(__file__).resolve().with_name("ha_postgresql_drill.sh")
        with tempfile.TemporaryDirectory(prefix="xirang-drill-preflight-") as temporary:
            root = Path(temporary)
            evidence = root / "evidence"
            evidence.mkdir()
            sentinel = evidence / "keep"
            sentinel.write_text("original evidence")
            linked = root / "linked"
            linked.symlink_to(evidence, target_is_directory=True)
            dangling = root / "dangling"
            dangling.symlink_to(root / "missing")
            for target in (evidence, sentinel, linked, dangling):
                with self.subTest(target=target.name):
                    result = subprocess.run(
                        ["bash", str(script)],
                        env={**os.environ, "XIRANG_HA_WORK_DIR": str(target)},
                        capture_output=True, text=True, timeout=5, check=False,
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("work directory already exists", result.stderr)
                    self.assertEqual(sentinel.read_text(), "original evidence")
                    self.assertTrue(linked.is_symlink())
                    self.assertTrue(dangling.is_symlink())


if __name__ == "__main__":
    unittest.main()
