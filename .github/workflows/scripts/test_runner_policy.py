import tempfile
import unittest
from pathlib import Path

import runner_policy


class RunnerPolicyTests(unittest.TestCase):
    def test_load_allowlist_ignores_comments_and_case_normalizes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "allowlist.txt"
            path.write_text(
                "julianknutsen\n"
                "  Csells  # maintainer\n"
                "\n"
                "# comment\n",
                encoding="utf-8",
            )

            self.assertEqual(runner_policy.load_allowlist(path), {"julianknutsen", "csells"})

    def test_pull_request_from_allowlisted_author_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "Quad341",
            {"quad341"},
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("every event", reason)
        self.assertEqual(runners["runner_32vcpu"], "blacksmith-32vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-6vcpu-macos-15")
        self.assertEqual(runners["runner_windows"], "blacksmith-4vcpu-windows-2025")

    def test_push_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "push",
            "julianknutsen",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("every event", reason)
        self.assertEqual(runners["runner_32vcpu"], "blacksmith-32vcpu-ubuntu-2404")

    def test_forced_workflow_call_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "workflow_call",
            "",
            set(),
            force_blacksmith=True,
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("every event", reason)
        self.assertEqual(runners["runner_16vcpu"], "blacksmith-16vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-6vcpu-macos-15")
        self.assertEqual(runners["runner_windows"], "blacksmith-4vcpu-windows-2025")

    def test_unlisted_pull_request_author_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "external-contributor",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("every event", reason)
        self.assertEqual(runners["runner_macos"], "blacksmith-6vcpu-macos-15")
        self.assertEqual(runners["runner_windows"], "blacksmith-4vcpu-windows-2025")

    def test_fork_repository_uses_github_even_when_forced(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "julianknutsen",
            {"julianknutsen"},
            force_blacksmith=True,
            repository="quickserve-ai/gascity",
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("GitHub-hosted", reason)
        self.assertEqual(runners["runner_32vcpu"], "ubuntu-latest")
        self.assertEqual(runners["runner_macos"], "macos-15")

    def test_upstream_repository_keeps_blacksmith(self) -> None:
        use_blacksmith, _, runners = runner_policy.select_runners(
            "push",
            "",
            set(),
            repository="gastownhall/gascity",
        )

        self.assertTrue(use_blacksmith)
        self.assertEqual(runners["runner_2vcpu"], "blacksmith-2vcpu-ubuntu-2404")


if __name__ == "__main__":
    unittest.main()
