from __future__ import annotations

import csv
import os
from dataclasses import dataclass
from pathlib import Path

from locust import HttpUser, LoadTestShape, constant_throughput, task


TRACE_FILE = Path(os.getenv("WORLD_CUP_TRACE_CSV", "example/test/locust/worldcup_a.csv"))
RPS_SCALE = float(os.getenv("RPS_SCALE", "80.0"))
MAX_USERS = int(os.getenv("MAX_USERS", "3000"))
SPAWN_FLOOR = float(os.getenv("SPAWN_FLOOR", "1.0"))
SPAWN_FACTOR = float(os.getenv("SPAWN_FACTOR", "3.0"))


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


class PodinfoUser(HttpUser):
    host = "http://placeholder.invalid"
    wait_time = constant_throughput(25)

    @task(5)
    def root(self):
        self.client.get("/")

    @task(2)
    def headers(self):
        self.client.get("/headers")

    @task(1)
    def delay(self):
        self.client.get("/delay/1")


class HttpbinUser(HttpUser):
    host = "http://placeholder.invalid"
    wait_time = constant_throughput(25)

    @task(5)
    def get(self):
        self.client.get("/get")

    @task(2)
    def headers(self):
        self.client.get("/headers")

    @task(1)
    def bytes(self):
        self.client.get("/bytes/262144", name="/bytes/[256KiB]")


class WorldCupReplayShape(LoadTestShape):
    """
    Replays a normalized traffic shape derived from the public World Cup 98 logs.
    Run one Locust process per namespace so each process can follow its own trace.
    """

    def tick(self):
        run_time = int(self.get_run_time())
        if run_time >= len(TRACE_POINTS):
            return None

        point = TRACE_POINTS[run_time]
        target_users = min(MAX_USERS, max(1, round(point.rps * RPS_SCALE)))
        spawn_rate = max(SPAWN_FLOOR, min(float(target_users), point.rps * RPS_SCALE * SPAWN_FACTOR))
        return (target_users, spawn_rate)
