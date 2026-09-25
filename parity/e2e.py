"""End-to-end tests: the shipped binary as a real process (Fase 4).

Everything else in this suite runs the interpreter in-process or through
`-c`. These run `unish` the way a user, a Makefile or another program
does: as a child process with real pipes, real files, real exit codes and
real signals. That is the only way to catch things like a broken
`unish script.sh`, a pipeline that loses an exit status, or stdin that is
not actually connected.

Cases are (name, kind, payload, expected) where kind is one of:
  script   - write payload to a file, run `unish file [args]`
  stdin    - pipe payload into `unish` (script on stdin)
  pipeline - payload is a shell pipeline string; run through `unish -c`
  process  - payload is a list of argv for two processes to pipe together

Expected is (stdout, exit_code). Anything machine-dependent is compared
against the oracle instead (see ORACLE_CASES below).
"""

CASES = [
    # --- running a script file: the most basic shell use ---
    ("script file runs", "script", "echo A\necho B\n", ("A\nB\n", 0)),
    ("script sees $1 $#", "script", 'echo "$1/$#"\n', ("x/2\n", 0)),
    ("script exit code", "script", "exit 7\n", ("", 7)),
    ("script with shebang", "script", "#!/usr/bin/env unish\necho ok\n", ("ok\n", 0)),
    ("script error exit", "script", "nosuchcmd123\n", ("", 127)),
    ("script reads stdin", "script", "cat\necho done\n", ("line\ndone\n", 0)),

    # --- script on stdin ---
    ("stdin script", "stdin", "echo from-stdin\n", ("from-stdin\n", 0)),
    ("stdin script exit", "stdin", "exit 9\n", ("", 9)),

    # --- pipelines between two real unish processes ---
    ("unish|unish", "process", [["seq", "5"], ["wc", "-l"]], ("5\n", 0)),
    ("unish|unish|unish", "process",
     [["seq", "10"], ["grep", "1"], ["wc", "-l"]], ("2\n", 0)),
    ("pipe carries data", "process",
     [["printf 'a\\nb\\nc\\n'"], ["sort", "-r"]], ("c\nb\na\n", 0)),
    ("pipe to external", "process", [["seq", "3"], ["cat"]], ("1\n2\n3\n", 0)),

    # --- redirections and exit codes at process level ---
    ("redirect out", "pipeline", "echo hi > out.txt; cat out.txt", ("hi\n", 0)),
    ("redirect append", "pipeline", "echo a > f; echo b >> f; cat f", ("a\nb\n", 0)),
    ("redirect in", "pipeline", "printf 'x\\n' > f; cat < f", ("x\n", 0)),
    ("pipeline exit is last", "pipeline", "false | true; echo $?", ("0\n", 0)),
    ("pipeline exit false", "pipeline", "true | false; echo $?", ("1\n", 0)),
    ("subshell exit", "pipeline", "(exit 4); echo $?", ("4\n", 0)),
    ("and-or exit", "pipeline", "false && echo no || echo yes", ("yes\n", 0)),
    ("stderr to file", "pipeline", "nosuchcmd123 2>err.txt; test -s err.txt && echo had-error", ("had-error\n", 0)),
    ("stdout and stderr split", "pipeline",
     "(echo out; echo err >&2) 2>e.txt", ("out\n", 0)),
    ("fd 3 roundtrip", "pipeline",
     "exec 3>f3; echo via3 >&3; exec 3>&-; cat f3", ("via3\n", 0)),
    ("here-doc", "pipeline", "cat <<EOF\nhello\nEOF", ("hello\n", 0)),
    ("command substitution", "pipeline", "x=$(echo sub); echo $x", ("sub\n", 0)),
    ("arithmetic", "pipeline", "echo $((2+3*4))", ("14\n", 0)),
    ("loop over words", "pipeline", "for w in a b c; do echo $w; done", ("a\nb\nc\n", 0)),
    ("while read", "pipeline",
     "printf 'p\\nq\\n' | while read l; do echo \"[$l]\"; done", ("[p]\n[q]\n", 0)),
    ("case statement", "pipeline",
     "for x in foo bar; do case $x in foo) echo F;; *) echo O;; esac; done", ("F\nO\n", 0)),
    ("function call", "pipeline", "f() { echo \"fn:$1\"; }; f hi", ("fn:hi\n", 0)),
    ("nested subshell", "pipeline", "( ( echo deep ) )", ("deep\n", 0)),
    ("glob expansion", "pipeline", "touch a1 a2; echo a*", ("a1 a2\n", 0)),
    ("var default", "pipeline", "echo ${UNSET:-fallback}", ("fallback\n", 0)),
    ("string length", "pipeline", "x=hello; echo ${#x}", ("5\n", 0)),
    ("trim suffix", "pipeline", "x=a/b/c; echo ${x##*/}", ("c\n", 0)),
    ("replace all", "pipeline", "x=aXbXc; echo ${x//X/-}", ("a-b-c\n", 0)),

    # --- large data through a real pipe (buffering/deadlock) ---
    ("big pipe no deadlock", "process",
     [["seq", "20000"], ["wc", "-l"]], ("20000\n", 0)),
    ("head closes early", "process",
     [["seq", "100000"], ["head", "-n", "3"]], ("1\n2\n3\n", 0)),
    ("yes|head terminates", "process",
     [["yes"], ["head", "-n", "2"]], ("y\ny\n", 0)),
]
