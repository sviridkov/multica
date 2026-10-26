"""Exercise identity preflight without running privileged host commands."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class MacOSBootstrapTests(unittest.TestCase):
    def run_preflight(self, query, failure=False):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            commands = {
                "dscl": '#!/bin/sh\nif [ "$*" = "$LAB_QUERY" ]; then\n'
                        '  if [ "$LAB_QUERY_FAILURE" = 1 ]; then exit 23; fi\n'
                        '  printf "occupied\\n"\nfi\n',
                "sudo": '#!/bin/sh\nprintf "mutation\\n" > "$LAB_MUTATION"\nexit 99\n',
            }
            for name, contents in commands.items():
                path = root / name
                path.write_text(contents)
                path.chmod(0o755)
            marker = root / "mutation"
            env = dict(os.environ, PATH=f"{root}:/usr/bin:/bin",
                       GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted",
                       GITHUB_RUN_ID="preflight-test", GITHUB_RUN_ATTEMPT="1",
                       LAB_QUERY=query, LAB_QUERY_FAILURE=str(int(failure)),
                       LAB_MUTATION=str(marker))
            result = subprocess.run(
                ["/bin/bash", str(Path(__file__).with_name("native-macos.sh")), "/disposable-build"],
                env=env, capture_output=True, text=True, timeout=60)
            return result, marker.exists()

    def test_occupied_identity_refuses_before_any_mutation(self):
        for query in [". -search /Users UniqueID 490",
                      ". -search /Users RecordName cachelabtask",
                      ". -search /Users UniqueID 60000",
                      ". -search /Groups PrimaryGroupID 60000",
                      ". -search /Users RecordName cachelabrun511",
                      ". -search /Groups RecordName cachelabrun511"]:
            with self.subTest(query=query):
                result, mutated = self.run_preflight(query)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertFalse(mutated, result.stderr)

    def test_directory_query_error_refuses_before_any_mutation(self):
        result, mutated = self.run_preflight(". -search /Users UniqueID 490", failure=True)
        self.assertNotEqual(result.returncode, 0, result.stderr)
        self.assertFalse(mutated, result.stderr)

    def test_clear_preflight_reaches_only_stubbed_mutation(self):
        result, mutated = self.run_preflight("")
        self.assertEqual(result.returncode, 99, result.stderr)
        self.assertTrue(mutated)


if __name__ == "__main__":
    unittest.main()
