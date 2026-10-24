#!/usr/bin/env python3
"""Contract tests for metadata-only teagramd log-volume accounting."""

import datetime as dt
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("teagramd-log-volume-accounting.py")
SPEC = importlib.util.spec_from_file_location("teagramd_log_volume_accounting", SCRIPT)
ACCOUNTING = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ACCOUNTING)


def timestamp(value: str) -> dt.datetime:
    return ACCOUNTING.parse_time(value)


class LogVolumeAccountingTest(unittest.TestCase):
    def test_replacement_adds_both_containers_without_a_gap(self) -> None:
        state = ACCOUNTING.new_state()
        created = "2026-10-24T00:00:00Z"
        state_reason = ACCOUNTING.record_reading(
            state, created, 100, timestamp("2026-10-24T00:00:00Z"), "sample"
        )
        self.assertIsNone(state_reason)
        state_reason = ACCOUNTING.record_reading(
            state, created, 1300, timestamp("2026-10-24T00:15:00Z"), "sample"
        )
        self.assertIsNone(state_reason)
        state_reason = ACCOUNTING.record_reading(
            state, created, 1300, timestamp("2026-10-24T00:16:00Z"), "before-replacement"
        )
        self.assertIsNone(state_reason)
        state_reason = ACCOUNTING.record_reading(
            state, "2026-10-24T00:16:30Z", 800,
            timestamp("2026-10-24T00:17:00Z"), "after-replacement"
        )

        self.assertIsNone(state_reason)
        self.assertEqual(state["current_day"]["bytes"], 2000)
        self.assertFalse(state["current_day"]["gap"])

    def test_missed_sample_keeps_observed_bytes_and_marks_gap(self) -> None:
        state = ACCOUNTING.new_state()
        created = "2026-10-24T00:00:00Z"
        ACCOUNTING.record_reading(state, created, 100, timestamp("2026-10-24T00:00:00Z"), "sample")
        reason = ACCOUNTING.record_reading(
            state, created, 600, timestamp("2026-10-24T00:30:00Z"), "sample"
        )

        self.assertEqual(reason, "sample_missed")
        self.assertEqual(state["current_day"]["bytes"], 500)
        self.assertTrue(state["current_day"]["gap"])

    def test_unavailable_source_is_not_recorded_as_zero(self) -> None:
        state = ACCOUNTING.new_state()
        reason = ACCOUNTING.record_unavailable(
            state, timestamp("2026-10-24T00:00:00Z"), "sample"
        )

        self.assertEqual(reason, "log_source_unavailable")
        self.assertEqual(state["collection_start"], "2026-10-24T00:00:00Z")
        self.assertIsNone(state["current_day"]["bytes"])
        self.assertTrue(state["current_day"]["gap"])
        ACCOUNTING.record_reading(
            state, "2026-10-24T00:00:00Z", 1000,
            timestamp("2026-10-24T00:15:00Z"), "sample"
        )
        self.assertIsNone(state["current_day"]["bytes"])

    def test_replacement_after_missing_pre_capture_keeps_observed_bytes_and_gap(self) -> None:
        state = ACCOUNTING.new_state()
        ACCOUNTING.record_unavailable(
            state, timestamp("2026-10-24T00:00:00Z"), "before-replacement"
        )
        ACCOUNTING.record_reading(
            state, "2026-10-24T00:00:30Z", 800,
            timestamp("2026-10-24T00:01:00Z"), "after-replacement"
        )

        self.assertEqual(state["current_day"]["bytes"], 800)
        self.assertTrue(state["current_day"]["gap"])

    def test_rotated_or_non_json_log_source_is_unavailable(self) -> None:
        for driver, options in (("syslog", "{}"), ("json-file", '{"max-size":"1m"}')):
            with self.subTest(driver=driver, options=options), patch.object(
                ACCOUNTING.subprocess,
                "run",
                return_value=SimpleNamespace(
                    returncode=0,
                    stdout=f"2026-10-24T00:00:00Z|/var/lib/docker/containers/log.json|{driver}|{options}\n",
                ),
            ):
                with self.assertRaises(ACCOUNTING.SourceUnavailable):
                    ACCOUNTING.read_snapshot()

    def test_missing_docker_command_is_unavailable(self) -> None:
        with patch.object(ACCOUNTING.subprocess, "run", side_effect=FileNotFoundError):
            with self.assertRaises(ACCOUNTING.SourceUnavailable):
                ACCOUNTING.read_snapshot()

    def test_unhooked_replacement_records_gap_and_observed_bytes(self) -> None:
        state = ACCOUNTING.new_state()
        ACCOUNTING.record_reading(
            state, "2026-10-24T00:00:00Z", 100,
            timestamp("2026-10-24T00:00:00Z"), "sample"
        )
        reason = ACCOUNTING.record_reading(
            state, "2026-10-24T00:10:00Z", 800,
            timestamp("2026-10-24T00:15:00Z"), "sample"
        )

        self.assertEqual(reason, "replacement_capture_missing")
        self.assertEqual(state["current_day"]["bytes"], 800)
        self.assertTrue(state["current_day"]["gap"])

    def test_utc_midnight_closes_a_day_without_a_gap(self) -> None:
        state = ACCOUNTING.new_state()
        created = "2026-10-24T22:00:00Z"
        ACCOUNTING.record_reading(
            state, created, 100, timestamp("2026-10-24T23:45:00Z"), "sample"
        )
        ACCOUNTING.record_reading(
            state, created, 400, timestamp("2026-10-25T00:00:10Z"), "sample"
        )

        self.assertEqual(state["days"][-1]["utc_date"], "2026-10-24")
        self.assertEqual(state["days"][-1]["bytes"], 300)
        self.assertFalse(state["days"][-1]["gap"])
        self.assertEqual(state["current_day"]["utc_date"], "2026-10-25")

    def test_report_uses_complete_rolling_weeks_after_fourteen_days(self) -> None:
        state = ACCOUNTING.new_state()
        first_day = dt.date(2026, 10, 1)
        state["collection_start"] = ACCOUNTING.format_time(ACCOUNTING.day_start(first_day))
        for offset in range(14):
            day = first_day + dt.timedelta(days=offset)
            state["days"].append(ACCOUNTING.new_day(
                day,
                start=ACCOUNTING.format_time(ACCOUNTING.day_start(day)),
                end=ACCOUNTING.format_time(ACCOUNTING.day_start(day + dt.timedelta(days=1))),
                byte_count=100 + offset,
            ))
        state["current_day"] = ACCOUNTING.new_day(first_day + dt.timedelta(days=14))

        report = ACCOUNTING.summarize(state, ACCOUNTING.day_start(first_day + dt.timedelta(days=14)))

        self.assertEqual(report["complete_utc_days"], 14)
        self.assertEqual(report["maximum_rolling_seven_day_bytes"], sum(range(107, 114)))
        self.assertEqual(report["recommended_quota_bytes"], 2 * sum(range(107, 114)))
        self.assertTrue(report["ready_for_capacity_decision"])

    def test_daily_history_has_a_fixed_retention_bound(self) -> None:
        state = ACCOUNTING.new_state()
        first_day = dt.date(2026, 10, 1)
        for offset in range(35):
            ACCOUNTING.append_day(state, ACCOUNTING.new_day(first_day + dt.timedelta(days=offset)))

        self.assertEqual(len(state["days"]), ACCOUNTING.MAX_RETAINED_DAYS)
        self.assertEqual(state["days"][0]["utc_date"], "2026-10-06")


if __name__ == "__main__":
    unittest.main()
