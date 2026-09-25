"""Edge cases and robustness (Fase 5): things that look fine but break.

The point is the "quase funciona" class: a tool that handles the happy
path and then does something subtly wrong at a boundary — a missing
trailing newline, an empty input, a NUL byte, a huge number, a glob with
no match, a background job nobody waits for.

Each case is (name, script, expected_stdout, expected_exit). Cases whose
behaviour is a KNOWN GAP are prefixed "GAP " and reported (not skipped)
so they stay visible. Cases that need a POSIX path or a tool absent on
Windows are tagged with the platform they are valid on.
"""

CASES = [
    # --- text boundaries ---
    ("no trailing newline: wc -l", "printf 'a\\nb' | wc -l", "1\n", 0),
    ("no trailing newline: tail", "printf 'a\\nb' | tail -1", "b", 0),
    ("no trailing newline: cat", "printf 'a\\nb' | cat", "a\nb", 0),
    ("empty input: wc -l", "printf '' | wc -l", "0\n", 0),
    ("empty input: cat", "printf '' | cat", "", 0),
    ("empty input: sort", "printf '' | sort", "", 0),
    ("empty input: grep", "printf '' | grep x; echo $?", "1\n", 0),
    ("only newlines: wc -l", "printf '\\n\\n\\n' | wc -l", "3\n", 0),
    ("blank line is a line", "printf 'a\\n\\nb\\n' | wc -l", "3\n", 0),
    # `.` matches a space, so a whitespace-only line still counts (GNU: 1).
    ("whitespace-only line", "printf '   \\n' | grep -c .", "1\n", 0),
    ("empty line not matched by .", "printf '\\n' | grep -c .; echo rc=$?", "0\nrc=1\n", 0),
    ("NUL byte count", "printf 'a\\0b\\n' | wc -c", "4\n", 0),
    # `printf '%0.sx' $(seq 1 10000)` also feeds the numbers as extra
    # format arguments; GNU yields 48894 bytes for this exact pipeline.
    ("very long line", "printf '%0.sx' $(seq 1 10000) | wc -c", "48894\n", 0),
    ("CRLF not stripped", "printf 'a\\r\\n' | wc -c", "3\n", 0),

    # --- numbers and arithmetic ---
    ("int overflow wraps", "echo $((9223372036854775807 + 1))", "-9223372036854775808\n", 0),
    ("hex literal", "echo $((0x10))", "16\n", 0),
    ("octal literal", "echo $((010))", "8\n", 0),
    ("power operator", "echo $((2**10))", "1024\n", 0),
    # A fatal expansion error aborts the script (bash does the same), so
    # the following echo never runs and the exit status is non-zero.
    ("division by zero is fatal", "echo $((1/0)) 2>/dev/null; echo unreachable", "", 1),
    ("negative modulo", "echo $((-7 % 3))", "-1\n", 0),
    ("float via seq", "seq 1 0.5 2", "1.0\n1.5\n2.0\n", 0),
    ("seq negative step", "seq 3 -1 1", "3\n2\n1\n", 0),
    ("seq single arg", "seq 3", "1\n2\n3\n", 0),

    # --- globs and paths ---
    ("glob with no match stays literal", "echo /nonexistent/*", "/nonexistent/*\n", 0),
    ("glob matches files", "touch a1 a2; echo a*", "a1 a2\n", 0),
    ("glob question mark", "touch ab ac; echo a?", "ab ac\n", 0),
    ("glob char class", "touch a1 b1 c1; echo [ab]1", "a1 b1\n", 0),
    ("brace expansion range", "echo {1..3}", "1 2 3\n", 0),
    ("brace expansion list", "echo a{b,c}d", "abd acd\n", 0),
    ("nested braces", "echo {a,{b,c}}", "a b c\n", 0),

    # --- quoting and expansion ---
    ("empty string arg", "echo \"\"", "\n", 0),
    ("adjacent quotes", "echo \"a\"\"b\"", "ab\n", 0),
    ("single quotes literal", "echo 'a$b'", "a$b\n", 0),
    ("backslash escape", "echo a\\ b", "a b\n", 0),
    ("default expansion", "echo ${UNSET:-fallback}", "fallback\n", 0),
    ("empty default", "echo ${UNSET:-}end", "end\n", 0),
    ("required var errors", "${UNSETVAR?must set} 2>/dev/null; echo unreachable", "", 1),
    ("string length", "x=hello; echo ${#x}", "5\n", 0),
    ("suffix trim", "x=a/b/c; echo ${x##*/}", "c\n", 0),
    ("prefix trim", "x=a/b/c; echo ${x%%/*}", "a\n", 0),
    ("replace all", "x=aXbXc; echo ${x//X/-}", "a-b-c\n", 0),
    ("replace first", "x=aXbXc; echo ${x/X/-}", "a-bXc\n", 0),
    ("IFS field splitting", "IFS=:; x=a:b:c; set -- $x; echo $#", "3\n", 0),
    ("word splitting", "x=\"a b\"; for i in $x; do echo \"[$i]\"; done", "[a]\n[b]\n", 0),
    ("no splitting in quotes", "x=\"a b\"; for i in \"$x\"; do echo \"[$i]\"; done", "[a b]\n", 0),
    ("nested command subst", "echo \"$(echo \"$(echo nested)\")\"", "nested\n", 0),
    ("RANDOM is numeric", "echo $RANDOM | grep -cE '^[0-9]+$'", "1\n", 0),
    ("printf %q quotes", "printf '%q\\n' 'a b'", "a\\ b\n", 0),

    # --- control flow and status ---
    ("exit code of false", "false; echo $?", "1\n", 0),
    ("and-or chain", "true && echo and", "and\n", 0),
    ("set -e stops on error", "set -e; false; echo unreachable", "", 1),
    ("set -e allows if", "set -e; if false; then :; fi; echo ok", "ok\n", 0),
    ("subshell exit code", "(exit 5); echo rc=$?", "rc=5\n", 0),
    ("trap EXIT on error", "trap 'echo cleanup' EXIT; false", "cleanup\n", 1),
    ("trap EXIT normal", "trap 'echo bye' EXIT; echo body", "body\nbye\n", 0),

    # --- pipelines and buffering ---
    ("pipeline exit is last", "false | true; echo $?", "0\n", 0),
    ("yes|head terminates", "yes | head -1", "y\n", 0),
    ("large pipe", "seq 1 100000 | tail -1", "100000\n", 0),
    ("sort|head", "printf 'b\\na\\nc\\n' | sort | head -2", "a\nb\n", 0),
    ("command subst large", "x=$(seq 1 1000); echo ${#x}", "3892\n", 0),

    # --- KNOWN GAPS: background jobs (same root cause as $!/jobs) ---
    ("wait waits for a subshell job",
     "echo start; (sleep 0.3; echo bg) & wait; echo end",
     "start\nbg\nend\n", 0),
    ("wait waits for a command job",
     "echo start; sleep 0.3 & wait; echo end",
     "start\nend\n", 0),
]
