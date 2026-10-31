#!/usr/bin/env python3
"""Tests for the OSV security gate and govulncheck reachability parser."""

from __future__ import annotations

from contextlib import redirect_stdout
from io import StringIO
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import check_osv_report
from check_osv_report import (
    analyze,
    analyze_govulncheck_findings,
    parse_govulncheck_report,
    summary_text,
)


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


def govulncheck_findings(*messages: dict[str, object]) -> dict[str, dict[str, object]]:
    with tempfile.TemporaryDirectory() as temporary_directory:
        report_path = Path(temporary_directory) / "govulncheck.json"
        report_path.write_text(
            "\n".join(json.dumps(message) for message in messages),
            encoding="utf-8",
        )
        return parse_govulncheck_report(report_path)


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
        osv_metadata = {"id": "GO-2026-1", "severity": "HIGH"}
        messages = [
            {"config": {"scanner_name": "govulncheck"}},
            {
                "finding": {
                    "osv": "GO-2026-1",
                    "fixed_version": "go1.25.4",
                    "trace": [{"function": "example.com/app.call"}],
                }
            },
            {
                "finding": {
                    "osv": "GO-2026-2",
                    "trace": [{"function": ""}],
                }
            },
            {"osv": osv_metadata},
        ]
        with tempfile.TemporaryDirectory() as temporary_directory:
            report_path = Path(temporary_directory) / "govulncheck.json"
            report_path.write_text(
                "\n".join(json.dumps(message) for message in messages),
                encoding="utf-8",
            )

            findings = parse_govulncheck_report(report_path)

        self.assertEqual(list(findings), ["GO-2026-1"])
        self.assertEqual(findings["GO-2026-1"]["osv"], osv_metadata)
        self.assertEqual(
            findings["GO-2026-1"]["finding"]["fixed_version"], "go1.25.4"
        )

    def test_main_fails_for_govulncheck_only_fixed_high_finding(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            osv_report_path = Path(temporary_directory) / "osv.json"
            osv_report_path.write_text(json.dumps({"results": []}), encoding="utf-8")
            govulncheck_report_path = Path(temporary_directory) / "govulncheck.json"
            govulncheck_report_path.write_text(
                "\n".join(
                    json.dumps(message)
                    for message in (
                        {"config": {"scanner_name": "govulncheck"}},
                        {
                            "finding": {
                                "osv": "GO-2026-6",
                                "fixed_version": "go1.25.4",
                                "trace": [{"function": "crypto/example.Verify"}],
                            }
                        },
                        {
                            "osv": vulnerability(
                                "GO-2026-6",
                                "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H",
                                "go1.25.4",
                            )
                        },
                    )
                ),
                encoding="utf-8",
            )
            summary_path = Path(temporary_directory) / "summary.md"
            output = StringIO()
            with (
                patch.object(
                    check_osv_report.sys,
                    "argv",
                    [
                        "check_osv_report.py",
                        str(osv_report_path),
                        str(govulncheck_report_path),
                    ],
                ),
                patch.dict(os.environ, {"GITHUB_STEP_SUMMARY": str(summary_path)}),
                redirect_stdout(output),
            ):
                status = check_osv_report.main()

        self.assertEqual(status, 1)
        self.assertIn("GO-2026-6", output.getvalue())
        self.assertIn("fixed version: go1.25.4", output.getvalue())

    def test_reachable_unfixed_high_govulncheck_finding_is_summarized(self) -> None:
        findings = govulncheck_findings(
            {"config": {"scanner_name": "govulncheck"}},
            {
                "osv": vulnerability(
                    "GO-2026-4",
                    "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H",
                    None,
                )
            },
            {
                "finding": {
                    "osv": "GO-2026-4",
                    "trace": [{"function": "crypto/example.Verify"}],
                }
            },
        )

        failures, without_fixes = analyze_govulncheck_findings(findings)

        self.assertEqual(failures, [])
        self.assertEqual(without_fixes, ["GO-2026-4 reachable via govulncheck"])
        self.assertIn(
            "GO-2026-4 reachable via govulncheck",
            summary_text(list(findings), without_fixes),
        )

    def test_fixed_reachable_govulncheck_finding_without_severity_fails_closed(self) -> None:
        findings = govulncheck_findings(
            {"config": {"scanner_name": "govulncheck"}},
            {"osv": {"id": "GO-2026-7", "aliases": ["GHSA-unknown-severity"]}},
            {
                "finding": {
                    "osv": "GO-2026-7",
                    "fixed_version": "go1.25.4",
                    "trace": [{"function": "crypto/example.Verify"}],
                }
            },
        )

        failures, without_fixes = analyze_govulncheck_findings(findings)

        self.assertEqual(without_fixes, [])
        self.assertEqual(len(failures), 1)
        self.assertIn("severity unavailable; failing closed", failures[0])

    def test_fixed_reachable_govulncheck_finding_uses_alias_advisory_severity(self) -> None:
        findings = govulncheck_findings(
            {"config": {"scanner_name": "govulncheck"}},
            {"osv": {"id": "GO-2026-8", "aliases": ["GHSA-high-alias"]}},
            {
                "finding": {
                    "osv": "GO-2026-8",
                    "fixed_version": "go1.25.4",
                    "trace": [{"function": "crypto/example.Verify"}],
                }
            },
        )
        osv_report = report(
            vulnerability(
                "GHSA-high-alias",
                "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H",
                None,
            )
        )

        failures, without_fixes = analyze_govulncheck_findings(
            findings, check_osv_report.aliased_advisory_severities(osv_report)
        )

        self.assertEqual(without_fixes, [])
        self.assertEqual(len(failures), 1)
        self.assertIn("fixed version: go1.25.4", failures[0])

    def test_fixed_reachable_govulncheck_finding_uses_known_medium_alias_severity(self) -> None:
        findings = govulncheck_findings(
            {"config": {"scanner_name": "govulncheck"}},
            {"osv": {"id": "GO-2026-9", "aliases": ["GHSA-medium-alias"]}},
            {
                "finding": {
                    "osv": "GO-2026-9",
                    "fixed_version": "go1.25.4",
                    "trace": [{"function": "crypto/example.Verify"}],
                }
            },
        )
        osv_report = report(
            vulnerability(
                "GHSA-medium-alias",
                "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:N/I:N/A:L",
                None,
            )
        )

        failures, without_fixes = analyze_govulncheck_findings(
            findings, check_osv_report.aliased_advisory_severities(osv_report)
        )

        self.assertEqual(failures, [])
        self.assertEqual(without_fixes, [])

    def test_reachable_govulncheck_finding_requires_osv_metadata(self) -> None:
        with self.assertRaisesRegex(ValueError, "missing OSV metadata"):
            govulncheck_findings(
                {"config": {"scanner_name": "govulncheck"}},
                {
                    "finding": {
                        "osv": "GO-2026-5",
                        "fixed_version": "go1.25.4",
                        "trace": [{"function": "crypto/example.Verify"}],
                    }
                },
            )


if __name__ == "__main__":
    unittest.main()
