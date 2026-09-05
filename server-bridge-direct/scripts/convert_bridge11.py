#!/usr/bin/env python3
"""Convert host:port:user:password lines to a proxy.csv-style SOCKS5 CSV.

The output intentionally contains credentials because it is meant for the
proxy test runner. Keep the generated file local and do not commit it.
"""

from __future__ import annotations

import argparse
import csv
import os
import tempfile
from pathlib import Path
from urllib.parse import quote


DEFAULT_INPUT = Path(r"D:\work\bridge11.csv")
DEFAULT_OUTPUT = Path(r"D:\work\bridge11-proxy.csv")


def convert(input_path: Path, output_path: Path, skip_invalid: bool = False) -> tuple[int, int]:
    proxies: list[str] = []
    invalid: list[str] = []

    with input_path.open("r", encoding="utf-8-sig", newline="") as source:
        for line_no, raw_line in enumerate(source, start=1):
            line = raw_line.strip()
            if not line or line.startswith("#"):
                continue

            parts = line.split(":", 3)
            if len(parts) != 4:
                invalid.append(f"line {line_no}: expected host:port:user:password")
                continue

            host, port_text, username, password = (part.strip() for part in parts)
            try:
                port = int(port_text)
            except ValueError:
                invalid.append(f"line {line_no}: invalid port")
                continue

            if not host or not 1 <= port <= 65535 or not username or not password:
                invalid.append(f"line {line_no}: host/port/username/password is invalid")
                continue

            # Encode credentials so '@', ':', '/', spaces, etc. cannot corrupt
            # the proxy URL syntax. The host is kept as supplied; IPv6 input is
            # ambiguous in the four-colon source format and is rejected above
            # by the field count when it cannot be represented safely.
            encoded_user = quote(username, safe="")
            encoded_password = quote(password, safe="")
            proxies.append(
                f"socks5://{encoded_user}:{encoded_password}@{host}:{port}"
            )

    if invalid and not skip_invalid:
        details = "\n".join(invalid[:20])
        more = "" if len(invalid) <= 20 else f"\n... and {len(invalid) - 20} more"
        raise ValueError(f"invalid input lines ({len(invalid)}):\n{details}{more}")

    output_path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{output_path.name}.tmp-", dir=output_path.parent
    )
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as target:
            writer = csv.writer(target, quoting=csv.QUOTE_ALL, lineterminator="\n")
            writer.writerow(["proxy"])
            writer.writerows([[proxy] for proxy in proxies])
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary_name, output_path)
    except Exception:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise

    return len(proxies), len(invalid)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Convert host:port:user:password lines to a SOCKS5 proxy CSV."
    )
    parser.add_argument("-i", "--input", type=Path, default=DEFAULT_INPUT)
    parser.add_argument("-o", "--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument(
        "--skip-invalid",
        action="store_true",
        help="skip malformed lines instead of failing the conversion",
    )
    args = parser.parse_args()

    try:
        converted, invalid = convert(args.input, args.output, args.skip_invalid)
    except Exception as exc:  # noqa: BLE001 - CLI should show a concise failure
        parser.error(str(exc))
        return 2

    print(f"converted={converted} invalid={invalid} output={args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

