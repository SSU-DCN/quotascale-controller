from __future__ import annotations

import argparse
import csv
import gzip
import struct
from collections import Counter
from dataclasses import dataclass
from datetime import datetime, timezone, timedelta
from pathlib import Path


REQUEST_STRUCT = struct.Struct(">IIII4B")
PARIS_OFFSET_SECONDS = 2 * 60 * 60
PARIS_TZ = timezone(timedelta(seconds=PARIS_OFFSET_SECONDS))


@dataclass
class SourceSummary:
    file: str
    requests: int
    start_gmt: int
    end_gmt: int
    start_local: int
    end_local: int


def read_counts(path: Path) -> tuple[Counter[int], SourceSummary]:
    counts: Counter[int] = Counter()
    total = 0
    start_ts = None
    end_ts = None

    with gzip.open(path, "rb") as handle:
        while True:
            chunk = handle.read(REQUEST_STRUCT.size)
            if not chunk:
                break
            if len(chunk) != REQUEST_STRUCT.size:
                raise ValueError(f"truncated record in {path}")
            timestamp, _, _, _, _, _, _, _ = REQUEST_STRUCT.unpack(chunk)
            counts[timestamp] += 1
            total += 1
            if start_ts is None:
                start_ts = timestamp
            end_ts = timestamp

    if start_ts is None or end_ts is None:
        raise ValueError(f"no records found in {path}")

    return counts, SourceSummary(
        file=path.name,
        requests=total,
        start_gmt=start_ts,
        end_gmt=end_ts,
        start_local=start_ts + PARIS_OFFSET_SECONDS,
        end_local=end_ts + PARIS_OFFSET_SECONDS,
    )


def choose_window(counts: Counter[int], window_seconds: int) -> tuple[int, int]:
    timestamps = sorted(counts)
    if not timestamps:
        raise ValueError("no timestamps available")

    left = 0
    current = 0
    best_sum = -1
    best_start = timestamps[0]
    best_end = timestamps[0]

    for right, ts in enumerate(timestamps):
        current += counts[ts]
        while timestamps[right] - timestamps[left] >= window_seconds:
            current -= counts[timestamps[left]]
            left += 1
        if current > best_sum:
            best_sum = current
            best_start = timestamps[left]
            best_end = best_start + window_seconds - 1

    return best_start, best_end


def moving_average(values: list[float], width: int) -> list[float]:
    if width <= 1:
        return values[:]

    radius = width // 2
    smoothed: list[float] = []
    for index in range(len(values)):
        start = max(0, index - radius)
        end = min(len(values), index + radius + 1)
        smoothed.append(sum(values[start:end]) / (end - start))
    return smoothed


def normalize(values: list[float], peak: float) -> list[float]:
    current_peak = max(values)
    if current_peak <= 0:
        raise ValueError("trace contains no traffic")
    scale = peak / current_peak
    return [round(value * scale, 4) for value in values]


def write_trace_csv(path: Path, values: list[float]) -> None:
    with path.open("w", newline="", encoding="ascii") as handle:
        writer = csv.writer(handle)
        writer.writerow(["second", "rps"])
        for second, value in enumerate(values):
            writer.writerow([second, value])


def write_summary(
    path: Path,
    summary: SourceSummary,
    window_start: int,
    window_end: int,
    peak_rps: float,
    smoothing_seconds: int,
) -> None:
    def utc_iso(ts: int) -> str:
        return datetime.fromtimestamp(ts, tz=timezone.utc).isoformat()

    def local_iso(ts: int) -> str:
        return datetime.fromtimestamp(ts, tz=PARIS_TZ).isoformat()

    text = "\n".join(
        [
            f"source_file: {summary.file}",
            f"requests_in_file: {summary.requests}",
            f"file_start_gmt_epoch: {summary.start_gmt}",
            f"file_start_gmt_iso: {utc_iso(summary.start_gmt)}",
            f"file_end_gmt_epoch: {summary.end_gmt}",
            f"file_end_gmt_iso: {utc_iso(summary.end_gmt)}",
            f"file_start_local_epoch: {summary.start_local}",
            f"file_start_local_iso: {local_iso(summary.start_gmt)}",
            f"file_end_local_epoch: {summary.end_local}",
            f"file_end_local_iso: {local_iso(summary.end_gmt)}",
            f"selected_window_start_gmt_epoch: {window_start}",
            f"selected_window_start_gmt_iso: {utc_iso(window_start)}",
            f"selected_window_end_gmt_epoch: {window_end}",
            f"selected_window_end_gmt_iso: {utc_iso(window_end)}",
            f"selected_window_start_local_epoch: {window_start + PARIS_OFFSET_SECONDS}",
            f"selected_window_start_local_iso: {local_iso(window_start)}",
            f"selected_window_end_local_epoch: {window_end + PARIS_OFFSET_SECONDS}",
            f"selected_window_end_local_iso: {local_iso(window_end)}",
            f"normalized_peak_rps: {peak_rps}",
            f"smoothing_seconds: {smoothing_seconds}",
            "",
        ]
    )
    path.write_text(text, encoding="ascii")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--summary", required=True, type=Path)
    parser.add_argument("--window-seconds", type=int, default=1800)
    parser.add_argument("--peak-rps", type=float, required=True)
    parser.add_argument("--smooth-seconds", type=int, default=11)
    args = parser.parse_args()

    counts, summary = read_counts(args.input)
    window_start, window_end = choose_window(counts, args.window_seconds)

    raw_values = [float(counts.get(ts, 0)) for ts in range(window_start, window_end + 1)]
    smoothed_values = moving_average(raw_values, args.smooth_seconds)
    normalized_values = normalize(smoothed_values, args.peak_rps)

    write_trace_csv(args.output, normalized_values)
    write_summary(
        args.summary,
        summary,
        window_start,
        window_end,
        args.peak_rps,
        args.smooth_seconds,
    )


if __name__ == "__main__":
    main()
