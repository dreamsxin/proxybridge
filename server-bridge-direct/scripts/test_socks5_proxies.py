#!/usr/bin/env python3
"""Test SOCKS5 proxies, credentials, and access to an HTTP endpoint.

The input can be the proxy CSV produced by ``convert_bridge11.py`` (the first
column is used) or a TXT file containing one proxy URL per line.  The script
implements the SOCKS5 handshake with the Python standard library, so no
third-party package such as requests[socks] is required.

Credentials are never printed.  A report, when requested, only contains a
redacted proxy address (host and port) and the observed exit IP.
"""

from __future__ import annotations

import argparse
import csv
import http.client
import ipaddress
import json
import socket
import ssl
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Iterable
from urllib.parse import unquote, urlsplit


DEFAULT_PROXY_FILE = Path(r"D:\work\bridge11-proxy.csv")
DEFAULT_URL = "http://myip.ipipv.com/"


class ProxyTestError(Exception):
    """An expected, user-facing proxy test failure."""

    def __init__(self, category: str, message: str) -> None:
        super().__init__(message)
        self.category = category


@dataclass(frozen=True)
class ProxySpec:
    scheme: str
    host: str
    port: int
    username: str | None
    password: str | None

    @property
    def display(self) -> str:
        return f"{self.scheme}://{self.host}:{self.port}"


@dataclass
class TestResult:
    index: int
    proxy: str
    status: str
    category: str
    message: str
    exit_ip: str = ""
    auth: str = ""
    elapsed_ms: int = 0
    attempt: int = 1


def read_proxy_values(path: Path) -> list[str]:
    """Read the first column from CSV, or one proxy URL per TXT line."""

    if path.suffix.lower() == ".csv":
        values: list[str] = []
        with path.open("r", encoding="utf-8-sig", newline="") as source:
            for row in csv.reader(source):
                if not row or not row[0].strip():
                    continue
                value = row[0].strip()
                if value.startswith("#"):
                    continue
                normalized = value.lstrip("#").strip().lower()
                if normalized in {"proxy", "proxy_url", "proxyurl"}:
                    continue
                values.append(value)
        return values

    values = []
    with path.open("r", encoding="utf-8-sig") as source:
        for raw_line in source:
            value = raw_line.strip()
            if value and not value.startswith("#"):
                values.append(value)
    return values


def parse_proxy(value: str) -> ProxySpec:
    raw = value.strip()
    if "://" not in raw:
        raw = f"socks5://{raw}"

    parsed = urlsplit(raw)
    scheme = parsed.scheme.lower()
    if scheme not in {"socks5", "socks5h"}:
        raise ProxyTestError("unsupported_scheme", f"unsupported scheme {scheme!r}")
    if not parsed.hostname:
        raise ProxyTestError("invalid_proxy", "proxy host is empty")
    try:
        port = parsed.port
    except ValueError as exc:
        raise ProxyTestError("invalid_proxy", "proxy port is invalid") from exc
    if port is None or not 1 <= port <= 65535:
        raise ProxyTestError("invalid_proxy", "proxy port must be 1..65535")

    username = parsed.username
    password = parsed.password
    if (username is None) != (password is None):
        raise ProxyTestError(
            "invalid_proxy", "proxy credentials must include both username and password"
        )
    return ProxySpec(
        scheme=scheme,
        host=parsed.hostname,
        port=port,
        username=unquote(username) if username is not None else None,
        password=unquote(password) if password is not None else None,
    )


def recv_exact(sock: socket.socket, size: int) -> bytes:
    chunks: list[bytes] = []
    remaining = size
    while remaining:
        chunk = sock.recv(remaining)
        if not chunk:
            raise ProxyTestError("proxy_closed", "proxy closed during SOCKS5 handshake")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def socks5_connect(sock: socket.socket, spec: ProxySpec, target_host: str, target_port: int) -> str:
    methods = b"\x00"  # no authentication
    if spec.username is not None and spec.password is not None:
        methods += b"\x02"  # username/password authentication
    sock.sendall(b"\x05" + bytes([len(methods)]) + methods)

    greeting = recv_exact(sock, 2)
    if greeting[0] != 5:
        raise ProxyTestError("proxy_protocol", "proxy returned an invalid SOCKS version")
    method = greeting[1]
    if method == 0xFF:
        raise ProxyTestError("auth_method_failed", "proxy rejected all offered auth methods")
    if method == 0x02:
        if spec.username is None or spec.password is None:
            raise ProxyTestError("auth_required", "proxy requires username/password")
        username = spec.username.encode("utf-8")
        password = spec.password.encode("utf-8")
        if len(username) > 255 or len(password) > 255:
            raise ProxyTestError("invalid_proxy", "SOCKS5 credentials exceed 255 bytes")
        sock.sendall(b"\x01" + bytes([len(username)]) + username + bytes([len(password)]) + password)
        auth_reply = recv_exact(sock, 2)
        if auth_reply[0] != 1 or auth_reply[1] != 0:
            raise ProxyTestError("auth_failed", "SOCKS5 username/password authentication failed")
        auth = "username_password"
    elif method == 0x00:
        auth = "not_required"
    else:
        raise ProxyTestError("auth_method_failed", f"proxy selected unsupported auth method 0x{method:02x}")

    target_bytes = target_host.encode("idna")
    if len(target_bytes) > 255:
        raise ProxyTestError("invalid_target", "target hostname exceeds SOCKS5 limit")
    request = b"\x05\x01\x00\x03" + bytes([len(target_bytes)]) + target_bytes + target_port.to_bytes(2, "big")
    sock.sendall(request)

    response = recv_exact(sock, 4)
    if response[0] != 5:
        raise ProxyTestError("proxy_protocol", "proxy returned an invalid SOCKS5 connect response")
    if response[1] != 0:
        raise ProxyTestError("proxy_connect_failed", f"SOCKS5 CONNECT rejected (reply={response[1]})")
    address_type = response[3]
    if address_type == 1:
        recv_exact(sock, 4)
    elif address_type == 3:
        length = recv_exact(sock, 1)[0]
        recv_exact(sock, length)
    elif address_type == 4:
        recv_exact(sock, 16)
    else:
        raise ProxyTestError("proxy_protocol", f"proxy returned unknown address type {address_type}")
    recv_exact(sock, 2)  # bound port
    return auth


def parse_target(url: str) -> tuple[str, int, str, bool]:
    parsed = urlsplit(url)
    if parsed.scheme.lower() not in {"http", "https"} or not parsed.hostname:
        raise ValueError("target must be an http:// or https:// URL with a hostname")
    scheme = parsed.scheme.lower()
    port = parsed.port or (443 if scheme == "https" else 80)
    path = parsed.path or "/"
    if parsed.query:
        path += f"?{parsed.query}"
    return parsed.hostname, port, path, scheme == "https"


def request_target(
    sock: socket.socket,
    target_host: str,
    target_port: int,
    target_path: str,
    use_tls: bool,
    timeout: float,
) -> tuple[int, bytes]:
    connection: socket.socket = sock
    if use_tls:
        context = ssl.create_default_context()
        connection = context.wrap_socket(sock, server_hostname=target_host)
    connection.settimeout(timeout)
    host_header = target_host
    if (use_tls and target_port != 443) or (not use_tls and target_port != 80):
        host_header = f"{host_header}:{target_port}"
    request = (
        f"GET {target_path} HTTP/1.1\r\n"
        f"Host: {host_header}\r\n"
        "User-Agent: proxy-connectivity-check/1.0\r\n"
        "Accept: application/json\r\n"
        "Connection: close\r\n\r\n"
    ).encode("ascii")
    connection.sendall(request)
    response = http.client.HTTPResponse(connection)
    response.begin()
    body = response.read(2 * 1024 * 1024)
    status = response.status
    response.close()
    if connection is not sock:
        connection.close()
    return status, body


def test_one(
    index: int,
    value: str,
    target: tuple[str, int, str, bool],
    timeout: float,
    attempt: int = 1,
) -> TestResult:
    started = time.monotonic()
    # Keep malformed entries anonymous too; the raw value may contain credentials.
    display = f"proxy#{index}"
    auth = ""
    exit_ip = ""
    try:
        spec = parse_proxy(value)
        display = spec.display
        target_host, target_port, target_path, use_tls = target
        with socket.create_connection((spec.host, spec.port), timeout=timeout) as sock:
            sock.settimeout(timeout)
            auth = socks5_connect(sock, spec, target_host, target_port)
            try:
                http_status, body = request_target(
                    sock, target_host, target_port, target_path, use_tls, timeout
                )
            except (socket.timeout, TimeoutError):
                raise
            except (http.client.HTTPException, ssl.SSLError, ConnectionError, OSError) as exc:
                raise ProxyTestError("target_request_failed", str(exc)) from exc
        if not 200 <= http_status < 300:
            raise ProxyTestError("target_http_failed", f"target returned HTTP {http_status}")
        try:
            payload = json.loads(body.decode("utf-8-sig"))
            exit_ip = str(payload.get("Ip", "")).strip()
            ipaddress.ip_address(exit_ip)
        except (UnicodeDecodeError, json.JSONDecodeError, AttributeError, ValueError, TypeError) as exc:
            raise ProxyTestError("target_response_invalid", "target response has no valid Ip field") from exc
        return TestResult(index, display, "success", "success", "ok", exit_ip, auth, elapsed_ms(started), attempt)
    except ProxyTestError as exc:
        return TestResult(index, display, "failed", exc.category, str(exc), exit_ip, auth, elapsed_ms(started), attempt)
    except socket.gaierror as exc:
        return TestResult(index, display, "failed", "proxy_dns_failed", str(exc), exit_ip, auth, elapsed_ms(started), attempt)
    except (socket.timeout, TimeoutError) as exc:
        return TestResult(index, display, "failed", "timeout", str(exc) or "timed out", exit_ip, auth, elapsed_ms(started), attempt)
    except (ConnectionError, OSError) as exc:
        return TestResult(index, display, "failed", "proxy_connect_failed", str(exc), exit_ip, auth, elapsed_ms(started), attempt)
    except (http.client.HTTPException, ssl.SSLError) as exc:
        return TestResult(index, display, "failed", "target_request_failed", str(exc), exit_ip, auth, elapsed_ms(started), attempt)
    except Exception as exc:  # noqa: BLE001 - preserve one result per proxy
        return TestResult(index, display, "failed", "unexpected_error", str(exc), exit_ip, auth, elapsed_ms(started), attempt)


def elapsed_ms(started: float) -> int:
    return int((time.monotonic() - started) * 1000)


def write_report(
    path: Path,
    results: Iterable[TestResult],
    target_url: str,
    timeout: float,
    concurrency: int,
    requests_per_proxy: int,
) -> None:
    payload = {
        "target": target_url,
        "timeout_seconds": timeout,
        "concurrency": concurrency,
        "requests_per_proxy": requests_per_proxy,
        "results": [asdict(result) for result in results],
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="Test SOCKS5 proxy connectivity, credentials, and myip access.")
    parser.add_argument("-p", "--proxy-file", type=Path, default=DEFAULT_PROXY_FILE)
    parser.add_argument("-u", "--url", default=DEFAULT_URL, help=f"target URL (default: {DEFAULT_URL})")
    parser.add_argument("-c", "--concurrency", type=int, default=10)
    parser.add_argument(
        "-n",
        "--requests-per-proxy",
        type=int,
        default=1,
        help="number of independent tests for each proxy",
    )
    parser.add_argument("-t", "--timeout", type=float, default=15.0, help="timeout per proxy in seconds")
    parser.add_argument("--report", type=Path, help="write a JSON report without credentials")
    args = parser.parse_args()

    if args.concurrency < 1 or args.requests_per_proxy < 1 or args.timeout <= 0:
        parser.error("concurrency and requests-per-proxy must be positive, timeout must be greater than zero")
    try:
        target = parse_target(args.url)
        values = read_proxy_values(args.proxy_file)
    except (OSError, ValueError) as exc:
        parser.error(str(exc))
    if not values:
        parser.error("proxy file contains no proxy entries")

    total_tests = len(values) * args.requests_per_proxy
    print(
        f"proxy-check loaded={len(values)} target={args.url} "
        f"concurrency={args.concurrency} requestsPerProxy={args.requests_per_proxy} total={total_tests}"
    )
    results: list[TestResult] = []
    with ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        futures = {
            executor.submit(test_one, index, value, target, args.timeout, attempt): (index, attempt)
            for index, value in enumerate(values, start=1)
            for attempt in range(1, args.requests_per_proxy + 1)
        }
        for future in as_completed(futures):
            result = future.result()
            results.append(result)
            suffix = f" ip={result.exit_ip}" if result.exit_ip else ""
            reason = ""
            if result.status != "success" and result.message:
                # Keep one failed result on one console line; messages may
                # contain newlines when they originate from a network error.
                reason = f" reason={' '.join(result.message.split())}"
            print(
                f"progress={len(results)}/{total_tests} proxy=#{result.index} "
                f"attempt={result.attempt}/{args.requests_per_proxy} "
                f"status={result.status} category={result.category} "
                f"elapsed_ms={result.elapsed_ms}{suffix}{reason}"
            )

    results.sort(key=lambda item: (item.index, item.attempt))
    counts: dict[str, int] = {}
    for result in results:
        counts[result.category] = counts.get(result.category, 0) + 1
    summary = " ".join(f"{category}={counts[category]}" for category in sorted(counts))
    print(
        f"proxy-check summary proxies={len(values)} requestsPerProxy={args.requests_per_proxy} "
        f"total={len(results)} {summary}"
    )
    if args.report:
        write_report(args.report, results, args.url, args.timeout, args.concurrency, args.requests_per_proxy)
        print(f"proxy-check report={args.report}")
    return 0 if all(result.status == "success" for result in results) else 1


if __name__ == "__main__":
    sys.exit(main())
