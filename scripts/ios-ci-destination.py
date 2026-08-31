#!/usr/bin/env python3
"""Choose or create a stable iOS Simulator destination for CI."""

from __future__ import annotations

import json
import subprocess
import sys
from dataclasses import dataclass
from typing import Any, Callable


PREFERRED_DEVICE_TYPE_IDENTIFIERS = (
    "com.apple.CoreSimulator.SimDeviceType.iPhone-16",
    "com.apple.CoreSimulator.SimDeviceType.iPhone-16-Pro",
    "com.apple.CoreSimulator.SimDeviceType.iPhone-15",
    "com.apple.CoreSimulator.SimDeviceType.iPhone-15-Pro",
    "com.apple.CoreSimulator.SimDeviceType.iPhone-14",
    "com.apple.CoreSimulator.SimDeviceType.iPhone-14-Pro",
)


@dataclass(frozen=True)
class Destination:
    name: str
    udid: str
    created: bool

    @property
    def xcodebuild_value(self) -> str:
        return f"platform=iOS Simulator,id={self.udid}"


Command = Callable[[list[str]], str]


def xcrun(command: list[str]) -> str:
    return subprocess.check_output(["xcrun", "simctl", *command], text=True)


def _available_devices(devices_json: dict[str, Any]) -> list[dict[str, Any]]:
    devices: list[dict[str, Any]] = []
    for runtime_devices in devices_json.get("devices", {}).values():
        if not isinstance(runtime_devices, list):
            continue
        for device in runtime_devices:
            if isinstance(device, dict) and device.get("isAvailable"):
                devices.append(device)
    return devices


def _preferred_existing_device(devices_json: dict[str, Any]) -> dict[str, Any] | None:
    devices = _available_devices(devices_json)
    if not devices:
        return None
    for identifier in PREFERRED_DEVICE_TYPE_IDENTIFIERS:
        for device in devices:
            if device.get("deviceTypeIdentifier") == identifier:
                return device
    for device in devices:
        if str(device.get("name", "")).startswith("iPhone"):
            return device
    return devices[0]


def _version_tuple(value: str) -> tuple[int, ...]:
    return tuple(int(part) for part in value.split(".") if part.isdigit())


def _latest_ios_runtime(runtimes_json: dict[str, Any]) -> str:
    candidates = [
        runtime
        for runtime in runtimes_json.get("runtimes", [])
        if isinstance(runtime, dict)
        and runtime.get("isAvailable")
        and runtime.get("platform") == "iOS"
        and runtime.get("identifier")
    ]
    if not candidates:
        raise RuntimeError("no available iOS simulator runtime")
    candidates.sort(key=lambda runtime: _version_tuple(str(runtime.get("version", ""))))
    return str(candidates[-1]["identifier"])


def _preferred_device_type(devicetypes_json: dict[str, Any]) -> tuple[str, str]:
    device_types = [
        device_type
        for device_type in devicetypes_json.get("devicetypes", [])
        if isinstance(device_type, dict)
        and str(device_type.get("name", "")).startswith("iPhone")
        and device_type.get("identifier")
    ]
    if not device_types:
        raise RuntimeError("no available iPhone simulator device type")
    for identifier in PREFERRED_DEVICE_TYPE_IDENTIFIERS:
        for device_type in device_types:
            if device_type.get("identifier") == identifier:
                return str(device_type["name"]), str(device_type["identifier"])
    selected = device_types[-1]
    return str(selected["name"]), str(selected["identifier"])


def select_destination(run: Command = xcrun) -> Destination:
    existing = _preferred_existing_device(json.loads(run(["list", "devices", "available", "--json"])))
    if existing is not None:
        return Destination(
            name=str(existing.get("name", "iOS Simulator")),
            udid=str(existing["udid"]),
            created=False,
        )

    runtime = _latest_ios_runtime(json.loads(run(["list", "runtimes", "--json"])))
    device_name, device_type = _preferred_device_type(json.loads(run(["list", "devicetypes", "--json"])))
    name = f"Cairn CI {device_name}"
    udid = run(["create", name, device_type, runtime]).strip()
    return Destination(name=name, udid=udid, created=True)


def main() -> int:
    try:
        destination = select_destination()
        if destination.created:
            xcrun(["boot", destination.udid])
            xcrun(["bootstatus", destination.udid, "-b"])
        print(f"destination={destination.xcodebuild_value}")
        print(f"ios simulator: {destination.name} ({destination.udid})", file=sys.stderr)
        return 0
    except Exception as error:
        print(f"ios simulator selection failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
