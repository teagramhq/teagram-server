#!/usr/bin/env python3
"""Tests for the OSV security gate and govulncheck reachability parser."""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from check_osv_report import analyze, parse_govulncheck_report, summary_text


def vulnerability(
    identifier: str,
    score: str,
    fixed: str | None,
) -> dict[str, object]:
    events: list[dict[str, str]] = [{"introduced": "0"}]
    if fixed is not None:
        events.append({"fixed": fixed})
    return {
        "id": identifier,
        "severity": [{"type": "CVSS_V3", "score": score}],
        "affected": [
            {
                "ranges": [{"type": "SEMVER", "events": events}],
            }
        ],
    }


def report(*vulnerabilities: dict[str, object]) -> dict[str, object]:
    return {
        "results": [
            {
                "source": {"path": "pnpm-lock.yaml"},
                "packages": [
                    {
                        "package": {"name": "example", "version": "1.0.0"},
                        "vulnerabilities": list(vulnerabilities),
                    }
                ],
            }
        ]
    }


class CheckOSVReportTest(unittest.TestCase):
    def test_fixed_high_and_critical_findings_fail(self) -> None:
        failures, without_fixes = analyze(
            report(
                vulnerability(
                    "GHSA-high-fixed",
                    "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H",
                    "1.0.1",
                ),
                vulnerability(
                    "GHSA-critical-fixed",
                    "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
                    "1.0.2",
                ),
            )
        )

        self.assertEqual(len(failures), 2)
        self.assertEqual(without_fixes, [])
        self.assertTrue(any("GHSA-high-fixed" in failure for failure in failures))
        self.assertTrue(any("fixed versions: 1.0.1" in failure for failure in failures))

    def test_findings_without_fixes_are_summarized_but_do_not_fail(self) -> None:
        failures, without_fixes = analyze(
            report(
                vulnerability(
                    "GHSA-high-no-fix",
                    "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H",
                    None,
                )
            )
        )

        self.assertEqual(failures, [])
        self.assertEqual(
            without_fixes,
            ["GHSA-high-no-fix in example@1.0.0 (pnpm-lock.yaml)"],
        )
        self.assertIn(
            "GHSA-high-no-fix in example@1.0.0 (pnpm-lock.yaml)",
            summary_text([], without_fixes),
        )

    def test_fixed_medium_finding_does_not_fail(self) -> None:
        failures, without_fixes = analyze(
            report(
                vulnerability(
                    "GHSA-medium-fixed",
                    "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:N/I:N/A:L",
                    "1.0.1",
                )
            )
        )

        self.assertEqual(failures, [])
        self.assertEqual(without_fixes, [])

    def test_malformed_cvss_does_not_bypass_a_fixed_finding(self) -> None:
        failures, _ = analyze(
            report(
                vulnerability(
                    "GHSA-unknown-score",
                    "CVSS:9.9/unsupported",
                    "1.0.1",
                )
            )
        )

        self.assertEqual(len(failures), 1)

    def test_govulncheck_only_returns_reachable_findings(self) -> None:
        messages = [
            {"config": {"scanner_name": "govulncheck"}},
            {
                "finding": {
                    "osv": "GO-2026-1",
                    "trace": [{"function": "example.com/app.call"}],
                }
            },
            {
                "finding": {
                    "osv": "GO-2026-2",
                    "trace": [{"function": ""}],
                }
            },
        ]
        with tempfile.TemporaryDirectory() as temporary_directory:
            report_path = Path(temporary_directory) / "govulncheck.json"
            report_path.write_text(
                "\n".join(json.dumps(message) for message in messages),
                encoding="utf-8",
            )

            self.assertEqual(
                parse_govulncheck_report(report_path),
                ["GO-2026-1"],
            )


if __name__ == "__main__":
    unittest.main()
