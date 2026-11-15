#!/usr/bin/env python3
"""Model the shared GitHub Actions queue using the committed workflow policies."""

import json
import pathlib
import subprocess
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]


def workflow(name):
    result = subprocess.run(["yq", "-o=json", ".", str(ROOT / ".github/workflows" / name)],
                            text=True, capture_output=True, check=True)
    return json.loads(result.stdout)


class QueueModel:
    # GitHub's single policy replaces pending runs; max retains up to 100.
    # https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency
    def __init__(self, active):
        self.active = active
        self.pending = []
        self.cancelled = []

    def submit(self, name, policy):
        if policy.get("queue", "single") == "max":
            if len(self.pending) == 100:
                self.cancelled.append(name)
            else:
                self.pending.append(name)
        else:
            self.cancelled.extend(self.pending)
            self.pending = [name]

    def finish(self):
        completed = self.active
        self.active = self.pending.pop(0) if self.pending else None
        return completed


class ConcurrencyTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.qa = workflow("qa-bot.yml")
        cls.deploy = workflow("deploy.yml")

    def test_successful_deploy_survives_later_failed_ci_while_qa_runs(self):
        qa_policy, deploy_policy = self.qa["concurrency"], self.deploy["concurrency"]
        self.assertEqual(qa_policy["group"], deploy_policy["group"])
        self.assertEqual(qa_policy["queue"], "max")
        self.assertEqual(deploy_policy.get("queue", "single"), "max")
        self.assertFalse(qa_policy["cancel-in-progress"])
        self.assertFalse(deploy_policy["cancel-in-progress"])
        self.assertIn("github.event.workflow_run.conclusion == 'success'", self.deploy["jobs"]["deploy-server"]["if"])
        self.assertIn("github.event.workflow_run.event == 'push'", self.deploy["jobs"]["deploy-server"]["if"])

        queue = QueueModel("qa-running")
        queue.submit("successful-ci-deploy", deploy_policy)
        queue.submit("failed-ci-deploy", deploy_policy)
        self.assertEqual(queue.pending, ["successful-ci-deploy", "failed-ci-deploy"])
        self.assertEqual(queue.cancelled, [])
        self.assertEqual([queue.finish(), queue.finish(), queue.finish()],
                         ["qa-running", "successful-ci-deploy", "failed-ci-deploy"])

    def test_qa_dispatch_does_not_displace_a_pending_deploy(self):
        queue = QueueModel("deploy-running")
        queue.submit("deploy-pending", self.deploy["concurrency"])
        queue.submit("qa-pending", self.qa["concurrency"])
        self.assertEqual(queue.pending, ["deploy-pending", "qa-pending"])
        self.assertEqual(queue.cancelled, [])

    def test_single_queue_reproduces_the_reported_loss(self):
        queue = QueueModel("qa-running")
        queue.submit("successful-ci-deploy", {"queue": "single"})
        queue.submit("failed-ci-deploy", {"queue": "single"})
        self.assertEqual(queue.cancelled, ["successful-ci-deploy"])
        self.assertEqual(queue.pending, ["failed-ci-deploy"])

    def test_full_queue_rejects_the_new_run_without_evicting_pending_runs(self):
        queue = QueueModel("qa-running")
        for index in range(100):
            queue.submit(f"deploy-{index}", self.deploy["concurrency"])
        queue.submit("later-deploy", self.deploy["concurrency"])
        self.assertEqual(len(queue.pending), 100)
        self.assertEqual(queue.pending[0], "deploy-0")
        self.assertEqual(queue.cancelled, ["later-deploy"])


if __name__ == "__main__":
    unittest.main()
