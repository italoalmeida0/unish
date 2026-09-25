"""Cross-shell benchmark harness (Windows + WSL).

Runs the same POSIX workloads under several shells, times them with a
warmup + N runs (median), and prints a TSV. Data lives in --data dir
(generate first with gen_data.py).
"""
import argparse, os, subprocess, sys, time, statistics

WORKLOADS = [
    # (name, script)
    ("startup",          ":"),
    ("loop_test10k",     'i=0; while [ $i -lt 10000 ]; do :; i=$((i+1)); done'),
    ("loop_arith10k",    'sum=0; i=0; while [ $i -lt 10000 ]; do sum=$((sum+i)); i=$((i+1)); done; echo $sum > /dev/null'),
    ("loop_echo5k",      'i=0; while [ $i -lt 5000 ]; do echo x > /dev/null; i=$((i+1)); done'),
    ("varexp10k",        'u=abcdef; i=0; while [ $i -lt 10000 ]; do x=${u#*d}; y=${u%%b*}; z=${u/ef/EF}; i=$((i+1)); done'),
    ("cmdsubst500",      'i=0; while [ $i -lt 500 ]; do x=$(echo hi); i=$((i+1)); done'),
    ("test_ops10k",      'i=0; while [ $i -lt 10000 ]; do [ -n "x" ] && : ; [ 5 -gt 3 ] && : ; i=$((i+1)); done'),
    ("seq_100k",         'seq 100000 > /dev/null'),
    ("sort_200k",        'sort nums.txt > /dev/null'),
    ("sort_key_200k",    'sort -k2,2n pairs.txt > /dev/null'),
    ("sort_u_200k",      'sort -u nums.txt > /dev/null'),
    ("grep_big",         'grep -c foo big.txt > /dev/null'),
    ("sed_big",          "sed 's/aaa/zzz/g' big.txt > /dev/null"),
    ("cut_tr",           'cut -d, -f2 big.csv | tr a-z A-Z > /dev/null'),
    ("wc_big",           'wc -l big.txt > /dev/null'),
    ("cat_16m",          'cat big16.bin > /dev/null'),
    ("head_1k",          'head -1000 big.txt > /dev/null'),
    ("pipeline_heavy",   'cat big.txt | grep x | sed s/a/b/ | sort | uniq | wc -l > /dev/null'),
    ("glob_2k",          'for f in many/*; do :; done'),
    ("glob_echo_2k",     'echo many/* > /dev/null'),
    ("loop_words2k",     'for f in $(cat w2000.txt); do :; done'),
    ("loop_assign2k",    'i=0; while [ $i -lt 2000 ]; do x=$((i+1)); i=$x; done'),
    ("find_2k",          'find many -type f > /dev/null'),
    ("tar_2k",           'tar -cf - many > /dev/null'),
    ("printf_5k",        'i=0; while [ $i -lt 5000 ]; do printf %s x > /dev/null; i=$((i+1)); done'),
]

def run_once(argv, script, cwd, env=None, repeat=1):
    if repeat > 1:
        script = "\n".join([script] * repeat)
    t0 = time.perf_counter()
    p = subprocess.run(argv + ["-c", script], cwd=cwd, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=600, env=env)
    dt = time.perf_counter() - t0
    return dt, p.returncode

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", required=True)
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--repeat", type=int, default=1,
                    help="run each workload N times per shell invocation")
    ap.add_argument("--only", default="")
    ap.add_argument("--shells", nargs="+", required=True,
                    help="name=CMD[+ARGS...][@PATHPREFIX] entries")
    ap.add_argument("--label", default="")
    args = ap.parse_args()

    shells = []
    for spec in args.shells:
        name, cmd = spec.split("=", 1)
        path_prefix = ""
        if "@" in cmd:
            cmd, path_prefix = cmd.rsplit("@", 1)
        env = None
        if path_prefix:
            env = dict(os.environ)
            env["PATH"] = path_prefix + os.pathsep + env.get("PATH", "")
        shells.append((name, cmd.split("+"), env))

    print("LABEL\tWORKLOAD\tSHELL\tMEDIAN_S\tRC")
    for wname, script in WORKLOADS:
        if args.only and args.only not in wname:
            continue
        for sname, argv, env in shells:
            times = []
            rc = 0
            for i in range(args.runs + 1):  # +1 warmup
                dt, rc = run_once(argv, script, args.data, env, args.repeat)
                if i > 0:
                    times.append(dt / args.repeat)
            med = statistics.median(times)
            print(f"{args.label}\t{wname}\t{sname}\t{med:.4f}\t{rc}")
            sys.stdout.flush()

if __name__ == "__main__":
    main()
