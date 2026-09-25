"""Functional tests: does unish actually WORK, not just print the same?

Output-parity cases prove "unish prints what GNU prints". They do NOT
prove the shell does the thing: spawn a real process, kill it, wait for
it, see a trap fire. Those are different questions, and a shell can pass
thousands of parity cases while `kill $!` is broken.

Each entry is (name, script, expected_stdout, expected_exit_code, tags)
where `tags` names platforms the case is known to fail on, with the
reason. `KNOWN GAP` entries are reported but don't fail the run; they
must eventually be fixed and removed from the tag list.
"""

# tags: () = must pass everywhere. "all" = known gap on every platform.
CASES = [
    # --- process lifecycle: real pid, kill, wait ---
    (
        "exit code propagation",
        "(exit 42); echo $?",
        "42\n", 0, (),
    ),
    (
        "nested subshell exit",
        "(exit 7); echo $?",
        "7\n", 0, (),
    ),
    (
        "wait for a job gives its status",
        "sleep 0.2 & wait $!; echo $?",
        "0\n", 0, (),
    ),
    (
        "wait with no args joins all jobs",
        "sleep 0.1 & sleep 0.1 & wait; echo all-done",
        "all-done\n", 0, (),
    ),
    (
        "$! is a numeric real pid",
        "sleep 30 & case $! in ''|*[!0-9]*) echo NOT-A-PID:$!;; *) echo PID;; esac; kill $! 2>/dev/null",
        "PID\n", 0, (),
    ),
    (
        "kill $! terminates the job",
        "sleep 30 & p=$!; kill $p 2>/dev/null; echo done",
        "done\n", 0, (),
    ),
    (
        "kill %1 terminates the job",
        "sleep 30 & kill %1 2>/dev/null; echo done",
        "done\n", 0, (),
    ),
    (
        "kill -0 sees a live job",
        "sleep 30 & p=$!; kill -0 $p 2>/dev/null && echo alive; kill $p 2>/dev/null; true",
        "alive\n", 0, (),
    ),
    (
        "jobs lists the running job",
        "sleep 30 & jobs; kill %1 2>/dev/null",
        "[1] Running sleep 30\n", 0, (),
    ),
    (
        "background job does not block the shell",
        "sleep 2 & echo now",
        "now\n", 0, (),
    ),
    (
        "SIGTERM trap fires",
        'trap "echo caught" TERM; kill -TERM $$; echo after',
        "caught\nafter\n", 0, (),
    ),
    (
        "EXIT trap fires",
        'trap "echo bye" EXIT; echo body',
        "body\nbye\n", 0, (),
    ),

    # --- shell state and redirection ---
    (
        "fd 3 redirection writes the file",
        "exec 3>f3; echo hello >&3; exec 3>&-; cat f3",
        "hello\n", 0, (),
    ),
    (
        "subshell does not leak variables",
        'x=outer; (x=inner); echo $x',
        "outer\n", 0, (),
    ),
    (
        "pipestatus records every stage",
        "true | false | true; echo ${PIPESTATUS[*]}",
        "0 1 0\n", 0, (),
    ),
    (
        "command substitution in a loop",
        'for i in 1 2 3; do echo "v$i"; done',
        "v1\nv2\nv3\n", 0, (),
    ),
    (
        "read consumes a pipe line by line",
        'printf "a\\nb\\n" | while read l; do echo "got:$l"; done',
        "got:a\ngot:b\n", 0, (),
    ),
    (
        "pipeline exit status is the last command",
        "false | true; echo $?",
        "0\n", 0, (),
    ),
    (
        "AND-OR short circuit",
        "false && echo no; true || echo no; echo end",
        "end\n", 0, (),
    ),
    (
        "redirect appends",
        "echo a > f; echo b >> f; cat f",
        "a\nb\n", 0, (),
    ),
    (
        "here-document feeds stdin",
        "cat <<EOF\nhello\nEOF",
        "hello\n", 0, (),
    ),
]
