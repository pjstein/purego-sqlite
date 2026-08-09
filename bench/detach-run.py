#!/usr/bin/env python3
"""Start the bake-off in its own session so nothing else can kill it.

Not defensiveness for its own sake: a first attempt at this run cleared the
quiet gate, wrote its environment record, and was then killed as collateral
when an unrelated watcher process in the same process group was reaped. A
forty-minute measurement should not be at the mercy of whoever else shares a
process group with it.
"""
import os
import subprocess
import sys

cmd = sys.argv[1:] or ["bash", "-c",
                       "./run-bakeoff.sh 5 30s 200000 && ./run-micro.sh 5 3s && echo ALL_RUNS_COMPLETE"]

pid = os.fork()
if pid > 0:
    print(f"detached run started, child pid {pid}")
    sys.exit(0)

os.setsid()                      # new session: no controlling terminal, own process group
with open("results/driver.log", "w") as log:
    os.dup2(log.fileno(), 1)
    os.dup2(log.fileno(), 2)
with open(os.devnull) as devnull:
    os.dup2(devnull.fileno(), 0)
os.execvp(cmd[0], cmd)
