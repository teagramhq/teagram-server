#!/usr/bin/env python3
"""Check Lima egress rules with renderer and fake-firewall integration tests."""

from __future__ import annotations

import collections
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("install-egress-isolation.sh")
IPTABLES_CHAIN = "TEAGRAM_DEMO_EGRESS"
EXAMPLE_IPV4_SUBNET = "198.18.90.0/24"
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

state_path = pathlib.Path(os.environ["TEAGRAM_TEST_FIREWALL_STATE"])
state = json.loads(state_path.read_text(encoding="utf-8"))
tool = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]

if tool == "docker":
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
    chain, index = args[1], int(args[2]) - 1
    if chain not in firewall:
        missing_chain(chain)
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
                    "-A DOCKER-USER -s 198.18.129.0/24 -j TEAGRAM_DEMO_EGRESS"
                ],
                "INPUT": [
                    "-A INPUT -s 198.18.129.0/24 -j TEAGRAM_DEMO_EGRESS"
                ],
                "FORWARD": ["-A FORWARD -j DOCKER-USER"],
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
    ) -> subprocess.CompletedProcess[str]:
        path = os.pathsep.join((str(mock_bin), os.environ.get("PATH", "")))
        env = os.environ.copy()
        env.update(
            {
                "PATH": path,
                "TEAGRAM_TEST_FIREWALL_STATE": str(state_file),
                "TEAGRAM_TEST_MISSING_NETWORKS": ",".join(missing_networks),
                "TEAGRAM_TEST_IPV6_NETWORKS": ",".join(ipv6_networks),
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
                *command,
            ]
        return subprocess.run(command, capture_output=True, text=True, env=env)

    def _firewall_listings(
        self, mock_bin: pathlib.Path, state_file: pathlib.Path
    ) -> tuple[str, ...]:
        env = os.environ.copy()
        env["PATH"] = os.pathsep.join((str(mock_bin), env.get("PATH", "")))
        env["TEAGRAM_TEST_FIREWALL_STATE"] = str(state_file)
        listings = []
        for binary in ("iptables", "ip6tables"):
            for chain in ("DOCKER-USER", "INPUT", IPTABLES_CHAIN):
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
                self.assertFalse(
                    any(
                        rule.endswith(f"-j {IPTABLES_CHAIN}")
                        for chain in ("DOCKER-USER", "INPUT")
                        for rule in state[family][chain]
                    )
                )

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
                    f"-A INPUT -s {subnet} -j {IPTABLES_CHAIN}"
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

    def test_multiple_subnets_render_one_hook_and_self_return_each(self) -> None:
        subnets = (
            "198.18.90.0/24",
            "198.18.91.0/24",
            "198.18.92.0/24",
        )
        rules = render("v4", *subnets)

        self.assertEqual(
            rules[: 2 * len(subnets)],
            [
                hook
                for subnet in subnets
                for hook in (
                    f"-A DOCKER-USER -s {subnet} -j {IPTABLES_CHAIN}",
                    f"-A INPUT -s {subnet} -j {IPTABLES_CHAIN}",
                )
            ],
        )
        self.assertEqual(
            rules[2 * len(subnets) : 3 * len(subnets)],
            [
                f"-A {IPTABLES_CHAIN} -d {subnet} -j RETURN"
                for subnet in subnets
            ],
        )
        self.assertEqual(
            sum(rule.startswith("-A DOCKER-USER ") for rule in rules),
            len(subnets),
        )
        self.assertEqual(
            sum(rule.startswith("-A INPUT ") for rule in rules),
            len(subnets),
        )
        self.assertTrue(
            all(rules.count(rule) == 1 for rule in rules[: 3 * len(subnets)])
        )
        self.assertFalse(any("," in rule for rule in rules[: 3 * len(subnets)]))

    def test_ipv4_drops_every_private_and_multicast_destination_for_demo_source(self) -> None:
        rules = render("v4", EXAMPLE_IPV4_SUBNET)

        self.assertEqual(
            rules[0],
            f"-A DOCKER-USER -s {EXAMPLE_IPV4_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[1],
            f"-A INPUT -s {EXAMPLE_IPV4_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[2],
            f"-A {IPTABLES_CHAIN} -d {EXAMPLE_IPV4_SUBNET} -j RETURN",
        )
        for destination in PRIVATE_IPV4_CIDRS:
            self.assertIn(
                f"-A {IPTABLES_CHAIN} -d {destination} -j DROP",
                rules,
            )
        self.assertEqual(rules[-1], f"-A {IPTABLES_CHAIN} -j RETURN")
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
            f"-A DOCKER-USER -s {EXAMPLE_IPV6_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[1],
            f"-A INPUT -s {EXAMPLE_IPV6_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[2],
            f"-A {IPTABLES_CHAIN} -d {EXAMPLE_IPV6_SUBNET} -j RETURN",
        )
        for destination in PRIVATE_IPV6_CIDRS:
            self.assertIn(
                f"-A {IPTABLES_CHAIN} -d {destination} -j DROP",
                rules,
            )
        self.assertEqual(rules[-1], f"-A {IPTABLES_CHAIN} -j RETURN")

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
