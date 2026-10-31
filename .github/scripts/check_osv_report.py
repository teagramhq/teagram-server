#!/usr/bin/env python3
"""Fail on fixed high or critical dependency and reachable Go findings."""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path
from typing import Any

from cvss import CVSS2, CVSS3, CVSS4


SAFE_ID = re.compile(r"[A-Za-z0-9._-]+")


def cvss_score(vector: str, score_type: str | None) -> float:
    normalized_type = score_type.upper() if isinstance(score_type, str) else ""
    if not normalized_type:
        if vector.startswith("CVSS:4.0/"):
            normalized_type = "CVSS_V4"
        elif vector.startswith(("CVSS:3.0/", "CVSS:3.1/")):
            normalized_type = "CVSS_V3"
        else:
            normalized_type = "CVSS_V2"

    try:
        if normalized_type == "CVSS_V2":
            if vector.startswith("CVSS:2.0/"):
                vector = vector.removeprefix("CVSS:2.0/")
            return CVSS2(vector).scores()[0]
        if normalized_type == "CVSS_V3":
            return CVSS3(vector).scores()[0]
        if normalized_type == "CVSS_V4":
            return CVSS4(vector).scores()[0]
    except Exception:
        return 10.0
    return 10.0


def severity_classification(vulnerability: dict[str, Any]) -> bool | None:
    candidates: list[Any] = []
    severity = vulnerability.get("severity", [])
    if isinstance(severity, list):
        candidates.extend(severity)
    elif severity:
        candidates.append(severity)

    database_specific = vulnerability.get("database_specific", {})
    if isinstance(database_specific, dict):
        candidates.append(database_specific.get("severity"))
    affected = vulnerability.get("affected", [])
    if isinstance(affected, list):
        for item in affected:
            if not isinstance(item, dict):
                continue
            for key in ("database_specific", "ecosystem_specific"):
                details = item.get(key, {})
                if isinstance(details, dict):
                    candidates.append(details.get("severity"))

    has_severity = False
    for candidate in candidates:
        score_type = None
        if isinstance(candidate, dict):
            score_type = candidate.get("type")
            candidate = candidate.get("score")
        if not isinstance(candidate, (str, int, float)):
            continue
        if isinstance(candidate, str):
            normalized = candidate.strip().upper()
            if normalized in {"HIGH", "CRITICAL"}:
                return True
            if normalized in {"NONE", "LOW", "MEDIUM", "MODERATE"}:
                has_severity = True
                continue
            try:
                score = float(candidate)
            except ValueError:
                score = cvss_score(candidate, score_type)
        else:
            score = float(candidate)
        has_severity = True
        if score >= 7.0:
            return True
    return False if has_severity else None


def severity_is_high_or_critical(vulnerability: dict[str, Any]) -> bool:
    return severity_classification(vulnerability) is True


def aliased_advisory_severities(report: dict[str, Any]) -> dict[str, bool]:
    """Index known OSV Scanner severities by advisory ID and aliases."""
    severities: dict[str, bool] = {}
    results = report.get("results", [])
    if not isinstance(results, list):
        return severities
    for result in results:
        if not isinstance(result, dict):
            continue
        packages = result.get("packages", [])
        if not isinstance(packages, list):
            continue
        for package in packages:
            if not isinstance(package, dict):
                continue
            vulnerabilities = package.get("vulnerabilities", [])
            if not isinstance(vulnerabilities, list):
                continue
            for vulnerability in vulnerabilities:
                if not isinstance(vulnerability, dict):
                    continue
                severity = severity_classification(vulnerability)
                if severity is None:
                    continue
                identifiers = [vulnerability.get("id")]
                aliases = vulnerability.get("aliases", [])
                if isinstance(aliases, list):
                    identifiers.extend(aliases)
                for identifier in identifiers:
                    if isinstance(identifier, str) and SAFE_ID.fullmatch(identifier):
                        severities[identifier] = severities.get(identifier, False) or severity
    return severities


def fixed_versions(vulnerability: dict[str, Any]) -> list[str]:
    versions: set[str] = set()
    affected = vulnerability.get("affected", [])
    if isinstance(affected, list):
        for item in affected:
            if not isinstance(item, dict):
                continue
            ranges = item.get("ranges", [])
            if isinstance(ranges, list):
                for version_range in ranges:
                    if not isinstance(version_range, dict):
                        continue
                    events = version_range.get("events", [])
                    if isinstance(events, list):
                        for event in events:
                            if isinstance(event, dict):
                                value = event.get("fixed")
                                if isinstance(value, str) and value.strip():
                                    versions.add(value.strip())
            for key in ("database_specific", "ecosystem_specific"):
                details = item.get(key, {})
                if isinstance(details, dict):
                    for fixed_key in ("fixed", "fixed_version", "fixedVersion"):
                        value = details.get(fixed_key)
                        if isinstance(value, str) and value.strip():
                            versions.add(value.strip())
    return sorted(versions)


def analyze(report: dict[str, Any]) -> tuple[list[str], list[str]]:
    results = report.get("results")
    if not isinstance(results, list):
        raise ValueError("OSV-Scanner report is missing a results array")

    failures: set[str] = set()
    without_fixes: set[str] = set()
    for result in results:
        if not isinstance(result, dict):
            raise ValueError("OSV-Scanner report contains an invalid result")
        source = result.get("source", {})
        source_path = source.get("path", "unknown source") if isinstance(source, dict) else "unknown source"
        packages = result.get("packages", [])
        if not isinstance(packages, list):
            raise ValueError("OSV-Scanner report contains an invalid packages list")
        for package_result in packages:
            if not isinstance(package_result, dict):
                raise ValueError("OSV-Scanner report contains an invalid package result")
            package = package_result.get("package", {})
            if not isinstance(package, dict):
                raise ValueError("OSV-Scanner report contains an invalid package")
            name = package.get("name")
            version = package.get("version")
            if not isinstance(name, str) or not isinstance(version, str):
                raise ValueError("OSV-Scanner package is missing its name or version")
            vulnerabilities = package_result.get("vulnerabilities", [])
            if not isinstance(vulnerabilities, list):
                raise ValueError("OSV-Scanner report contains an invalid vulnerabilities list")
            for vulnerability in vulnerabilities:
                if not isinstance(vulnerability, dict):
                    raise ValueError("OSV-Scanner report contains an invalid vulnerability")
                identifier = vulnerability.get("id")
                if not isinstance(identifier, str) or not SAFE_ID.fullmatch(identifier):
                    raise ValueError("OSV-Scanner returned a vulnerability without a safe ID")
                item = f"{identifier} in {name}@{version} ({source_path})"
                fixes = fixed_versions(vulnerability)
                if not fixes:
                    without_fixes.add(item)
                elif severity_is_high_or_critical(vulnerability):
                    failures.add(f"{item}; fixed versions: {', '.join(fixes)}")
    return sorted(failures), sorted(without_fixes)


def parse_govulncheck_report(report_path: Path) -> dict[str, dict[str, Any]]:
    try:
        text = report_path.read_text(encoding="utf-8")
    except OSError as error:
        raise ValueError(f"could not read govulncheck report: {error}") from error

    decoder = json.JSONDecoder()
    messages: list[dict[str, Any]] = []
    offset = 0
    while offset < len(text):
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset == len(text):
            break
        try:
            message, offset = decoder.raw_decode(text, offset)
        except json.JSONDecodeError as error:
            raise ValueError(f"could not parse govulncheck report: {error}") from error
        if not isinstance(message, dict):
            raise ValueError("govulncheck emitted a non-object JSON message")
        messages.append(message)

    if not any(
        isinstance(message.get("config"), dict)
        and message["config"].get("scanner_name") == "govulncheck"
        for message in messages
    ):
        raise ValueError("govulncheck report is missing its scanner configuration")

    osv_entries: dict[str, dict[str, Any]] = {}
    findings: dict[str, dict[str, Any]] = {}
    for message in messages:
        if "osv" in message:
            vulnerability = message["osv"]
            if not isinstance(vulnerability, dict):
                raise ValueError("govulncheck emitted invalid OSV metadata")
            identifier = vulnerability.get("id")
            if not isinstance(identifier, str) or not SAFE_ID.fullmatch(identifier):
                raise ValueError("govulncheck emitted OSV metadata without a safe ID")
            existing = osv_entries.get(identifier)
            if existing is not None and existing != vulnerability:
                raise ValueError(f"govulncheck emitted conflicting OSV metadata for {identifier}")
            osv_entries[identifier] = vulnerability

        finding = message.get("finding")
        if finding is None:
            continue
        if not isinstance(finding, dict) or not isinstance(finding.get("osv"), str):
            raise ValueError("govulncheck emitted a finding without a vulnerability ID")
        identifier = finding["osv"]
        if not SAFE_ID.fullmatch(identifier):
            raise ValueError("govulncheck emitted a finding with an unsafe vulnerability ID")
        fixed_version = finding.get("fixed_version", "")
        if not isinstance(fixed_version, str):
            raise ValueError("govulncheck emitted an invalid fixed version")
        trace = finding.get("trace", [])
        if not isinstance(trace, list):
            raise ValueError("govulncheck emitted an invalid finding trace")
        if any(
            isinstance(frame, dict)
            and isinstance(frame.get("function"), str)
            and frame["function"]
            for frame in trace
        ):
            existing = findings.get(identifier)
            if (
                existing is not None
                and existing.get("fixed_version")
                and fixed_version
                and existing["fixed_version"] != fixed_version
            ):
                raise ValueError(
                    f"govulncheck emitted conflicting fixed versions for {identifier}"
                )
            if existing is None or (not existing.get("fixed_version") and fixed_version):
                findings[identifier] = finding

    missing_metadata = sorted(findings.keys() - osv_entries.keys())
    if missing_metadata:
        raise ValueError(
            "govulncheck findings are missing OSV metadata: " + ", ".join(missing_metadata)
        )
    return {
        identifier: {"finding": findings[identifier], "osv": osv_entries[identifier]}
        for identifier in sorted(findings)
    }


def analyze_govulncheck_findings(
    findings: dict[str, dict[str, Any]],
    aliased_severities: dict[str, bool] | None = None,
) -> tuple[list[str], list[str]]:
    failures: set[str] = set()
    without_fixes: set[str] = set()
    aliased_severities = aliased_severities or {}
    for identifier, joined in findings.items():
        vulnerability = joined["osv"]
        finding = joined["finding"]
        fixed_version = finding.get("fixed_version", "")
        item = f"{identifier} reachable via govulncheck"
        if not fixed_version:
            without_fixes.add(item)
            continue

        severity = severity_classification(vulnerability)
        if severity is None:
            aliases = vulnerability.get("aliases", [])
            if isinstance(aliases, list):
                for alias in aliases:
                    if isinstance(alias, str) and alias in aliased_severities:
                        severity = aliased_severities[alias]
                        if severity:
                            break

        if severity is True:
            failures.add(f"{item}; fixed version: {fixed_version}")
        elif severity is None:
            failures.add(
                f"{item}; fixed version: {fixed_version}; severity unavailable; failing closed"
            )
    return sorted(failures), sorted(without_fixes)


def summary_text(reachable_findings: list[str], without_fixes: list[str]) -> str:
    lines = ["### Security scan"]
    if reachable_findings:
        lines.append("- govulncheck reachable findings: " + ", ".join(reachable_findings))
    else:
        lines.append("- govulncheck reachable findings: none")
    lines.append("- Findings without a fixed version:")
    if without_fixes:
        lines.extend(f"  - {finding}" for finding in without_fixes)
    else:
        lines.append("  - none")
    return "\n".join(lines) + "\n"


def main() -> int:
    if len(sys.argv) != 3:
        print(
            "usage: check_osv_report.py <osv-report.json> <govulncheck-report.json>",
            file=sys.stderr,
        )
        return 2
    try:
        report = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
        if not isinstance(report, dict):
            raise ValueError("OSV-Scanner report is not an object")
        failures, without_fixes = analyze(report)
        aliased_severities = aliased_advisory_severities(report)
        reachable_findings = parse_govulncheck_report(Path(sys.argv[2]))
        govulncheck_failures, govulncheck_without_fixes = analyze_govulncheck_findings(
            reachable_findings, aliased_severities
        )
        failures = sorted(set(failures) | set(govulncheck_failures))
        without_fixes = sorted(set(without_fixes) | set(govulncheck_without_fixes))
        summary = summary_text(sorted(reachable_findings), without_fixes)
        summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary_path:
            with Path(summary_path).open("a", encoding="utf-8") as output:
                output.write(summary)
        else:
            print(summary, end="")
    except (OSError, json.JSONDecodeError, ValueError) as error:
        print(error, file=sys.stderr)
        return 1

    if failures:
        print("Security scanners found blocking vulnerabilities:")
        for failure in failures:
            print(f"- {failure}")
        return 1
    print("Security scanners found no blocking vulnerabilities")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
