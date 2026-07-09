from __future__ import annotations

import csv
import os
from dataclasses import dataclass
from pathlib import Path

from locust import HttpUser, LoadTestShape, constant, task


TRACE_FILE = Path(os.getenv("WORLD_CUP_TRACE_CSV", "example/test/locust/worldcup_a.csv"))
USER_SCALE = float(os.getenv("USER_SCALE", "12.0"))
MAX_USERS = int(os.getenv("MAX_USERS", "500"))
SPAWN_RATE = float(os.getenv("SPAWN_RATE", "20.0"))
STEP_SECONDS = int(os.getenv("STEP_SECONDS", "30"))


@dataclass
class TracePoint:
    second: int
    rps: float


def load_trace(path: Path) -> list[TracePoint]:
    if not path.exists():
        raise FileNotFoundError(
            f"missing trace file: {path}. "
            "Generate a per-second CSV from the World Cup 98 logs first."
        )

    points: list[TracePoint] = []
    with path.open(newline="", encoding="ascii") as handle:
        reader = csv.DictReader(handle)
        for row in reader:
            points.append(TracePoint(second=int(row["second"]), rps=float(row["rps"])))

    if not points:
        raise ValueError(f"trace file is empty: {path}")

    return points


TRACE_POINTS = load_trace(TRACE_FILE)


def stepped_point(run_time: int) -> TracePoint | None:
    if run_time >= len(TRACE_POINTS):
        return None

    start = (run_time // STEP_SECONDS) * STEP_SECONDS
    end = min(start + STEP_SECONDS, len(TRACE_POINTS))
    bucket = TRACE_POINTS[start:end]
    avg_rps = sum(point.rps for point in bucket) / len(bucket)
    return TracePoint(second=start, rps=avg_rps)


class CpuBurnAUser(HttpUser):
    host = "http://placeholder.invalid"
    wait_time = constant(0)

    @task
    def burn(self):
        self.client.get("/burn?rounds=240000", name="/burn")


class CpuBurnBUser(HttpUser):
    host = "http://placeholder.invalid"
    wait_time = constant(0)

    @task
    def derive(self):
        self.client.get("/derive?iterations=350000", name="/derive")


class WorldCupReplayShape(LoadTestShape):
    """
    Replays a stepped demand shape derived from the public World Cup 98 logs.
    The trace is bucketed into STEP_SECONDS windows to avoid unstable user churn.
    """

    def tick(self):
        run_time = int(self.get_run_time())
        point = stepped_point(run_time)
        if point is None:
            return None

        target_users = min(MAX_USERS, max(1, round(point.rps * USER_SCALE)))
        return (target_users, SPAWN_RATE)
