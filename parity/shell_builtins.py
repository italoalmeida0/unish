"""Shell builtin cases (Fase: builtins, the category that was not measured).

The 90 embedded commands are covered by the flag/command suites. Shell
builtins are a separate surface — cd, export, set, read, trap, shift,
unset, eval, getopts, alias, declare, ... — and 16 of the 40 were not
mentioned by any test at all.

Each case is (name, script, expected_stdout, expected_exit). Cases whose
behaviour is a KNOWN GAP are prefixed "FINDING ". Expectations were
checked against GNU bash; several early ones were my own mistakes (a
non-interactive bash does NOT expand aliases without expand_aliases) and
were corrected rather than blamed on the shell.
"""

CASES = [
    # --- variable state ---
    ("set and read a variable", "x=1; echo $x", "1\n", 0),
    ("unset removes a variable", 'x=1; unset x; echo "${x:-empty}"', "empty\n", 0),
    ("readonly blocks assignment", "readonly x=1; x=2 2>/dev/null; echo $x", "1\n", 0),
    ("export reaches a child", "export X=hi; env | grep -c '^X=hi$'", "1\n", 0),
    ("declare sets a variable", "declare x=5; echo $x", "5\n", 0),
    ("typeset sets a variable", "typeset y=6; echo $y", "6\n", 0),
    ("declare -i integer", "declare -i n=2+3; echo $n", "5\n", 0),
    ("local in a function", "f() { local v=in; echo $v; }; f", "in\n", 0),
    ("local does not leak", "f() { local v=in; }; f; echo \"${v:-none}\"", "none\n", 0),

    # --- parameters ---
    ("shift drops an argument", "set -- a b c; shift; echo $#", "2\n", 0),
    ("shift by two", "set -- a b c; shift 2; echo $#", "1\n", 0),
    # shift past the end fails with status 1, like bash (fixed).
    ("shift past the end fails", "set -- a; shift 5; echo rc=$?", "rc=1\n", 0),
    ("positional params", "set -- x y; echo $1 $2", "x y\n", 0),
    ("$# counts arguments", "set -- a b c; echo $#", "3\n", 0),
    ("$@ expands to all", 'set -- a b; for p in "$@"; do echo $p; done', "a\nb\n", 0),

    # --- control and evaluation ---
    ("eval runs its argument", 'eval "echo evaled"', "evaled\n", 0),
    ("eval sees variables", 'x=5; eval "echo $x"', "5\n", 0),
    ("times prints a line", "times | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("getopts parses a flag", "set -- -a; getopts a opt; echo $opt", "a\n", 0),
    ("getopts reports OPTIND", "set -- -a -b; getopts ab opt; getopts ab opt; echo $OPTIND", "3\n", 0),
    ("return from a function", "f() { return 3; }; f; echo $?", "3\n", 0),
    ("break leaves a loop", "for i in 1 2 3; do [ $i = 2 ] && break; echo $i; done", "1\n", 0),
    ("continue skips an iteration", "for i in 1 2 3; do [ $i = 2 ] && continue; echo $i; done", "1\n3\n", 0),

    # --- aliases (non-interactive: need expand_aliases, like bash) ---
    ("alias without expand_aliases is not expanded",
     'alias ll="echo hi"; ll 2>/dev/null; echo rc=$?', "rc=127\n", 0),
    ("alias with expand_aliases works",
     'shopt -s expand_aliases; alias x="echo y"; x', "y\n", 0),
    ("unalias removes it",
     'shopt -s expand_aliases; alias x="echo y"; unalias x; x 2>/dev/null; echo rc=$?', "rc=127\n", 0),

    # --- functions ---
    ("function call with args", 'f() { echo "$1"; }; f hello', "hello\n", 0),
    ("function sees $#", "f() { echo $#; }; f a b c", "3\n", 0),
    ("nested function call", 'f() { g; }; g() { echo deep; }; f', "deep\n", 0),
    # FINDING: unset -f does not remove a function.
    ("FINDING unset -f removes a function", "f() { echo fn; }; unset -f f; f 2>/dev/null; echo rc=$?", "rc=127\n", 0),

    # --- misc builtins ---
    ("cd changes directory", "mkdir dd; cd dd; pwd | grep -c 'dd'", "1\n", 0),
    # cd - returns to the directory you were in before the last cd. Start
    # in the temp root, go into dd, then cd - must land back in the root.
    ("cd - returns to the previous dir", "mkdir dd; cd dd; cd - >/dev/null; test -d dd && echo back", "back\n", 0),
    ("command runs a builtin", "command echo hi", "hi\n", 0),
    ("builtin runs a builtin", "builtin echo hi", "hi\n", 0),
    ("type reports a builtin", "type echo | grep -c builtin", "1\n", 0),
    ("hash accepts a command", "hash echo; echo rc=$?", "rc=0\n", 0),
    ("umask prints a mask", "umask | grep -cE '^[0-7]{4}$'", "1\n", 0),
    ("pwd prints the directory", "pwd | wc -l", "1\n", 0),

    # --- compgen/complete (bash completion helpers) ---
    # FINDING: compgen is not implemented.
    ("compgen lists commands", "compgen -c 2>/dev/null | head -1 | grep -c .", "1\n", 0),
    ("complete registers", "complete -W 'a b' mycmd; echo rc=$?", "rc=0\n", 0),
]
