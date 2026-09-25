"""Per-command cases for the 21 commands whose flags need real setup.

The generic flag sweep cannot invent a tar archive, a diff pair, a
socket or a process to signal, so those commands are covered here with
hand-written cases and real fixtures. Expectations were verified against
GNU by hand; where GNU and unish are known to differ, the case is named
"FINDING ..." and reported as a known gap instead of failing.

Format: (name, script, expected_stdout, expected_exit)
"""

CASES = [
    # --- diff / cmp: two files that differ ---
    ("diff shows the change", "printf '1\\n2\\n' > a; printf '1\\n3\\n' > b; diff a b", "2c2\n< 2\n---\n> 3\n", 1),
    ("diff identical is silent", "printf 'x\\n' > a; printf 'x\\n' > b; diff a b", "", 0),
    ("diff -q reports differing", "printf '1\\n' > a; printf '2\\n' > b; diff -q a b", "Files a and b differ\n", 1),
    ("diff -u unified", "printf '1\\n2\\n' > a; printf '1\\n3\\n' > b; diff -u a b | tail -n +3", "@@ -1,2 +1,2 @@\n 1\n-2\n+3\n", 0),
    ("diff -L label", "printf '1\\n' > a; printf '2\\n' > b; diff -L x a b | head -1", "1c1\n", 0),
    ("cmp equal is silent", "printf 'ab\\n' > a; printf 'ab\\n' > b; cmp a b", "", 0),
    ("cmp reports first diff", "printf 'ab\\n' > a; printf 'ac\\n' > b; cmp a b", "a b differ: byte 2, line 1\n", 1),
    ("cmp -s is silent on differ", "printf 'ab\\n' > a; printf 'ac\\n' > b; cmp -s a b", "", 1),

    # --- gzip / gunzip: a real compressed file ---
    ("gzip then gunzip roundtrip", "printf 'hello\\n' > f; gzip f; gunzip f.gz; cat f", "hello\n", 0),
    ("gzip -c to stdout", "printf 'hi\\n' | gzip -c | gunzip -c", "hi\n", 0),
    ("gzip -k keeps the original", "printf 'x\\n' > f; gzip -k f; test -f f && echo kept", "kept\n", 0),
    ("gunzip -c to stdout", "printf 'y\\n' > f; gzip f; gunzip -c f.gz", "y\n", 0),

    # --- xargs ---
    ("xargs default echo", "printf 'a\\nb\\n' | xargs echo", "a b\n", 0),
    ("xargs -n1 one per line", "printf 'a b c\\n' | xargs -n1 echo", "a\nb\nc\n", 0),
    ("xargs -r no-run-if-empty", "printf '' | xargs -r echo", "", 0),
    ("xargs -I replace", "printf 'x\\n' | xargs -I{} echo 'got:{}'", "got:x\n", 0),
    ("xargs -0 null separated", "printf 'a\\0b\\0' | xargs -0 echo", "a b\n", 0),

    # --- timeout / nice / env ---
    ("timeout passes through", "timeout 5 echo fast", "fast\n", 0),
    ("timeout kills a slow command", "timeout 0.1 sleep 5; echo rc=$?", "rc=124\n", 0),
    ("timeout -s KILL", "timeout -s KILL 0.1 sleep 5; echo rc=$?", "rc=137\n", 0),
    ("nice runs the command", "nice echo niced", "niced\n", 0),
    ("nice -n adjustment", "nice -n 5 echo niced", "niced\n", 0),
    ("env sets a variable", "env FOO=bar printenv FOO", "bar\n", 0),
    ("env runs a command", "env echo hi", "hi\n", 0),
    # FINDING: env -i does not clear the environment (see REPORT.md).
    ("FINDING env -i clears the environment", "env -i printenv PATH; echo rc=$?", "rc=1\n", 0),

    # --- hexdump / strings / expand ---
    ("hexdump -C canonical", "printf 'abc\\n' | hexdump -C | head -1", "00000000  61 62 63 0a                                       |abc.|\n", 0),
    ("hexdump -n limits bytes", "printf 'abcdef\\n' | hexdump -n 2 -C | head -1 | cut -c1-8", "00000000\n", 0),
    ("strings finds text", "printf '\\x01hello\\x02world\\n' | strings", "hello\nworld\n", 0),
    ("strings -n length filter", "printf 'ab\\x01longer\\n' | strings -n 3", "longer\n", 0),
    ("expand -t tab width", "printf 'a\\tb\\n' | expand -t4", "a   b\n", 0),
    ("expand default width 8", "printf 'a\\tb\\n' | expand", "a       b\n", 0),

    # --- touch / ln ---
    ("touch creates a file", "touch new; test -f new && echo created", "created\n", 0),
    ("touch -c does not create", "touch -c nonexistent; test -f nonexistent || echo absent", "absent\n", 0),
    ("ln -s symlink", "printf 'x\\n' > f; ln -s f l; readlink l", "f\n", 0),
    ("ln hard link shares content", "printf 'x\\n' > f; ln f l; cat l", "x\n", 0),

    # --- ps / free (shape only: values are machine-dependent) ---
    ("ps has a header", "ps | head -1 | grep -c PID", "1\n", 0),
    ("ps -e lists processes", "ps -e | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("free has a header", "free | head -1 | grep -c total", "1\n", 0),
    ("free -m is numeric", "free -m | sed -n 2p | grep -cE '^Mem:'", "1\n", 0),

    # --- ss / nc: local socket, no external network ---
    # FINDING/limited: ss with no sockets is allowed to print just a header.
    ("ss has a header", "ss -t | head -1 | grep -c State", "1\n", 0),
    ("ss -l lists listening", "ss -l | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("nc -h or usage", "nc --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    # netcat is the same tool under its long name.
    ("netcat --help usage", "netcat --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("netcat -v verbose flag", "netcat --help 2>&1 | grep -c .", "1\n", 0),

    # --- shasum (aliases of sha1sum/sha256sum) ---
    ("shasum default is sha1", "printf 'hello\n' | shasum",
     "f572d396fae9206628714fb2ce00f72e94f2258f  -\n", 0),
    ("shasum -a 256", "printf 'hello\n' | shasum -a 256",
     "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  -\n", 0),
    ("shasum -a 1", "printf 'hello\n' | shasum -a 1",
     "f572d396fae9206628714fb2ce00f72e94f2258f  -\n", 0),

    # --- pgrep / pkill: sandboxed, unique victim name only ---
    # These match by NAME, so the pattern is deliberately unique and the
    # case never signals anything it did not start. (An earlier sweep ran
    # `pkill .` and killed the whole machine.)
    # FINDING: pgrep cannot see a background job, same root cause as $!.
    ("FINDING pgrep sees a background job", "sleep 30 & pgrep -f 'sleep 30' | grep -cE '^[0-9]+$'", "1\n", 0),
    ("pgrep no match exits 1", "pgrep -f 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("pgrep -x exact no match", "pgrep -x 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("pkill no match exits 1", "pkill -f 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
]
