"""Sandboxed process/network tests (Fase: the last 43 flags).

pgrep, pkill, nc and ss cannot be tested on the host: their whole job is
to look at or signal OTHER processes, so a test that runs them against
the real machine either proves nothing (no match) or is dangerous (the
`pkill .` incident killed the developer's session).

Inside a container they are safe and meaningful: the container sees only
its own processes, so a test can spawn a uniquely-named victim, signal
it, and assert the effect — with no reach beyond the sandbox.

Run with:  python parity/sandbox.py --unish ./unish-linux-arm64
Requires a Docker daemon reachable as `sudo -n docker` (WSL) or `docker`.
"""
import argparse
import os
import subprocess
import sys
import tempfile

IMAGE = "alpine:3.20"  # busybox nc is enough as the peer
PLATFORM = "linux/arm64"

# (name, script, expected_stdout, expected_exit)
CASES = [
    # --- pgrep: unish's builtin, looking at a victim the container owns ---
    ("pgrep finds a spawned victim",
     "sleep 30 & sleep 0.3; /tmp/unish -c \"pgrep -f 'sleep 30' | grep -cE '^[0-9]+$'\"",
     "1\n", 0),
    ("pgrep -c counts matches",
     "sleep 30 & sleep 0.3; /tmp/unish -c \"pgrep -c -f 'sleep 30'\"",
     "1\n", 0),
    ("pgrep -l lists the name",
     "sleep 30 & sleep 0.3; /tmp/unish -c \"pgrep -l -f 'sleep 30' | grep -c sleep\"",
     "1\n", 0),
    # FINDING: pgrep -x compares ^name$ against the full cmdline's base
    # ('sleep 30'), so it never matches a process started with arguments.
    ("FINDING pgrep -x exact matches the command name",
     "sleep 30 & sleep 0.3; /tmp/unish -c \"pgrep -x sleep | grep -cE '^[0-9]+$'\"",
     "1\n", 0),
    ("pgrep -i case-insensitive",
     "sleep 30 & sleep 0.3; /tmp/unish -c \"pgrep -i -f 'SLEEP 30' | grep -cE '^[0-9]+$'\"",
     "1\n", 0),
    ("pgrep --count long form",
     "sleep 30 & sleep 0.3; /tmp/unish -c \"pgrep --count -f 'sleep 30'\"",
     "1\n", 0),
    # The pattern must not appear in this script's own cmdline (PID 1 runs
    # `sh -c <script>`, and -f would match it). Build it at run time.
    ("pgrep no match exits 1",
     "/tmp/unish -c 'pgrep -f zzq; echo rc=$?'",
     "rc=1\n", 0),

    # --- pkill: unish's builtin, killing only the container's victim ---
    ("pkill terminates the victim",
     "sleep 30 & V=$!; sleep 0.3; /tmp/unish -c \"pkill -f 'sleep 30'\"; sleep 0.3; kill -0 $V 2>/dev/null; echo rc=$?",
     "rc=1\n", 0),
    ("pkill -e echoes the killed name",
     "printf '#!/bin/sh\nsleep 30\n' > /tmp/victe; chmod +x /tmp/victe; /tmp/victe & sleep 0.3; /tmp/unish -c \"pkill -e -x victe\" | grep -c victe",
     "1\n", 0),
    # -x matches the exact command name; use a victim whose name is unique
    # so the assertion cannot be confused by another sleep in the container.
    ("pkill -x exact",
     "printf '#!/bin/sh\nsleep 30\n' > /tmp/victimxyz; chmod +x /tmp/victimxyz; /tmp/victimxyz & V=$!; sleep 0.3; /tmp/unish -c \"pkill -x victimxyz\"; sleep 0.3; kill -0 $V 2>/dev/null; echo rc=$?",
     "rc=1\n", 0),
    ("pkill -i case-insensitive",
     "sleep 30 & V=$!; sleep 0.3; /tmp/unish -c \"pkill -i -f 'SLEEP 30'\"; sleep 0.3; kill -0 $V 2>/dev/null; echo rc=$?",
     "rc=1\n", 0),
    ("FINDING pkill -s chooses the signal",
     "sleep 30 & V=$!; sleep 0.3; /tmp/unish -c \"pkill -s TERM -f 'sleep 30'\"; sleep 0.3; kill -0 $V 2>/dev/null; echo rc=$?",
     "rc=1\n", 0),
    ("FINDING pkill --signal long form",
     "sleep 30 & V=$!; sleep 0.3; /tmp/unish -c \"pkill --signal TERM -f 'sleep 30'\"; sleep 0.3; kill -0 $V 2>/dev/null; echo rc=$?",
     "rc=1\n", 0),
    ("FINDING pkill no match exits 1",
     "/tmp/unish -c 'pkill -f zzq; echo rc=$?'",
     "rc=1\n", 0),

    # --- ss: unish's builtin, looking at a socket the container opened ---
    # `ss -t` shows non-listening tcp states; a listener needs -l (GNU is
    # the same, verified on the host). So the assertion is on -tl.
    ("ss -tl shows a listening tcp socket",
     "nc -l -p 34567 & sleep 0.5; /tmp/unish -c \"ss -tl\" | grep -c 34567",
     "1\n", 0),
    ("ss -l lists listening",
     "nc -l -p 34568 & sleep 0.5; /tmp/unish -c \"ss -l\" | grep -c 34568",
     "1\n", 0),
    ("ss -a shows all",
     "nc -l -p 34569 & sleep 0.5; /tmp/unish -c \"ss -a\" | grep -c 34569",
     "1\n", 0),
    ("ss -tln numeric listing",
     "nc -l -p 34570 & sleep 0.5; /tmp/unish -c \"ss -tln\" | grep -c 34570",
     "1\n", 0),
    ("ss -u udp",
     "nc -u -l -p 34571 & sleep 0.5; /tmp/unish -c \"ss -u\" | grep -c 34571",
     "1\n", 0),
    ("ss --tcp --listening long form",
     "nc -l -p 34572 & sleep 0.5; /tmp/unish -c \"ss --tcp --listening\" | grep -c 34572",
     "1\n", 0),
    ("ss --listening long form",
     "nc -l -p 34573 & sleep 0.5; /tmp/unish -c \"ss --listening\" | grep -c 34573",
     "1\n", 0),
    ("ss --all long form",
     "nc -l -p 34574 & sleep 0.5; /tmp/unish -c \"ss --all\" | grep -c 34574",
     "1\n", 0),
    ("ss --numeric --listening long form",
     "nc -l -p 34575 & sleep 0.5; /tmp/unish -c \"ss --numeric --listening\" | grep -c 34575",
     "1\n", 0),
    ("ss --udp long form",
     "nc -u -l -p 34576 & sleep 0.5; /tmp/unish -c \"ss --udp\" | grep -c 34576",
     "1\n", 0),

    # --- nc: unish's builtin, talking to the container's nc ---
    # FINDING: pipeConn returns as soon as EITHER direction ends, so the
    # stdout copier (which finishes first when the peer does not reply)
    # closes the connection before the stdin copier has sent the data.
    ("FINDING nc sends data to a listener",
     "nc -l -p 34580 > got.txt & sleep 0.5; /tmp/unish -c \"printf 'hello\\n' | nc -w1 127.0.0.1 34580\"; sleep 0.5; cat got.txt",
     "hello\n", 0),
    ("nc -z zero-io probe",
     "nc -l -p 34581 & sleep 0.5; /tmp/unish -c \"nc -z 127.0.0.1 34581; echo rc=$?\"",
     "rc=0\n", 0),
    ("FINDING nc -u udp send",
     "(nc -u -l -p 34582 > u.txt &) ; sleep 0.5; /tmp/unish -c \"printf 'udp\\n' | nc -u -w1 127.0.0.1 34582\"; sleep 0.5; cat u.txt",
     "udp\n", 0),
    ("nc -w timeout accepted",
     "nc -l -p 34583 & sleep 0.5; /tmp/unish -c \"nc -w 1 -z 127.0.0.1 34583; echo rc=$?\"",
     "rc=0\n", 0),
    ("nc -p source port",
     "nc -l -p 34585 & sleep 0.5; /tmp/unish -c \"nc -p 34586 -z 127.0.0.1 34585; echo rc=$?\"",
     "rc=0\n", 0),
]


def docker_cmd():
    """Return the docker argv prefix that works on this machine."""
    for prefix in (["sudo", "-n", "docker"], ["docker"]):
        try:
            p = subprocess.run(prefix + ["ps"], capture_output=True, timeout=15)
            if p.returncode == 0:
                return prefix
        except Exception:  # noqa: BLE001
            continue
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--unish", required=True, help="linux binary to test")
    ap.add_argument("--platform", default=PLATFORM,
                    help="container platform (linux/arm64 or linux/amd64)")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    dk = docker_cmd()
    if not dk:
        print("=== sandbox: SKIPPED (no reachable docker daemon)")
        return 0

    unish = os.path.abspath(args.unish)
    if not os.path.exists(unish):
        print("sandbox: binary not found: %s" % unish)
        return 1

    total = failed = gaps = 0
    for name, script, want_out, want_rc in CASES:
        total += 1
        # Copy the binary into a throwaway container and run the case there.
        # The container sees only its own processes, so pgrep/pkill/nc are
        # both meaningful and harmless.
        cmd = [
            *dk, "run", "--rm", "--platform", args.platform,
            "-v", "%s:/u:ro" % unish,
            IMAGE, "sh", "-c",
            "cp /u /tmp/unish && chmod +x /tmp/unish && cd /tmp && " + script,
        ]
        try:
            p = subprocess.run(cmd, capture_output=True, timeout=120)
        except subprocess.TimeoutExpired:
            failed += 1
            print("FAIL  [sandbox] %s (timeout)" % name)
            continue
        got = p.stdout.replace(b"\r\n", b"\n").decode("utf-8", "replace")
        if got == want_out and p.returncode == want_rc:
            if args.verbose:
                print("ok    [sandbox] %s" % name)
        elif name.startswith("FINDING"):
            gaps += 1
            print("KNOWN GAP [sandbox] %s" % name)
            if args.verbose:
                print("      want %r/%d got %r/%d" % (want_out, want_rc, got, p.returncode))
        else:
            failed += 1
            print("FAIL  [sandbox] %s" % name)
            print("      script: %r" % script)
            print("      want %r exit %d" % (want_out, want_rc))
            print("      got  %r exit %d" % (got, p.returncode))
            if p.stderr:
                print("      stderr: %r" % p.stderr[:200])
    print("=== sandbox: %d cases, %d failures, %d known gaps" % (total, failed, gaps))
    return failed


if __name__ == "__main__":
    sys.exit(main())
