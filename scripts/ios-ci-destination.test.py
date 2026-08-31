#!/usr/bin/env python3
"""Tests for the iOS CI simulator destination selector."""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
import unittest


SCRIPT = Path(__file__).with_name("ios-ci-destination.py")
SPEC = importlib.util.spec_from_file_location("ios_ci_destination", SCRIPT)
assert SPEC is not None
assert SPEC.loader is not None
ios_ci_destination = importlib.util.module_from_spec(SPEC)
sys.modules["ios_ci_destination"] = ios_ci_destination
SPEC.loader.exec_module(ios_ci_destination)


class DestinationSelectionTest(unittest.TestCase):
    def test_reuses_existing_preferred_device(self) -> None:
        calls: list[list[str]] = []

        def run(command: list[str]) -> str:
            calls.append(command)
            self.assertEqual(command, ["list", "devices", "available", "--json"])
            return """
            {
              "devices": {
                "com.apple.CoreSimulator.SimRuntime.iOS-19-0": [
                  {
                    "name": "iPhone 16 Pro",
                    "udid": "preferred-udid",
                    "isAvailable": true,
                    "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-16-Pro"
                  }
                ]
              }
            }
            """

        destination = ios_ci_destination.select_destination(run)

        self.assertFalse(destination.created)
        self.assertEqual(destination.name, "iPhone 16 Pro")
        self.assertEqual(destination.xcodebuild_value, "platform=iOS Simulator,id=preferred-udid")
        self.assertEqual(calls, [["list", "devices", "available", "--json"]])

    def test_creates_device_when_runner_has_only_placeholders(self) -> None:
        def run(command: list[str]) -> str:
            if command == ["list", "devices", "available", "--json"]:
                return '{"devices": {}}'
            if command == ["list", "runtimes", "--json"]:
                return """
                {
                  "runtimes": [
                    {
                      "platform": "iOS",
                      "version": "18.5",
                      "identifier": "com.apple.CoreSimulator.SimRuntime.iOS-18-5",
                      "isAvailable": true
                    },
                    {
                      "platform": "iOS",
                      "version": "19.0",
                      "identifier": "com.apple.CoreSimulator.SimRuntime.iOS-19-0",
                      "isAvailable": true
                    }
                  ]
                }
                """
            if command == ["list", "devicetypes", "--json"]:
                return """
                {
                  "devicetypes": [
                    {
                      "name": "iPhone 15",
                      "identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-15"
                    },
                    {
                      "name": "iPhone 16",
                      "identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-16"
                    }
                  ]
                }
                """
            if command == [
                "create",
                "Cairn CI iPhone 16",
                "com.apple.CoreSimulator.SimDeviceType.iPhone-16",
                "com.apple.CoreSimulator.SimRuntime.iOS-19-0",
            ]:
                return "created-udid\n"
            self.fail(f"unexpected command: {command}")

        destination = ios_ci_destination.select_destination(run)

        self.assertTrue(destination.created)
        self.assertEqual(destination.name, "Cairn CI iPhone 16")
        self.assertEqual(destination.xcodebuild_value, "platform=iOS Simulator,id=created-udid")


if __name__ == "__main__":
    unittest.main()
