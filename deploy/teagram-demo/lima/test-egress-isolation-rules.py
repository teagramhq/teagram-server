#!/usr/bin/env python3
"""Check the rendered Lima Docker egress rules without touching a firewall."""

from __future__ import annotations

import collections
import pathlib
import subprocess
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


def render(family: str, subnet: str) -> list[str]:
    result = subprocess.run(
        [str(SCRIPT), f"--render-{family}", subnet],
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.splitlines()


class EgressIsolationRuleTests(unittest.TestCase):
    def test_ipv4_drops_every_private_and_multicast_destination_for_demo_source(self) -> None:
        rules = render("v4", EXAMPLE_IPV4_SUBNET)

        self.assertEqual(
            rules[0],
            f"-A DOCKER-USER -s {EXAMPLE_IPV4_SUBNET} -j {IPTABLES_CHAIN}",
        )
        self.assertEqual(
            rules[1],
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
