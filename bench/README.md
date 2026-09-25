# Benchmark harness

Cross-shell performance benchmarks for unish: the same POSIX workloads
run under unish and bash (or Git Bash / MSYS2 on Windows), timed with a
warmup and medians over repeated runs.

## Usage

Generate the data set once (about 30 MB: 200k-line files, a 16 MiB blob
and 2000 small files):

    python3 gen_data.py data

Measure (here: unish vs bash, 5 runs each, every workload repeated 3x
per shell invocation to amortize spawn overhead):

    python3 bench_core.py --data data --runs 5 --repeat 3 \
        --shells unish=./unish bash=/bin/bash

On Windows, give each shell the PATH prefix it needs to find its own
coreutils (`name=CMD@PATHPREFIX`), e.g.:

    python bench_core.py --data data --runs 5 --repeat 3 ^
        --shells unish=.\unish.exe ^
        "gitbash=C:\Program Files\Git\bin\bash.exe@C:\Program Files\Git\usr\bin"

The output is a TSV: label, workload, shell, median seconds, exit code.

## Notes

- Sub-20ms workloads are dominated by process spawn and scheduler noise
  in virtualized environments (WSL2 quantizes heavily); prefer `--repeat`
  and compare workloads above ~30ms, or run several times and take the
  minimum.
- The workloads exercise both the shell (loops, expansions, command
  substitution) and the embedded utilities (sort/grep/sed/wc/tar...),
  where bash uses external GNU tools and unish uses its built-ins: that
  is the architectural trade the benchmark is meant to expose.
