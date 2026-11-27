#!/usr/bin/env python3
"""Check Lima egress rules with renderer and fake-firewall integration tests."""

from __future__ import annotations

import collections
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("install-egress-isolation.sh")
IPTABLES_CHAIN = "TEAGRAM_DEMO_EGRESS"
INPUT_CHAIN = "TEAGRAM_DEMO_INPUT"
EXAMPLE_IPV4_SUBNET = "198.18.90.0/24"
EXAMPLE_IPV4_GATEWAY = "198.18.90.1"
EXAMPLE_IPV6_SUBNET = "fd42:1234:5678::/64"
PRIVATE_IPV4_CIDRS = (
    "10.0.0.0/8",
    "172.16.0.0/12",
    "192.168.0.0/16",
    "100.64.0.0/10",
    "169.254.0.0/16",
    "224.0.0.0/4",
)
PRIVATE_IPV6_CIDRS = ("fc00::/7", "fe80::/10", "ff00::/8")
DEMO_NETWORKS = (
    "teagram-demo_backend",
    "teagram-demo_egress",
    "teagram-demo_edge",
)

FAKE_FIREWALL = r'''#!/usr/bin/env python3
import json
import os
import pathlib
import sys
import time

state_path = pathlib.Path(os.environ["TEAGRAM_TEST_FIREWALL_STATE"])
state = json.loads(state_path.read_text(encoding="utf-8"))
tool = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]

if tool == "docker":
    call_log = pathlib.Path(os.environ["TEAGRAM_TEST_DOCKER_CALL_LOG"])
    with call_log.open("a", encoding="utf-8") as calls:
        print(" ".join(args), file=calls)
    if args == ["info"]:
        count_path = pathlib.Path(os.environ["TEAGRAM_TEST_DOCKER_INFO_COUNT"])
        try:
            count = int(count_path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            count = 0
        count += 1
        count_path.write_text(str(count), encoding="utf-8")
        info_mode = os.environ.get("TEAGRAM_TEST_DOCKER_INFO_MODE", "ready")
        if info_mode == "stall":
            time.sleep(30)
        if info_mode == "fail":
            print("fake Docker daemon is not ready", file=sys.stderr)
            raise SystemExit(1)
        ready_after = int(os.environ.get("TEAGRAM_TEST_DOCKER_READY_AFTER", "1"))
        if count < ready_after:
            print("fake Docker daemon is not ready", file=sys.stderr)
            raise SystemExit(1)
        raise SystemExit(0)
    if args[:2] == ["network", "ls"]:
        missing = set(os.environ.get("TEAGRAM_TEST_MISSING_NETWORKS", "").split(","))
        names = [
            "teagram-demo_backend",
            "teagram-demo_egress",
            "teagram-demo_edge",
        ]
        print("\n".join(name for name in names if name not in missing))
        raise SystemExit(0)
    if args[:2] == ["network", "inspect"]:
        ipv6 = set(os.environ.get("TEAGRAM_TEST_IPV6_NETWORKS", "").split(","))
        subnets = {
            "teagram-demo_backend": "fd42:100::/64",
            "teagram-demo_egress": "fd42:101::/64",
            "teagram-demo_edge": "fd42:102::/64",
        }
        network = args[-1]
        if network in ipv6:
            print(f"true {subnets[network]}")
        else:
            print("false")
        raise SystemExit(0)
    print("unexpected fake docker command", file=sys.stderr)
    raise SystemExit(2)

family = tool.removesuffix("-restore")
firewall = state[family]
if args[:2] == ["-w", "10"]:
    args = args[2:]

def save():
    state_path.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")

def missing_chain(chain):
    print(f"{tool}: No chain/target/match by that name.", file=sys.stderr)
    raise SystemExit(1)

if tool.endswith("-restore"):
    for line in sys.stdin.read().splitlines():
        fields = line.split()
        if not fields:
            continue
        if fields[0] == "-F":
            if fields[1] not in firewall:
                missing_chain(fields[1])
            firewall[fields[1]] = []
        elif fields[0] == "-A":
            chain = fields[1]
            if chain not in firewall:
                missing_chain(chain)
            firewall[chain].append(line)
    save()
    raise SystemExit(0)

operation = args[0]
if operation == "-S":
    chain = args[1]
    if os.environ.get("TEAGRAM_TEST_LISTING_FAILURE") == f"{family}:{chain}":
        print(f"{tool}: Resource temporarily unavailable (xtables lock)", file=sys.stderr)
        raise SystemExit(4)
    if chain not in firewall:
        missing_chain(chain)
    print(f"-N {chain}")
    print("\n".join(firewall[chain]))
elif operation == "-C":
    chain = args[1]
    if chain not in firewall:
        missing_chain(chain)
    expected = f"-A {chain} {' '.join(args[2:])}"
    raise SystemExit(0 if expected in firewall[chain] else 1)
elif operation == "-N":
    chain = args[1]
    if chain in firewall:
        print(f"{tool}: Chain already exists.", file=sys.stderr)
        raise SystemExit(1)
    firewall[chain] = []
    save()
elif operation == "-F":
    chain = args[1]
    if chain not in firewall:
        missing_chain(chain)
    firewall[chain] = []
    save()
elif operation == "-X":
    chain = args[1]
    if chain not in firewall:
        missing_chain(chain)
    del firewall[chain]
    save()
elif operation == "-I":
    chain, index = args[1], int(args[2]) - 1
    if chain not in firewall:
        missing_chain(chain)
    firewall[chain].insert(index, f"-A {chain} {' '.join(args[3:])}")
    save()
elif operation == "-D":
    chain = args[1]
    if chain not in firewall:
        missing_chain(chain)
    if args[2].isdigit():
        index = int(args[2]) - 1
        expected = ""
    else:
        expected = f"-A {chain} {' '.join(args[2:])}"
        try:
            index = firewall[chain].index(expected)
        except ValueError:
            print(f"{tool}: Bad rule (does a matching rule exist in that chain?)", file=sys.stderr)
            raise SystemExit(1)
    interleave = os.environ.get("TEAGRAM_TEST_INTERLEAVE_RULE_CHANGE")
    marker = state_path.with_name("interleaved-rule-change")
    if interleave == "1" and not marker.exists():
        foreign_rule = f"-A {chain} -m comment --comment injected-foreign -j ACCEPT"
        firewall[chain].insert(index, foreign_rule)
        marker.write_text("inserted", encoding="utf-8")
        save()
        if expected:
            index = firewall[chain].index(expected)
    del firewall[chain][index]
    save()
else:
    print(f"unexpected {tool} operation: {operation}", file=sys.stderr)
    raise SystemExit(2)
'''


def render(family: str, *subnets: str) -> list[str]:
    result = subprocess.run(
        [str(SCRIPT), f"--render-{family}", *subnets],
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.splitlines()


class EgressIsolationRuleTests(unittest.TestCase):
    @staticmethod
    def _initial_firewall_state() -> dict[str, dict[str, list[str]]]:
        state: dict[str, dict[str, list[str]]] = {}
        for family in ("iptables", "ip6tables"):
            state[family] = {
                "DOCKER-USER": [
                    "-A DOCKER-USER -s 198.18.129.0/24 -j TEAGRAM_DEMO_EGRESS",
                    "-A DOCKER-USER -j RETURN",
                ],
                "INPUT": [
                    "-A INPUT -s 198.18.129.0/24 -j TEAGRAM_DEMO_EGRESS",
                    "-A INPUT -j DROP",
                ],
                "FORWARD": [
                    "-A FORWARD -j ACCEPT",
                    "-A FORWARD -j DOCKER-USER",
                    "-A FORWARD -j RETURN",
                ],
            }
        return state

    def _mock_firewall(self, directory: str) -> tuple[pathlib.Path, pathlib.Path]:
        mock_bin = pathlib.Path(directory) / "bin"
        mock_bin.mkdir()
        executable = mock_bin / "fake-firewall"
        executable.write_text(FAKE_FIREWALL, encoding="utf-8")
        executable.chmod(0o755)
        for name in (
            "iptables",
            "iptables-restore",
            "ip6tables",
            "ip6tables-restore",
            "docker",
        ):
            (mock_bin / name).symlink_to(executable)
        fake_sleep = mock_bin / "sleep"
        fake_sleep.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        fake_sleep.chmod(0o755)

        state_file = pathlib.Path(directory) / "firewall.json"
        state_file.write_text(
            json.dumps(self._initial_firewall_state(), sort_keys=True), encoding="utf-8"
        )
        return mock_bin, state_file

    def _run_firewall_mode(
        self,
        mock_bin: pathlib.Path,
        state_file: pathlib.Path,
        mode: str,
        missing_networks: tuple[str, ...] = (),
        ipv6_networks: tuple[str, ...] = (),
        listing_failure: str = "",
        docker_ready_after: int = 1,
        interleave_rule_change: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        path = os.pathsep.join((str(mock_bin), os.environ.get("PATH", "")))
        env = os.environ.copy()
        env.update(
            {
                "PATH": path,
                "TEAGRAM_TEST_FIREWALL_STATE": str(state_file),
                "TEAGRAM_TEST_MISSING_NETWORKS": ",".join(missing_networks),
                "TEAGRAM_TEST_IPV6_NETWORKS": ",".join(ipv6_networks),
                "TEAGRAM_TEST_LISTING_FAILURE": listing_failure,
                "TEAGRAM_TEST_INTERLEAVE_RULE_CHANGE": "1" if interleave_rule_change else "0",
                "TEAGRAM_TEST_DOCKER_READY_AFTER": str(docker_ready_after),
                "TEAGRAM_TEST_DOCKER_INFO_COUNT": str(
                    state_file.with_name("docker-info-count")
                ),
                "TEAGRAM_TEST_DOCKER_CALL_LOG": str(
                    state_file.with_name("docker-calls.log")
                ),
            }
        )
        command = [str(SCRIPT), mode]
        if os.geteuid() != 0:
            sudo = shutil.which("sudo")
            if sudo is None:
                self.skipTest("fake firewall integration requires root or passwordless sudo")
            command = [
                sudo,
                "-n",
                "env",
                f"PATH={path}",
                f"TEAGRAM_TEST_FIREWALL_STATE={state_file}",
                f"TEAGRAM_TEST_MISSING_NETWORKS={','.join(missing_networks)}",
                f"TEAGRAM_TEST_IPV6_NETWORKS={','.join(ipv6_networks)}",
                f"TEAGRAM_TEST_LISTING_FAILURE={listing_failure}",
                f"TEAGRAM_TEST_INTERLEAVE_RULE_CHANGE={'1' if interleave_rule_change else '0'}",
                f"TEAGRAM_TEST_DOCKER_READY_AFTER={docker_ready_after}",
                f"TEAGRAM_TEST_DOCKER_INFO_COUNT={state_file.with_name('docker-info-count')}",
                f"TEAGRAM_TEST_DOCKER_CALL_LOG={state_file.with_name('docker-calls.log')}",
                *command,
            ]
        return subprocess.run(command, capture_output=True, text=True, env=env)

    def _run_apply_rules_function(
        self,
        mock_bin: pathlib.Path,
        state_file: pathlib.Path,
        docker_ready_after: int,
        docker_info_mode: str = "ready",
        interleave_rule_change: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env.update(
            {
                "PATH": os.pathsep.join((str(mock_bin), env.get("PATH", ""))),
                "TEAGRAM_TEST_FIREWALL_STATE": str(state_file),
                "TEAGRAM_TEST_MISSING_NETWORKS": "",
                "TEAGRAM_TEST_IPV6_NETWORKS": "",
                "TEAGRAM_TEST_LISTING_FAILURE": "",
                "TEAGRAM_TEST_INTERLEAVE_RULE_CHANGE": "1" if interleave_rule_change else "0",
                "TEAGRAM_TEST_DOCKER_READY_AFTER": str(docker_ready_after),
                "TEAGRAM_TEST_DOCKER_INFO_MODE": docker_info_mode,
                "TEAGRAM_TEST_DOCKER_INFO_COUNT": str(
                    state_file.with_name("docker-info-count")
                ),
                "TEAGRAM_TEST_DOCKER_CALL_LOG": str(
                    state_file.with_name("docker-calls.log")
                ),
            }
        )
        return subprocess.run(
            [
                "bash",
                "-c",
                'source "$1"; require_root() { :; }; apply_rules',
                "test-apply-rules",
                str(SCRIPT),
            ],
            capture_output=True,
            text=True,
            env=env,
        )

    def _run_wait_for_docker_function(
        self,
        mock_bin: pathlib.Path,
        state_file: pathlib.Path,
        *,
        docker_info_mode: str,
        ready_timeout_seconds: int,
        probe_timeout_seconds: int,
        wall_timeout_seconds: int,
    ) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env.update(
            {
                "PATH": os.pathsep.join((str(mock_bin), env.get("PATH", ""))),
                "TEAGRAM_TEST_FIREWALL_STATE": str(state_file),
                "TEAGRAM_TEST_DOCKER_READY_AFTER": "1",
                "TEAGRAM_TEST_DOCKER_INFO_MODE": docker_info_mode,
                "TEAGRAM_TEST_DOCKER_INFO_COUNT": str(
                    state_file.with_name("docker-info-count")
                ),
                "TEAGRAM_TEST_DOCKER_CALL_LOG": str(
                    state_file.with_name("docker-calls.log")
                ),
            }
        )
        return subprocess.run(
            [
                "bash",
                "-c",
                'source "$1"; wait_for_docker "$2" "$3"',
                "test-wait-for-docker",
                str(SCRIPT),
                str(ready_timeout_seconds),
                str(probe_timeout_seconds),
            ],
            capture_output=True,
            text=True,
            env=env,
            timeout=wall_timeout_seconds,
        )

    def _firewall_listings(
        self, mock_bin: pathlib.Path, state_file: pathlib.Path
    ) -> tuple[str, ...]:
        env = os.environ.copy()
        env["PATH"] = os.pathsep.join((str(mock_bin), env.get("PATH", "")))
        env["TEAGRAM_TEST_FIREWALL_STATE"] = str(state_file)
        listings = []
        for binary in ("iptables", "ip6tables"):
            for chain in ("FORWARD", "DOCKER-USER", "INPUT", IPTABLES_CHAIN, INPUT_CHAIN):
                result = subprocess.run(
                    [str(mock_bin / binary), "-w", "10", "-S", chain],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=env,
                )
                listings.append(
                    result.stdout
                    if result.returncode == 0
                    else f"missing:{result.stderr.strip()}"
                )
        return tuple(listings)

    @unittest.skipUnless(
        os.geteuid() == 0 or shutil.which("sudo"),
        "fake firewall integration requires root or passwordless sudo",
    )
    def test_apply_is_idempotent_and_check_is_read_only(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            first_apply = self._run_firewall_mode(
                mock_bin,
                state_file,
                "apply-rules",
                ipv6_networks=DEMO_NETWORKS,
            )
            self.assertEqual(first_apply.returncode, 0, first_apply.stderr)
            state_after_first_apply = state_file.read_bytes()
            listings_after_first_apply = self._firewall_listings(mock_bin, state_file)

            second_apply = self._run_firewall_mode(
                mock_bin,
                state_file,
                "apply-rules",
                ipv6_networks=DEMO_NETWORKS,
            )
            self.assertEqual(second_apply.returncode, 0, second_apply.stderr)
            self.assertEqual(state_file.read_bytes(), state_after_first_apply)
            self.assertEqual(
                self._firewall_listings(mock_bin, state_file), listings_after_first_apply
            )

            check = self._run_firewall_mode(
                mock_bin, state_file, "check", ipv6_networks=DEMO_NETWORKS
            )
            self.assertEqual(check.returncode, 0, check.stderr)
            self.assertEqual(state_file.read_bytes(), state_after_first_apply)

            remove = self._run_firewall_mode(mock_bin, state_file, "remove")
            self.assertEqual(remove.returncode, 0, remove.stderr)
            state = json.loads(state_file.read_text(encoding="utf-8"))
            for family in ("iptables", "ip6tables"):
                self.assertNotIn(IPTABLES_CHAIN, state[family])
                self.assertNotIn(INPUT_CHAIN, state[family])
                self.assertFalse(
                    any(
                        rule.endswith((f"-j {IPTABLES_CHAIN}", f"-j {INPUT_CHAIN}"))
                        for chain in ("DOCKER-USER", "INPUT", "FORWARD")
                        for rule in state[family][chain]
                    )
                )

    def test_apply_waits_for_docker_api_before_inspecting_networks(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)

            result = self._run_apply_rules_function(
                mock_bin, state_file, docker_ready_after=4
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            count_path = state_file.with_name("docker-info-count")
            self.assertEqual(count_path.read_text(encoding="utf-8"), "4")
            calls = state_file.with_name("docker-calls.log").read_text(
                encoding="utf-8"
            ).splitlines()
            self.assertEqual(calls[:4], ["info"] * 4)
            self.assertTrue(calls[4].startswith("network ls "), calls[4])

    def test_apply_fails_clearly_when_docker_never_becomes_ready(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            before = state_file.read_bytes()

            result = self._run_apply_rules_function(
                mock_bin, state_file, docker_ready_after=61
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn(
                "Docker daemon did not become ready within 60 seconds", result.stderr
            )
            self.assertEqual(
                state_file.with_name("docker-info-count").read_text(encoding="utf-8"),
                "60",
            )
            calls = state_file.with_name("docker-calls.log").read_text(
                encoding="utf-8"
            ).splitlines()
            self.assertEqual(calls, ["info"] * 60)
            self.assertEqual(state_file.read_bytes(), before)

    def test_wait_for_docker_bounds_immediate_failures_by_deadline(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)

            result = self._run_wait_for_docker_function(
                mock_bin,
                state_file,
                docker_info_mode="fail",
                ready_timeout_seconds=2,
                probe_timeout_seconds=1,
                wall_timeout_seconds=4,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn(
                "Docker daemon did not become ready within 2 seconds", result.stderr
            )
            calls = state_file.with_name("docker-info-count").read_text(
                encoding="utf-8"
            )
            self.assertGreater(int(calls), 0)

    def test_wait_for_docker_bounds_stalled_docker_info(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)

            result = self._run_wait_for_docker_function(
                mock_bin,
                state_file,
                docker_info_mode="stall",
                ready_timeout_seconds=2,
                probe_timeout_seconds=5,
                wall_timeout_seconds=4,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn(
                "Docker daemon did not become ready within 2 seconds", result.stderr
            )
            calls = state_file.with_name("docker-info-count").read_text(
                encoding="utf-8"
            )
            self.assertEqual(calls, "1")

    def test_supervisor_bounds_stalled_docker_info_probe(self) -> None:
        shell = r'''
source "$1"
require_root() { :; }
require_repo_path() { :; }
stack_lock_try() { STACK_LOCK_SKIPPED=0; STACK_LOCK_ACQUIRED=0; }
stack_start_locked() { :; }
stack_reconcile_locked() { :; }
stack_stop() { :; }
sleep() { kill -TERM "$BASHPID"; }
stack_supervise
'''
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            env = os.environ.copy()
            env.update(
                {
                    "PATH": os.pathsep.join((str(mock_bin), env.get("PATH", ""))),
                    "TEAGRAM_TEST_FIREWALL_STATE": str(state_file),
                    "TEAGRAM_TEST_DOCKER_INFO_MODE": "stall",
                    "TEAGRAM_TEST_DOCKER_INFO_COUNT": str(
                        state_file.with_name("docker-info-count")
                    ),
                    "TEAGRAM_TEST_DOCKER_CALL_LOG": str(
                        state_file.with_name("docker-calls.log")
                    ),
                }
            )
            try:
                result = subprocess.run(
                    ["bash", "-c", shell, "test-stack-supervisor", str(SCRIPT)],
                    capture_output=True,
                    text=True,
                    env=env,
                    timeout=8,
                )
            except subprocess.TimeoutExpired:
                self.fail("stack supervisor blocked indefinitely in docker info")

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Docker daemon is unavailable", result.stdout)
            calls = state_file.with_name("docker-info-count").read_text(
                encoding="utf-8"
            )
            self.assertEqual(calls, "1")

    def test_supervisor_reapplies_rules_after_tailscale_forward_hook(self) -> None:
        shell = r'''
source "$1"
require_root() { :; }
require_repo_path() { :; }
stack_lock_try() { STACK_LOCK_SKIPPED=0; STACK_LOCK_ACQUIRED=0; }
stack_start_locked() { :; }
stack_reconcile_locked() { :; }
stack_stop() { :; }
sleep() { kill -TERM "$BASHPID"; }
stack_supervise
'''
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            apply = self._run_apply_rules_function(
                mock_bin, state_file, docker_ready_after=1
            )
            self.assertEqual(apply.returncode, 0, apply.stderr)

            env = os.environ.copy()
            env.update(
                {
                    "PATH": os.pathsep.join((str(mock_bin), env.get("PATH", ""))),
                    "TEAGRAM_TEST_FIREWALL_STATE": str(state_file),
                    "TEAGRAM_TEST_DOCKER_READY_AFTER": "1",
                    "TEAGRAM_TEST_DOCKER_INFO_MODE": "ready",
                    "TEAGRAM_TEST_DOCKER_INFO_COUNT": str(
                        state_file.with_name("docker-info-count")
                    ),
                    "TEAGRAM_TEST_DOCKER_CALL_LOG": str(
                        state_file.with_name("docker-calls.log")
                    ),
                }
            )
            state = json.loads(state_file.read_text(encoding="utf-8"))
            state["iptables"]["ts-forward"] = [
                "-A ts-forward -o tailscale0 -j ACCEPT"
            ]
            state_file.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")
            tailscale_hook = subprocess.run(
                [
                    str(mock_bin / "iptables"),
                    "-w",
                    "10",
                    "-I",
                    "FORWARD",
                    "1",
                    "-j",
                    "ts-forward",
                ],
                capture_output=True,
                text=True,
                env=env,
            )
            self.assertEqual(tailscale_hook.returncode, 0, tailscale_hook.stderr)

            result = subprocess.run(
                ["bash", "-c", shell, "test-stack-supervisor", str(SCRIPT)],
                capture_output=True,
                text=True,
                env=env,
                timeout=8,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            state = json.loads(state_file.read_text(encoding="utf-8"))
            expected_hooks = [
                f"-A FORWARD -s {subnet} -j {IPTABLES_CHAIN}"
                for subnet in (
                    "198.18.130.0/24",
                    "198.18.132.0/24",
                    "198.18.133.0/24",
                )
            ]
            self.assertEqual(state["iptables"]["FORWARD"][:3], expected_hooks)
            self.assertGreater(
                state["iptables"]["FORWARD"].index("-A FORWARD -j ts-forward"), 2
            )
            self.assertEqual(
                state["iptables"]["ts-forward"],
                ["-A ts-forward -o tailscale0 -j ACCEPT"],
            )

    @unittest.skipUnless(
        os.geteuid() == 0 or shutil.which("sudo"),
        "fake firewall integration requires root or passwordless sudo",
    )
    def test_render_failure_aborts_apply_without_changing_input_chain(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            python = mock_bin / "python3"
            python.write_text(
                f"#!{sys.executable}\n"
                "import os\n"
                "import sys\n"
                "if sys.argv[1:2] == ['-']:\n"
                "    raise SystemExit(1)\n"
                f"real_python = {sys.executable!r}\n"
                "os.execv(real_python, [real_python, *sys.argv[1:]])\n",
                encoding="utf-8",
            )
            python.chmod(0o755)
            before = json.loads(state_file.read_text(encoding="utf-8"))

            result = self._run_firewall_mode(mock_bin, state_file, "apply-rules")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("cannot derive the bridge gateway", result.stderr)
            after = json.loads(state_file.read_text(encoding="utf-8"))
            self.assertEqual(after["iptables"]["INPUT"], before["iptables"]["INPUT"])
            self.assertEqual(after, before)

    @unittest.skipUnless(
        os.geteuid() == 0 or shutil.which("sudo"),
        "fake firewall integration requires root or passwordless sudo",
    )
    def test_inspection_errors_abort_apply_and_remove_without_mutation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            apply = self._run_firewall_mode(mock_bin, state_file, "apply-rules")
            self.assertEqual(apply.returncode, 0, apply.stderr)

            before_remove = state_file.read_bytes()
            remove = self._run_firewall_mode(
                mock_bin,
                state_file,
                "remove",
                listing_failure="iptables:INPUT",
            )
            self.assertNotEqual(remove.returncode, 0)
            self.assertIn("could not inspect INPUT", remove.stderr)
            self.assertIn("Resource temporarily unavailable", remove.stderr)
            self.assertEqual(state_file.read_bytes(), before_remove)

            before_apply = state_file.read_bytes()
            apply = self._run_firewall_mode(
                mock_bin,
                state_file,
                "apply-rules",
                listing_failure=f"iptables:{IPTABLES_CHAIN}",
            )
            self.assertNotEqual(apply.returncode, 0)
            self.assertIn("could not inspect", apply.stderr)
            self.assertEqual(state_file.read_bytes(), before_apply)

    @unittest.skipUnless(
        os.geteuid() == 0 or shutil.which("sudo"),
        "fake firewall integration requires root or passwordless sudo",
    )
    def test_check_rejects_drift_missing_duplicate_and_misordered_hooks_without_mutation(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            apply = self._run_firewall_mode(mock_bin, state_file, "apply-rules")
            self.assertEqual(apply.returncode, 0, apply.stderr)
            baseline = json.loads(state_file.read_text(encoding="utf-8"))

            cases = (
                "drifted chain rule",
                "missing input hook",
                "unscoped input hook",
                "duplicate docker hook",
                "misordered input hooks",
            )
            for case in cases:
                with self.subTest(case=case):
                    state = json.loads(json.dumps(baseline))
                    if case == "drifted chain rule":
                        state["iptables"][IPTABLES_CHAIN][3] = (
                            f"-A {IPTABLES_CHAIN} -d 10.0.0.0/8 -j RETURN"
                        )
                    elif case == "missing input hook":
                        state["iptables"]["INPUT"].pop(0)
                    elif case == "unscoped input hook":
                        state["iptables"]["INPUT"][0] = (
                            f"-A INPUT -s 198.18.130.0/24 -j {INPUT_CHAIN}"
                        )
                    elif case == "duplicate docker hook":
                        state["iptables"]["DOCKER-USER"].insert(
                            0, state["iptables"]["DOCKER-USER"][0]
                        )
                    else:
                        state["iptables"]["INPUT"][0:2] = reversed(
                            state["iptables"]["INPUT"][0:2]
                        )
                    state_file.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")
                    before_check = state_file.read_bytes()

                    result = self._run_firewall_mode(mock_bin, state_file, "check")

                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(state_file.read_bytes(), before_check)

    @unittest.skipUnless(
        os.geteuid() == 0 or shutil.which("sudo"),
        "fake firewall integration requires root or passwordless sudo",
    )
    def test_check_rejects_shadowed_forward_hook_without_mutation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            apply = self._run_firewall_mode(mock_bin, state_file, "apply-rules")
            self.assertEqual(apply.returncode, 0, apply.stderr)
            state = json.loads(state_file.read_text(encoding="utf-8"))
            state["iptables"]["FORWARD"].insert(0, "-A FORWARD -j ACCEPT")
            state_file.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")
            before_check = state_file.read_bytes()

            result = self._run_firewall_mode(mock_bin, state_file, "check")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("FORWARD isolation hooks are missing or out of order", result.stderr)
            self.assertEqual(state_file.read_bytes(), before_check)

    @unittest.skipUnless(
        os.geteuid() == 0 or shutil.which("sudo"),
        "fake firewall integration requires root or passwordless sudo",
    )
    def test_apply_and_remove_preserve_existing_host_rule_order(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            initial = json.loads(state_file.read_text(encoding="utf-8"))
            host_rules = {
                family: {
                    chain: [
                        rule
                        for rule in initial[family][chain]
                        if not rule.endswith((f"-j {IPTABLES_CHAIN}", f"-j {INPUT_CHAIN}"))
                    ]
                    for chain in ("FORWARD", "DOCKER-USER", "INPUT")
                }
                for family in ("iptables", "ip6tables")
            }

            result = self._run_firewall_mode(
                mock_bin, state_file, "apply-rules", ipv6_networks=DEMO_NETWORKS
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            state = json.loads(state_file.read_text(encoding="utf-8"))
            subnets_by_family = {
                "iptables": ("198.18.130.0/24", "198.18.132.0/24", "198.18.133.0/24"),
                "ip6tables": ("fd42:100::/64", "fd42:101::/64", "fd42:102::/64"),
            }
            for family, subnets in subnets_by_family.items():
                hooks_by_chain = {
                    "FORWARD": [
                        f"-A FORWARD -s {subnet} -j {IPTABLES_CHAIN}"
                        for subnet in subnets
                    ],
                    "DOCKER-USER": [
                        f"-A DOCKER-USER -s {subnet} -j {IPTABLES_CHAIN}"
                        for subnet in subnets
                    ],
                    "INPUT": [
                        f"-A INPUT -i br+ -s {subnet} -j {INPUT_CHAIN}"
                        for subnet in subnets
                    ],
                }
                for chain, hooks in hooks_by_chain.items():
                    self.assertEqual(state[family][chain], hooks + host_rules[family][chain])

            remove = self._run_firewall_mode(mock_bin, state_file, "remove")

            self.assertEqual(remove.returncode, 0, remove.stderr)
            state = json.loads(state_file.read_text(encoding="utf-8"))
            for family in subnets_by_family:
                for chain, expected_host_rules in host_rules[family].items():
                    self.assertEqual(state[family][chain], expected_host_rules)

    def test_apply_preserves_foreign_rule_inserted_between_listing_and_delete(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)

            result = self._run_apply_rules_function(
                mock_bin,
                state_file,
                docker_ready_after=1,
                interleave_rule_change=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            marker = state_file.with_name("interleaved-rule-change")
            self.assertEqual(marker.read_text(encoding="utf-8"), "inserted")
            state = json.loads(state_file.read_text(encoding="utf-8"))
            self.assertIn(
                "-A DOCKER-USER -m comment --comment injected-foreign -j ACCEPT",
                state["iptables"]["DOCKER-USER"],
            )
            self.assertEqual(
                state["iptables"]["DOCKER-USER"][:3],
                [
                    f"-A DOCKER-USER -s {subnet} -j {IPTABLES_CHAIN}"
                    for subnet in (
                        "198.18.130.0/24",
                        "198.18.132.0/24",
                        "198.18.133.0/24",
                    )
                ],
            )

    def test_stack_reconciler_recovers_missing_services_and_leaves_migrate_stopped(
        self,
    ) -> None:
        shell = r'''
source "$1"
active_release_image_ref() { printf '%s\n' "$TEAGRAM_TEST_ACTIVE_IMAGE_REF"; }
compose_stack() {
  local image_ref=$1
  shift
  if [[ "$1" == ps ]]; then
    cat "$TEAGRAM_TEST_RUNNING_SERVICES"
  elif [[ "$1" == up ]]; then
    local command="$*"
    local service
    local -a services=()
    shift
    while (( $# > 0 )); do
      if [[ "$1" == --wait-timeout ]]; then
        shift 2
      elif [[ "$1" == -* ]]; then
        shift
      else
        services+=("$1")
        shift
      fi
    done
    if (( ${#services[@]} == 0 )); then
      services=(postgres migrate teagramd turnstile-egress haproxy edge cloudflared)
    fi
    for service in "${services[@]}"; do
      if ! grep -Fqx "$service" "$TEAGRAM_TEST_RUNNING_SERVICES"; then
        printf '%s\n' "$service" >> "$TEAGRAM_TEST_RUNNING_SERVICES"
      fi
    done
    printf 'compose:%s:%s\n' "$image_ref" "$command" >> "$TEAGRAM_TEST_STACK_LOG"
  else
    return 2
  fi
}
apply_rules() { printf '%s\n' firewall >> "$TEAGRAM_TEST_STACK_LOG"; }
stack_lock_release() { :; }
stack_reconcile_locked
'''
        with tempfile.TemporaryDirectory() as directory:
            log = pathlib.Path(directory) / "reconcile.log"
            running_services = pathlib.Path(directory) / "running-services"
            running_services.write_text(
                "\n".join(("postgres", "teagramd", "turnstile-egress", "haproxy", "edge"))
                + "\n",
                encoding="utf-8",
            )
            active_ref = "ghcr.io/teagramhq/teagram-server-demo@sha256:" + "c" * 64
            env = os.environ.copy()
            env["TEAGRAM_TEST_ACTIVE_IMAGE_REF"] = active_ref
            env["TEAGRAM_TEST_STACK_LOG"] = str(log)
            env["TEAGRAM_TEST_RUNNING_SERVICES"] = str(running_services)
            result = subprocess.run(
                ["bash", "-c", shell, "test-stack-reconcile", str(SCRIPT)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                log.read_text(encoding="utf-8").splitlines(),
                [
                    "firewall",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 cloudflared",
                ],
            )
            running = set(running_services.read_text(encoding="utf-8").splitlines())
            self.assertIn("cloudflared", running)
            self.assertNotIn("migrate", running)

    def test_stack_reconciler_recovers_docker_restart_in_dependency_order(self) -> None:
        shell = r'''
source "$1"
active_release_image_ref() { printf '%s\n' "$TEAGRAM_TEST_ACTIVE_IMAGE_REF"; }
compose_stack() {
  local image_ref=$1
  shift
  if [[ "$1" == ps ]]; then
    cat "$TEAGRAM_TEST_RUNNING_SERVICES"
  elif [[ "$1" == up ]]; then
    printf 'compose:%s:%s\n' "$image_ref" "$*" >> "$TEAGRAM_TEST_STACK_LOG"
    local service
    shift
    while (( $# > 0 )); do
      if [[ "$1" == --wait-timeout ]]; then
        shift 2
      elif [[ "$1" == -* ]]; then
        shift
      else
        service=$1
        printf '%s\n' "$service" >> "$TEAGRAM_TEST_RUNNING_SERVICES"
        shift
      fi
    done
  else
    return 2
  fi
}
apply_rules() { printf '%s\n' firewall >> "$TEAGRAM_TEST_STACK_LOG"; }
stack_lock_release() { :; }
stack_reconcile_locked
'''
        with tempfile.TemporaryDirectory() as directory:
            log = pathlib.Path(directory) / "reconcile.log"
            running_services = pathlib.Path(directory) / "running-services"
            running_services.write_text("", encoding="utf-8")
            active_ref = "ghcr.io/teagramhq/teagram-server-demo@sha256:" + "c" * 64
            env = os.environ.copy()
            env["TEAGRAM_TEST_ACTIVE_IMAGE_REF"] = active_ref
            env["TEAGRAM_TEST_STACK_LOG"] = str(log)
            env["TEAGRAM_TEST_RUNNING_SERVICES"] = str(running_services)

            result = subprocess.run(
                ["bash", "-c", shell, "test-stack-reconcile", str(SCRIPT)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                log.read_text(encoding="utf-8").splitlines(),
                [
                    "firewall",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 postgres",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 turnstile-egress",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 teagramd",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 haproxy",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 edge",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 cloudflared",
                ],
            )
            self.assertEqual(
                set(running_services.read_text(encoding="utf-8").splitlines()),
                {
                    "postgres",
                    "turnstile-egress",
                    "haproxy",
                    "teagramd",
                    "edge",
                    "cloudflared",
                },
            )
            self.assertNotIn("migrate", running_services.read_text(encoding="utf-8"))

    def test_supervisor_restart_after_recovery_failure_leaves_migrate_stopped(self) -> None:
        shell = r'''
source "$1"
active_release_image_ref() { printf '%s\n' "$TEAGRAM_TEST_ACTIVE_IMAGE_REF"; }
disable_existing_restart_policies() { :; }
compose_stack() {
  local image_ref=$1
  shift
  local operation=$1
  shift
  printf 'compose:%s:%s %s\n' "$image_ref" "$operation" "$*" >> "$TEAGRAM_TEST_STACK_LOG"
  if [[ $operation == create ]]; then
    return 0
  elif [[ $operation == ps ]]; then
    cat "$TEAGRAM_TEST_RUNNING_SERVICES"
  elif [[ $operation == up ]]; then
    local service
    local -a services=()
    while (( $# > 0 )); do
      if [[ $1 == --wait-timeout ]]; then
        shift 2
      elif [[ $1 == -* ]]; then
        shift
      else
        services+=("$1")
        shift
      fi
    done
    if (( ${#services[@]} == 0 )); then
      services=(postgres migrate turnstile-egress teagramd haproxy edge cloudflared)
    fi
    for service in "${services[@]}"; do
      if [[ $service == "$TEAGRAM_TEST_FAIL_SERVICE" && ! -e $TEAGRAM_TEST_FAIL_ONCE_FILE ]]; then
        : > "$TEAGRAM_TEST_FAIL_ONCE_FILE"
        return 1
      fi
      if ! grep -Fqx "$service" "$TEAGRAM_TEST_RUNNING_SERVICES"; then
        printf '%s\n' "$service" >> "$TEAGRAM_TEST_RUNNING_SERVICES"
      fi
    done
  else
    return 2
  fi
}
apply_rules() { printf '%s\n' firewall >> "$TEAGRAM_TEST_STACK_LOG"; }
stack_lock_release() { :; }
stack_start_locked
'''
        with tempfile.TemporaryDirectory() as directory:
            log = pathlib.Path(directory) / "supervisor-restart.log"
            running_services = pathlib.Path(directory) / "running-services"
            running_services.write_text(
                "\n".join(("postgres", "turnstile-egress", "teagramd")) + "\n",
                encoding="utf-8",
            )
            failed_once = pathlib.Path(directory) / "failed-once"
            active_ref = "ghcr.io/teagramhq/teagram-server-demo@sha256:" + "e" * 64
            env = os.environ.copy()
            env["TEAGRAM_TEST_ACTIVE_IMAGE_REF"] = active_ref
            env["TEAGRAM_TEST_FAIL_SERVICE"] = "haproxy"
            env["TEAGRAM_TEST_FAIL_ONCE_FILE"] = str(failed_once)
            env["TEAGRAM_TEST_STACK_LOG"] = str(log)
            env["TEAGRAM_TEST_RUNNING_SERVICES"] = str(running_services)

            failed_recovery = subprocess.run(
                ["bash", "-c", shell, "test-supervisor-restart", str(SCRIPT)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )
            self.assertNotEqual(failed_recovery.returncode, 0)
            self.assertIn("teagramd", running_services.read_text(encoding="utf-8"))
            self.assertNotIn("migrate", running_services.read_text(encoding="utf-8"))

            restarted_supervisor = subprocess.run(
                ["bash", "-c", shell, "test-supervisor-restart", str(SCRIPT)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(restarted_supervisor.returncode, 0, restarted_supervisor.stderr)
            running = set(running_services.read_text(encoding="utf-8").splitlines())
            self.assertEqual(
                running,
                {
                    "postgres",
                    "turnstile-egress",
                    "teagramd",
                    "haproxy",
                    "edge",
                    "cloudflared",
                },
            )
            up_calls = [
                line
                for line in log.read_text(encoding="utf-8").splitlines()
                if ":up " in line
            ]
            self.assertEqual(
                [line.rsplit(" ", 1)[-1] for line in up_calls],
                ["haproxy"] * 2 + ["edge", "cloudflared"],
            )
            self.assertTrue(
                all("--no-deps --wait --wait-timeout 120 " in line for line in up_calls)
            )

    def test_stack_start_uses_active_digest_and_applies_rules_before_recovery(self) -> None:
        shell = r'''
source "$1"
active_release_image_ref() { printf '%s\n' "$TEAGRAM_TEST_ACTIVE_IMAGE_REF"; }
disable_existing_restart_policies() {
  printf '%s\n' disable-restart-policies >> "$TEAGRAM_TEST_STACK_LOG"
}
compose_stack() {
  local image_ref=$1
  shift
  printf 'compose:%s:%s\n' "$image_ref" "$*" >> "$TEAGRAM_TEST_STACK_LOG"
  if [[ "$1" == ps ]]; then
    printf '%s\n' postgres turnstile-egress haproxy edge cloudflared
  fi
}
apply_rules() { printf '%s\n' firewall >> "$TEAGRAM_TEST_STACK_LOG"; }
stack_lock_release() { :; }
stack_start_locked
'''
        with tempfile.TemporaryDirectory() as directory:
            log = pathlib.Path(directory) / "stack-start.log"
            active_ref = "ghcr.io/teagramhq/teagram-server-demo@sha256:" + "d" * 64
            env = os.environ.copy()
            env["TEAGRAM_TEST_ACTIVE_IMAGE_REF"] = active_ref
            env["TEAGRAM_TEST_STACK_LOG"] = str(log)
            result = subprocess.run(
                ["bash", "-c", shell, "test-stack-start", str(SCRIPT)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                log.read_text(encoding="utf-8").splitlines(),
                [
                    "disable-restart-policies",
                    f"compose:{active_ref}:create --no-build",
                    "firewall",
                    f"compose:{active_ref}:ps --status running --services",
                    "firewall",
                    f"compose:{active_ref}:up -d --no-build --no-deps "
                    "--wait --wait-timeout 120 teagramd",
                ],
            )

    def test_stack_start_runs_project_up_for_bootstrap_without_active_release(self) -> None:
        shell = r'''
source "$1"
active_release_image_ref() { printf '\n'; }
disable_existing_restart_policies() { :; }
compose_stack() {
  printf 'compose:%s:%s\n' "${1:-}" "${*:2}" >> "$TEAGRAM_TEST_STACK_LOG"
}
apply_rules() { printf '%s\n' firewall >> "$TEAGRAM_TEST_STACK_LOG"; }
stack_lock_release() { :; }
stack_start_locked
'''
        with tempfile.TemporaryDirectory() as directory:
            log = pathlib.Path(directory) / "bootstrap-start.log"
            env = os.environ.copy()
            env["TEAGRAM_TEST_STACK_LOG"] = str(log)
            result = subprocess.run(
                ["bash", "-c", shell, "test-bootstrap-stack-start", str(SCRIPT)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                log.read_text(encoding="utf-8").splitlines(),
                [
                    "compose::create --no-build",
                    "firewall",
                    "compose::up -d --no-build",
                ],
            )

    def test_active_release_digest_overrides_bootstrap_image_for_compose(self) -> None:
        active_ref = "ghcr.io/teagramhq/teagram-server-demo@sha256:" + "c" * 64
        bootstrap_ref = "ghcr.io/teagramhq/teagram-server-demo@sha256:" + "b" * 64
        shell = r'''
source "$1"
active_ref=$(active_release_image_ref "$2")
compose_stack "$active_ref" config
'''
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            mock_bin = root / "bin"
            mock_bin.mkdir()
            env_file = root / "lima.env"
            env_file.write_text(
                f"TEAGRAM_DEMO_IMAGE_REF={bootstrap_ref}\n", encoding="utf-8"
            )
            active_file = root / "active-release.json"
            active_file.write_text('{"image_ref":"fixture"}\n', encoding="utf-8")
            (mock_bin / "stat").write_text(
                "#!/bin/sh\n"
                "if [ \"$1\" = -c ] && [ \"$2\" = '%u:%a' ]; then\n"
                "  printf '0:600\\n'\n"
                "else\n"
                f"  exec {shutil.which('stat')} \"$@\"\n"
                "fi\n",
                encoding="utf-8",
            )
            (mock_bin / "jq").write_text(
                "#!/bin/sh\nprintf '%s\\n' \"$TEAGRAM_TEST_ACTIVE_IMAGE_REF\"\n",
                encoding="utf-8",
            )
            (mock_bin / "docker").write_text(
                "#!/usr/bin/env python3\n"
                "import os, pathlib, sys\n"
                "ref = os.environ.get('TEAGRAM_DEMO_IMAGE_REF')\n"
                "if ref is None:\n"
                "    args = sys.argv[1:]\n"
                "    env_file = pathlib.Path(args[args.index('--env-file') + 1])\n"
                "    ref = next(\n"
                "        (line.split('=', 1)[1] for line in env_file.read_text().splitlines()\n"
                "         if line.startswith('TEAGRAM_DEMO_IMAGE_REF=')),\n"
                "        '',\n"
                "    )\n"
                "print(ref)\n",
                encoding="utf-8",
            )
            for name in ("stat", "jq", "docker"):
                (mock_bin / name).chmod(0o755)

            env = os.environ.copy()
            env.update(
                {
                    "PATH": os.pathsep.join((str(mock_bin), env.get("PATH", ""))),
                    "TEAGRAM_DEMO_LIMA_ENV_FILE": str(env_file),
                    "TEAGRAM_TEST_ACTIVE_IMAGE_REF": active_ref,
                }
            )
            result = subprocess.run(
                ["bash", "-c", shell, "test-active-image-ref", str(SCRIPT), str(active_file)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), active_ref)
            self.assertNotEqual(result.stdout.strip(), bootstrap_ref)

            no_active_release = subprocess.run(
                [
                    "bash",
                    "-c",
                    shell,
                    "test-bootstrap-image-ref",
                    str(SCRIPT),
                    str(root / "missing-active-release.json"),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )
            self.assertEqual(no_active_release.returncode, 0, no_active_release.stderr)
            self.assertEqual(no_active_release.stdout.strip(), bootstrap_ref)

    @unittest.skipUnless(
        os.geteuid() == 0 or shutil.which("sudo"),
        "fake firewall integration requires root or passwordless sudo",
    )
    def test_missing_networks_are_named_and_existing_ipv6_rules_are_preserved(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mock_bin, state_file = self._mock_firewall(directory)
            state = json.loads(state_file.read_text(encoding="utf-8"))
            ipv6_subnets = (EXAMPLE_IPV6_SUBNET,)
            state["ip6tables"][IPTABLES_CHAIN] = [
                rule
                for rule in render("v6", *ipv6_subnets)
                if rule.startswith(f"-A {IPTABLES_CHAIN} ")
            ]
            for subnet in ipv6_subnets:
                state["ip6tables"]["DOCKER-USER"].append(
                    f"-A DOCKER-USER -s {subnet} -j {IPTABLES_CHAIN}"
                )
                state["ip6tables"]["INPUT"].append(
                    f"-A INPUT -i br+ -s {subnet} -j {INPUT_CHAIN}"
                )
            state_file.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")
            ipv6_before = json.dumps(state["ip6tables"], sort_keys=True)

            result = self._run_firewall_mode(
                mock_bin,
                state_file,
                "apply-rules",
                missing_networks=(DEMO_NETWORKS[-1],),
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn(DEMO_NETWORKS[-1], result.stderr)
            self.assertEqual(
                json.dumps(
                    json.loads(state_file.read_text(encoding="utf-8"))["ip6tables"],
                    sort_keys=True,
                ),
                ipv6_before,
            )
            check = self._run_firewall_mode(
                mock_bin,
                state_file,
                "check",
                missing_networks=(DEMO_NETWORKS[-1],),
            )
            self.assertNotEqual(check.returncode, 0)
            self.assertIn(DEMO_NETWORKS[-1], check.stderr)

    def test_input_hook_only_matches_demo_traffic_arriving_from_bridge(self) -> None:
        rules = render("v4", EXAMPLE_IPV4_SUBNET)

        input_hooks = [rule for rule in rules if rule.startswith("-A INPUT ")]
        self.assertEqual(
            input_hooks,
            [f"-A INPUT -i br+ -s {EXAMPLE_IPV4_SUBNET} -j {INPUT_CHAIN}"],
        )

    def test_multiple_subnets_render_one_hook_and_self_return_each(self) -> None:
        subnets = (
            "198.18.90.0/24",
            "198.18.91.0/24",
            "198.18.92.0/24",
        )
        rules = render("v4", *subnets)

        self.assertEqual(
            rules[: 3 * len(subnets)],
            [
                hook
                for subnet in subnets
                for hook in (
                    f"-A FORWARD -s {subnet} -j {IPTABLES_CHAIN}",
                    f"-A DOCKER-USER -s {subnet} -j {IPTABLES_CHAIN}",
                    f"-A INPUT -i br+ -s {subnet} -j {INPUT_CHAIN}",
                )
            ],
        )
        self.assertEqual(
            rules[3 * len(subnets) : 4 * len(subnets)],
            [
                f"-A {IPTABLES_CHAIN} -d {subnet} -j RETURN"
                for subnet in subnets
            ],
        )
        self.assertEqual(
            sum(rule.startswith("-A FORWARD ") for rule in rules),
            len(subnets),
        )
        self.assertEqual(
            sum(rule.startswith("-A DOCKER-USER ") for rule in rules),
            len(subnets),
        )
        self.assertEqual(
            sum(rule.startswith("-A INPUT ") for rule in rules),
            len(subnets),
        )
        self.assertFalse(
            any(
                rule.startswith(f"-A {INPUT_CHAIN} -d {subnet} -j RETURN")
                for rule in rules
                for subnet in subnets
            )
        )
        self.assertTrue(
            all(rules.count(rule) == 1 for rule in rules[: 4 * len(subnets)])
        )
        self.assertFalse(any("," in rule for rule in rules[: 4 * len(subnets)]))

    def test_ipv4_drops_every_private_and_multicast_destination_for_demo_source(self) -> None:
        rules = render("v4", EXAMPLE_IPV4_SUBNET)

        self.assertEqual(
            rules[0],
            f"-A FORWARD -s {EXAMPLE_IPV4_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[1],
            f"-A DOCKER-USER -s {EXAMPLE_IPV4_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[2],
            f"-A INPUT -i br+ -s {EXAMPLE_IPV4_SUBNET} -j {INPUT_CHAIN}",
        )
        self.assertEqual(
            rules[3],
            f"-A {IPTABLES_CHAIN} -d {EXAMPLE_IPV4_SUBNET} -j RETURN",
        )
        for destination in PRIVATE_IPV4_CIDRS:
            self.assertIn(
                f"-A {IPTABLES_CHAIN} -d {destination} -j DROP",
                rules,
            )
            self.assertIn(
                f"-A {INPUT_CHAIN} -d {destination} -j DROP",
                rules,
            )
        self.assertIn(
            f"-A {INPUT_CHAIN} -d {EXAMPLE_IPV4_GATEWAY}/32 -j DROP",
            rules,
        )
        self.assertFalse(
            any(
                rule.startswith(f"-A {INPUT_CHAIN} -d {EXAMPLE_IPV4_SUBNET} -j RETURN")
                for rule in rules
            )
        )
        self.assertIn(f"-A {IPTABLES_CHAIN} -j RETURN", rules)
        self.assertEqual(rules[-1], f"-A {INPUT_CHAIN} -j RETURN")
        self.assertIn(
            f"-A {IPTABLES_CHAIN} -m conntrack "
            "--ctstate RELATED,ESTABLISHED -j RETURN",
            rules,
        )
        self.assertFalse(any("ACCEPT" in rule or "REJECT" in rule for rule in rules))

    def test_ipv6_drops_ula_link_local_and_multicast_when_enabled(self) -> None:
        rules = render("v6", EXAMPLE_IPV6_SUBNET)

        self.assertEqual(
            rules[0],
            f"-A FORWARD -s {EXAMPLE_IPV6_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[1],
            f"-A DOCKER-USER -s {EXAMPLE_IPV6_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[2],
            f"-A INPUT -i br+ -s {EXAMPLE_IPV6_SUBNET} -j {INPUT_CHAIN}",
        )
        self.assertEqual(
            rules[3],
            f"-A {IPTABLES_CHAIN} -d {EXAMPLE_IPV6_SUBNET} -j RETURN",
        )
        for destination in PRIVATE_IPV6_CIDRS:
            self.assertIn(
                f"-A {IPTABLES_CHAIN} -d {destination} -j DROP",
                rules,
            )
            self.assertIn(
                f"-A {INPUT_CHAIN} -d {destination} -j DROP",
                rules,
            )
        self.assertIn(f"-A {IPTABLES_CHAIN} -j RETURN", rules)
        self.assertEqual(rules[-1], f"-A {INPUT_CHAIN} -j RETURN")

    def test_established_replies_return_before_private_destination_drops(self) -> None:
        established_rule = (
            f"-A {IPTABLES_CHAIN} -m conntrack "
            "--ctstate RELATED,ESTABLISHED -j RETURN"
        )
        established_input_rule = (
            f"-A {INPUT_CHAIN} -m conntrack "
            "--ctstate RELATED,ESTABLISHED -j RETURN"
        )
        for family, subnet, blocked_cidrs in (
            ("v4", EXAMPLE_IPV4_SUBNET, PRIVATE_IPV4_CIDRS),
            ("v6", EXAMPLE_IPV6_SUBNET, PRIVATE_IPV6_CIDRS),
        ):
            with self.subTest(family=family):
                rules = render(family, subnet)
                established_index = rules.index(established_rule)
                self.assertLess(
                    rules.index(f"-A {IPTABLES_CHAIN} -d {subnet} -j RETURN"),
                    established_index,
                )
                for destination in blocked_cidrs:
                    self.assertLess(
                        established_index,
                        rules.index(
                            f"-A {IPTABLES_CHAIN} -d {destination} -j DROP"
                        ),
                    )
                input_established_index = rules.index(established_input_rule)
                for destination in blocked_cidrs:
                    self.assertLess(
                        input_established_index,
                        rules.index(f"-A {INPUT_CHAIN} -d {destination} -j DROP"),
                    )

    def test_rendering_is_stable_and_contains_no_duplicate_rules(self) -> None:
        first = render("v4", EXAMPLE_IPV4_SUBNET)
        second = render("v4", EXAMPLE_IPV4_SUBNET)

        self.assertEqual(first, second)
        self.assertTrue(all(count == 1 for count in collections.Counter(first).values()))
        self.assertEqual(
            first.count(
                f"-A DOCKER-USER -s {EXAMPLE_IPV4_SUBNET} -j {IPTABLES_CHAIN}"
            ),
            1,
        )

    def test_render_mode_rejects_invalid_subnets_without_firewall_access(self) -> None:
        result = subprocess.run(
            [str(SCRIPT), "--render-v4", "not-a-subnet"],
            capture_output=True,
            text=True,
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("invalid source subnet", result.stderr)


if __name__ == "__main__":
    unittest.main()
