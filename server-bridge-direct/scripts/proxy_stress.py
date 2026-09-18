#!/usr/bin/env python3
"""SOCKS5 代理并发稳定性压测：固定并发持续打流量，按窗口观察是否劣化。

和 ``test_socks5_proxies.py`` 的分工：那个脚本回答「这批代理现在通不通、
账号密码对不对」，每个代理跑固定次数就结束；这个脚本回答「持续压下去稳不稳」，
按时长跑，输出分窗口的成功率、延迟分位和错误分类，用来抓间歇性故障——
代理侧连接数打满、上游偶发 RST、桥重建监听造成的窗口、出口 IP 中途漂移。

SOCKS5 握手、目标请求等逻辑直接复用 test_socks5_proxies.py，不重复实现。

每次请求都新建一条 TCP 连接，这与 bridge 的转发模型一致（bridge 是 accept
一条就拨一条上游，没有连接池），所以这里也不做连接复用。

用法示例：
    python proxy_stress.py -p D:\\work\\bridge11-proxy.csv -c 50 -d 300
    python proxy_stress.py --proxy socks5://user:pass@1.2.3.4:41290 -c 20 -d 60
    python proxy_stress.py -p proxies.txt -c 30 -d 600 --interval 30 --report soak.json

口令不会被打印，报告里也只保留代理的 host:port 与出口 IP。
"""

from __future__ import annotations

import argparse
import json
import signal
import statistics
import sys
import threading
import time
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path

try:
    from test_socks5_proxies import (  # 同目录脚本，避免重复实现 SOCKS5
        DEFAULT_PROXY_FILE,
        DEFAULT_URL,
        ProxyTestError,
        parse_target,
        read_proxy_values,
        safe_console_text,
        test_one,
    )
except ImportError as exc:  # pragma: no cover - 运行位置不对时给出可操作的提示
    raise SystemExit(
        "无法导入 test_socks5_proxies.py，请在 scripts/ 目录下运行本脚本，"
        f"或把该目录加入 PYTHONPATH（原始错误：{exc}）"
    ) from exc


@dataclass
class ProxyStats:
    """单个代理的累计统计。索引与命令行加载顺序一致。"""

    index: int
    display: str = ""
    proxy_ip: str = ""
    proxy_port: int = 0
    account: str = ""
    attempts: int = 0
    successes: int = 0
    categories: Counter[str] = field(default_factory=Counter)
    latencies_ms: list[int] = field(default_factory=list)
    exit_ips: Counter[str] = field(default_factory=Counter)
    # 连续失败是间歇性故障最直接的信号：成功率 99% 但连续挂 40 次，
    # 说明有一段时间完全不可用，和均匀分布的 1% 抖动是两回事
    current_streak: int = 0
    max_fail_streak: int = 0
    first_failure_at: float | None = None
    last_failure_message: str = ""

    def record(self, result, started_at: float) -> None:
        self.attempts += 1
        self.categories[result.category] += 1
        self.latencies_ms.append(result.elapsed_ms)
        if not self.display:
            self.display = result.proxy
            self.proxy_ip = result.proxy_ip
            self.proxy_port = result.proxy_port
            self.account = result.account
        if result.status == "success":
            self.successes += 1
            self.current_streak = 0
            if result.exit_ip:
                self.exit_ips[result.exit_ip] += 1
            return
        self.current_streak += 1
        self.max_fail_streak = max(self.max_fail_streak, self.current_streak)
        if self.first_failure_at is None:
            self.first_failure_at = started_at
        if result.message:
            self.last_failure_message = " ".join(result.message.split())

    @property
    def success_rate(self) -> float:
        return self.successes / self.attempts if self.attempts else 0.0


@dataclass
class WindowStats:
    """一个报告窗口内的统计，用来看压测过程中是否随时间劣化。"""

    attempts: int = 0
    successes: int = 0
    categories: Counter[str] = field(default_factory=Counter)
    latencies_ms: list[int] = field(default_factory=list)

    def record(self, result) -> None:
        self.attempts += 1
        self.categories[result.category] += 1
        self.latencies_ms.append(result.elapsed_ms)
        if result.status == "success":
            self.successes += 1


def percentile(values: list[int], fraction: float) -> int:
    """最近排名法取分位。样本量小时比插值更容易和日志里的真实值对上。"""

    if not values:
        return 0
    ordered = sorted(values)
    rank = max(1, min(len(ordered), round(fraction * len(ordered))))
    return ordered[rank - 1]


def latency_summary(values: list[int]) -> dict[str, int]:
    if not values:
        return {"p50": 0, "p90": 0, "p99": 0, "max": 0, "avg": 0}
    return {
        "p50": percentile(values, 0.50),
        "p90": percentile(values, 0.90),
        "p99": percentile(values, 0.99),
        "max": max(values),
        "avg": int(statistics.fmean(values)),
    }


class Stopper:
    """时长到点、达到请求上限或 Ctrl+C 都通过它统一收敛。"""

    def __init__(self, duration: float | None, max_requests: int | None) -> None:
        self._event = threading.Event()
        self._deadline = time.monotonic() + duration if duration else None
        self._max_requests = max_requests
        self._lock = threading.Lock()
        self._issued = 0

    def stop(self) -> None:
        self._event.set()

    @property
    def stopped(self) -> bool:
        if self._event.is_set():
            return True
        if self._deadline is not None and time.monotonic() >= self._deadline:
            return True
        return False

    def claim(self) -> bool:
        """领一个请求配额。返回 False 表示该收工了。"""

        if self.stopped:
            return False
        if self._max_requests is None:
            return True
        with self._lock:
            if self._issued >= self._max_requests:
                return False
            self._issued += 1
            return True


def worker(
    worker_id: int,
    values: list[str],
    target,
    timeout: float,
    stopper: Stopper,
    sink,
    think_time: float,
) -> None:
    """一个并发单元：不停地取下一个代理发请求，直到 stopper 说停。

    代理的选取用 worker_id 错开起点再轮转，避免所有 worker 在同一时刻打同一个
    代理——那样测出来的是单代理的排队延迟，不是整体稳定性。
    """

    position = worker_id % len(values)
    attempt = 0
    while stopper.claim():
        index = position % len(values)
        position += 1
        attempt += 1
        started_at = time.time()
        result = test_one(index + 1, values[index], target, timeout, attempt)
        sink(result, started_at)
        if think_time > 0 and not stopper.stopped:
            time.sleep(think_time)


def format_categories(categories: Counter[str]) -> str:
    return " ".join(f"{name}={count}" for name, count in sorted(categories.items()))


def build_report(
    args,
    started_wall: float,
    elapsed: float,
    total: WindowStats,
    windows: list[dict[str, object]],
    per_proxy: dict[int, ProxyStats],
) -> dict[str, object]:
    return {
        "target": args.url,
        "concurrency": args.concurrency,
        "timeout_seconds": args.timeout,
        "duration_seconds": round(elapsed, 3),
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%S%z", time.localtime(started_wall)),
        "proxies": len(per_proxy),
        "attempts": total.attempts,
        "successes": total.successes,
        "success_rate": round(total.successes / total.attempts, 6) if total.attempts else 0.0,
        "rps": round(total.attempts / elapsed, 3) if elapsed > 0 else 0.0,
        "latency_ms": latency_summary(total.latencies_ms),
        "categories": dict(sorted(total.categories.items())),
        "windows": windows,
        # 报告里不含账号与口令，只留能定位来源的 host:port
        "per_proxy": [
            {
                "index": stats.index,
                "proxy": stats.display,
                "attempts": stats.attempts,
                "successes": stats.successes,
                "success_rate": round(stats.success_rate, 6),
                "max_fail_streak": stats.max_fail_streak,
                "latency_ms": latency_summary(stats.latencies_ms),
                "categories": dict(sorted(stats.categories.items())),
                "exit_ips": dict(sorted(stats.exit_ips.items())),
                "last_failure": stats.last_failure_message,
            }
            for stats in sorted(per_proxy.values(), key=lambda item: item.index)
        ],
    }


def main() -> int:
    parser = argparse.ArgumentParser(
        description="SOCKS5 代理并发稳定性压测：固定并发持续打流量，按窗口输出成功率与延迟分位。"
    )
    source = parser.add_mutually_exclusive_group()
    source.add_argument("-p", "--proxy-file", type=Path, default=DEFAULT_PROXY_FILE,
                        help=f"代理清单 CSV/TXT（默认 {DEFAULT_PROXY_FILE}）")
    source.add_argument("--proxy", action="append", metavar="URL",
                        help="直接指定代理，可重复；给了它就不读文件")
    parser.add_argument("-u", "--url", default=DEFAULT_URL, help=f"目标 URL（默认 {DEFAULT_URL}）")
    parser.add_argument("-c", "--concurrency", type=int, default=20, help="并发 worker 数（默认 20）")
    parser.add_argument("-d", "--duration", type=float, default=60.0,
                        help="持续时长(秒)，默认 60；配合 -n 时谁先到谁停")
    parser.add_argument("-n", "--max-requests", type=int,
                        help="请求总数上限，不给则只按时长跑")
    parser.add_argument("-t", "--timeout", type=float, default=15.0, help="单次请求超时(秒)，默认 15")
    parser.add_argument("--interval", type=float, default=10.0, help="窗口报告间隔(秒)，默认 10")
    parser.add_argument("--think-time", type=float, default=0.0,
                        help="每个 worker 两次请求之间的间隔(秒)，默认 0=尽可能快")
    parser.add_argument("--min-success-rate", type=float, default=0.99,
                        help="判定通过的成功率下限，默认 0.99")
    parser.add_argument("--report", type=Path, help="输出 JSON 报告（不含账号口令）")
    args = parser.parse_args()

    if args.concurrency < 1:
        parser.error("--concurrency 必须 >= 1")
    if args.timeout <= 0 or args.interval <= 0:
        parser.error("--timeout 和 --interval 必须大于 0")
    if args.duration is not None and args.duration <= 0:
        parser.error("--duration 必须大于 0")
    if args.max_requests is not None and args.max_requests < 1:
        parser.error("--max-requests 必须 >= 1")
    if args.think_time < 0:
        parser.error("--think-time 不能为负")
    if not 0 < args.min_success_rate <= 1:
        parser.error("--min-success-rate 必须落在 (0, 1]")

    try:
        target = parse_target(args.url)
        values = list(args.proxy) if args.proxy else read_proxy_values(args.proxy_file)
    except (OSError, ValueError, ProxyTestError) as exc:
        parser.error(str(exc))
    if not values:
        parser.error("没有可用的代理条目")

    lock = threading.Lock()
    total = WindowStats()
    window = WindowStats()
    windows: list[dict[str, object]] = []
    per_proxy: dict[int, ProxyStats] = {}
    started = time.monotonic()
    started_wall = time.time()
    window_started = started

    def sink(result, started_at: float) -> None:
        nonlocal window, window_started
        with lock:
            total.record(result)
            window.record(result)
            stats = per_proxy.get(result.index)
            if stats is None:
                stats = ProxyStats(index=result.index)
                per_proxy[result.index] = stats
            stats.record(result, started_at)

            now = time.monotonic()
            if now - window_started >= args.interval:
                windows.append(flush_window(window, window_started, now, started))
                window = WindowStats()
                window_started = now

    def flush_window(current: WindowStats, begin: float, end: float, origin: float, final: bool = False) -> dict[str, object]:
        span = max(end - begin, 1e-9)
        rate = current.successes / current.attempts if current.attempts else 0.0
        latency = latency_summary(current.latencies_ms)
        snapshot = {
            "at_seconds": round(end - origin, 3),
            "final": final,
            "attempts": current.attempts,
            "successes": current.successes,
            "success_rate": round(rate, 6),
            "rps": round(current.attempts / span, 3),
            "latency_ms": latency,
            "categories": dict(sorted(current.categories.items())),
        }
        failures = format_categories(
            Counter({name: count for name, count in current.categories.items() if name != "success"})
        )
        # 末尾那段通常不足一个 interval，单独标出来，避免把它的 rps 当成稳态值
        label = "window(final)" if final else "window"
        print(
            f"{label} t={snapshot['at_seconds']:.0f}s attempts={current.attempts} "
            f"ok={current.successes} rate={rate:.4f} rps={snapshot['rps']:.1f} "
            f"p50={latency['p50']} p90={latency['p90']} p99={latency['p99']} max={latency['max']}"
            + (f" {failures}" if failures else ""),
            flush=True,
        )
        return snapshot

    stopper = Stopper(args.duration, args.max_requests)

    def on_signal(signum, frame) -> None:  # noqa: ARG001 - 签名由 signal 决定
        print("收到中断信号，正在收尾……", flush=True)
        stopper.stop()

    signal.signal(signal.SIGINT, on_signal)
    if hasattr(signal, "SIGTERM"):
        signal.signal(signal.SIGTERM, on_signal)

    limit = f" maxRequests={args.max_requests}" if args.max_requests else ""
    print(
        f"proxy-stress proxies={len(values)} target={args.url} concurrency={args.concurrency} "
        f"duration={args.duration:g}s interval={args.interval:g}s timeout={args.timeout:g}s{limit}",
        flush=True,
    )

    threads = [
        threading.Thread(
            target=worker,
            args=(worker_id, values, target, args.timeout, stopper, sink, args.think_time),
            name=f"stress-{worker_id}",
            daemon=True,
        )
        for worker_id in range(args.concurrency)
    ]
    for thread in threads:
        thread.start()
    try:
        for thread in threads:
            # join 不给超时会吞掉 Ctrl+C（主线程收不到信号），所以分片轮询
            while thread.is_alive():
                thread.join(timeout=0.2)
    except KeyboardInterrupt:
        stopper.stop()
        for thread in threads:
            thread.join(timeout=args.timeout + 1)

    elapsed = time.monotonic() - started
    with lock:
        if window.attempts:
            windows.append(flush_window(window, window_started, time.monotonic(), started, final=True))
        attempts = total.attempts
        successes = total.successes
        latency = latency_summary(total.latencies_ms)
        categories = Counter(total.categories)
        proxy_snapshot = dict(per_proxy)

    if attempts == 0:
        print("proxy-stress 没有完成任何请求", flush=True)
        return 1

    rate = successes / attempts
    print(
        f"proxy-stress summary elapsed={elapsed:.1f}s attempts={attempts} ok={successes} "
        f"rate={rate:.4f} rps={attempts / elapsed:.1f} "
        f"p50={latency['p50']} p90={latency['p90']} p99={latency['p99']} max={latency['max']} avg={latency['avg']}",
        flush=True,
    )
    failures = {name: count for name, count in categories.items() if name != "success"}
    if failures:
        print("proxy-stress failures " + format_categories(Counter(failures)), flush=True)

    # 只列出有问题的代理：稳定性排查关心的是谁在掉，全量清单交给 --report
    problem = [
        stats
        for stats in sorted(proxy_snapshot.values(), key=lambda item: (item.success_rate, -item.max_fail_streak))
        if stats.successes < stats.attempts or len(stats.exit_ips) > 1
    ]
    if problem:
        print("proxy-stress 不稳定的代理：", flush=True)
        for stats in problem:
            ip_note = ""
            if len(stats.exit_ips) > 1:
                # 出口 IP 中途变化：代理池在轮换出口，或后端换了机器。
                # 成功率可能仍是 100%，但对需要固定出口的业务等于故障
                ip_note = f" exitIPs={len(stats.exit_ips)}"
            reason = f" reason={stats.last_failure_message}" if stats.last_failure_message else ""
            print(
                f"  proxy=#{stats.index} {safe_console_text(stats.display, '<unknown>')} "
                f"attempts={stats.attempts} ok={stats.successes} rate={stats.success_rate:.4f} "
                f"maxFailStreak={stats.max_fail_streak} p99={latency_summary(stats.latencies_ms)['p99']}"
                f"{ip_note}{reason}",
                flush=True,
            )

    if args.report:
        report = build_report(args, started_wall, elapsed, total, windows, proxy_snapshot)
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        print(f"proxy-stress report={args.report}", flush=True)

    if rate < args.min_success_rate:
        print(
            f"proxy-stress FAIL 成功率 {rate:.4f} 低于阈值 {args.min_success_rate:.4f}",
            flush=True,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
