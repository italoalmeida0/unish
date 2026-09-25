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
    # env -i clears the environment (fixed).
    ("env -i clears the environment", "env -i printenv PATH; echo rc=$?", "rc=1\n", 0),

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
    # FINDING: BSD ps (macOS) has a different header, so 'PID' is not
    # guaranteed; the universal part is that ps prints a table.
    ("FINDING ps has a header", "ps | head -1 | grep -c PID", "1\n", 0),
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
    ("pgrep sees a background job", "sleep 30 & sleep 0.3; pgrep -f 'sleep 30' | grep -cE '^[0-9]+$'", "1\\n", 0),
    # FINDING: where the process list cannot be read (macOS), pgrep/pkill
    # exit 2; GNU exits 1 for "no match". Real platform gap.
    ("FINDING pgrep no match exits 1", "pgrep -f 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("FINDING pgrep -x exact no match", "pgrep -x 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("FINDING pkill no match exits 1", "pkill -f 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),

    # --- pgrep/pkill flags, still sandboxed (unique pattern, no victims) ---
    ("FINDING pgrep -c counts zero", "pgrep -c -f 'unish-no-such-proc-xyz'; echo rc=$?", "0\nrc=1\n", 0),
    ("pgrep -l lists nothing", "pgrep -l -f 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("pgrep -x exact form", "pgrep -x 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("pgrep -i case-insensitive", "pgrep -i -f 'UNISH-NO-SUCH-PROC-XYZ'; echo rc=$?", "rc=1\n", 0),
    ("FINDING pgrep --count long form", "pgrep --count -f 'unish-no-such-proc-xyz'; echo rc=$?", "0\nrc=1\n", 0),
    ("pkill -e echo no match", "pkill -e -f 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("pkill -x exact no match", "pkill -x 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),
    ("pkill -i case-insensitive", "pkill -i -f 'UNISH-NO-SUCH-PROC-XYZ'; echo rc=$?", "rc=1\n", 0),
    # FINDING: pkill -s (session) is not implemented.
    ("FINDING pkill -s session", "pkill -s 0 -f 'unish-no-such-proc-xyz'; echo rc=$?", "rc=1\n", 0),

    # --- ss flags: shape only, no live sockets assumed ---
    ("ss -t tcp table", "ss -t | head -1 | grep -c State", "1\n", 0),
    ("ss -a all table", "ss -a | head -1 | grep -c Netid", "1\n", 0),
    ("ss -l listening", "ss -l | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("ss -n numeric", "ss -n | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("ss -u udp", "ss -u | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("ss --tcp long form", "ss --tcp | head -1 | grep -c State", "1\n", 0),
    ("ss --all long form", "ss --all | head -1 | grep -c Netid", "1\n", 0),
    ("ss --listening long form", "ss --listening | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("ss --numeric long form", "ss --numeric | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("ss --udp long form", "ss --udp | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),

    # --- nc/netcat flags: usage/shape only, no outbound connection ---
    ("nc --udp usage", "nc --udp --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("nc -v verbose usage", "nc -v --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("nc -z zero usage", "nc -z --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("nc -w timeout usage", "nc -w 1 --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("netcat --udp usage", "netcat --udp --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("netcat -z zero usage", "netcat -z --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("netcat -w timeout usage", "netcat -w 1 --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("netcat -p source port usage", "netcat -p 1 --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("netcat -u udp usage", "netcat -u --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("nc -p source port usage", "nc -p 1 --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),
    ("nc -u udp usage", "nc -u --help 2>&1 | wc -l | grep -cE '^[0-9]+$'", "1\n", 0),

    # --- strings ---
    ("strings finds text", "printf '\x01hello\x02world\n' | strings", "hello\nworld\n", 0),
    ("strings -n length filter", "printf 'ab\x01longer\n' | strings -n 3", "longer\n", 0),
    # FINDING: strings -a is not implemented (GNU: scan whole file).
    ("strings -a scans whole file", "printf '\\x01hello\\n' | strings -a", "hello\n", 0),
    # FINDING: strings -t/-o (offset prefix) produce nothing.
    ("strings -t offset prefix", "printf 'hello\\n' | strings -t x", "      0 hello\n", 0),
    ("strings -o offset prefix", "printf 'hello\\n' | strings -o", "      0 hello\n", 0),
    ("strings --radix offset prefix", "printf 'hello\\n' | strings --radix x", "      0 hello\n", 0),

    # --- hexdump ---
    ("hexdump -C canonical", "printf 'abc\n' | hexdump -C | head -1 | cut -c1-10", "00000000  \n", 0),
    ("hexdump -v no squeeze", "printf 'abc\n' | hexdump -v | head -1 | cut -c1-7", "0000000\n", 0),
    # FINDING: hexdump -s on a pipe fails (Illegal seek); GNU handles it.
    # hexdump -s on a PIPE fails with "Illegal seek" in GNU too;
    # it only works on a seekable file, which is what we test.
    ("hexdump -s skip on a file", "printf 'abcdef\\n' > f; hexdump -s 2 f | head -1 | cut -c1-7", "0000002\n", 0),
    ("hexdump -e format string", "printf 'abc\n' | hexdump -e '16/1 \"%c\"' | head -c 3", "abc", 0),

    # --- free ---
    ("free has a header", "free | head -1 | grep -c total", "1\n", 0),
    ("free -m header", "free -m | head -1 | grep -c total", "1\n", 0),
    ("free -g header", "free -g | head -1 | grep -c total", "1\n", 0),
    ("free -h header", "free -h | head -1 | grep -c total", "1\n", 0),

    # --- checksum -q/-s (quiet/status) ---
    ("md5sum -q quiet", "printf 'x\\n' > f; md5sum f > sum; md5sum -c -q sum; echo rc=$?", "rc=0\n", 0),
    ("md5sum -s status", "printf 'x\\n' > f; md5sum f > sum; md5sum -c -s sum; echo rc=$?", "rc=0\n", 0),
    ("sha1sum -q quiet", "printf 'x\\n' > f; sha1sum f > sum; sha1sum -c -q sum; echo rc=$?", "rc=0\n", 0),
    ("sha1sum -s status", "printf 'x\\n' > f; sha1sum f > sum; sha1sum -c -s sum; echo rc=$?", "rc=0\n", 0),
    ("sha256sum -q quiet", "printf 'x\\n' > f; sha256sum f > sum; sha256sum -c -q sum; echo rc=$?", "rc=0\n", 0),
    ("sha256sum -s status", "printf 'x\\n' > f; sha256sum f > sum; sha256sum -c -s sum; echo rc=$?", "rc=0\n", 0),

    # --- shasum ---
    ("shasum default sha1", "printf 'x\n' | shasum | cut -d' ' -f1 | tr -d '\n'", "6fcf9dfbd479ed82697fee719b9f8c610a11ff2a", 0),
    ("shasum -a 256", "printf 'x\n' | shasum -a 256 | cut -d' ' -f1 | tr -d '\\n'", "73cb3858a687a8494ca3323053016282f3dad39d42cf62ca4e79dda2aac7d9ac", 0),
    ("shasum -b binary", "printf 'x\n' > f; shasum -b f | cut -d' ' -f1 | tr -d '\\n'", "6fcf9dfbd479ed82697fee719b9f8c610a11ff2a", 0),
    ("shasum -t text", "printf 'x\n' > f; shasum -t f | cut -d' ' -f1 | tr -d '\\n'", "6fcf9dfbd479ed82697fee719b9f8c610a11ff2a", 0),
    ("shasum --algorithm 256", "printf 'x\n' | shasum --algorithm 256 | cut -d' ' -f1 | tr -d '\\n'", "73cb3858a687a8494ca3323053016282f3dad39d42cf62ca4e79dda2aac7d9ac", 0),
    ("shasum --binary", "printf 'x\n' > f; shasum --binary f | cut -d' ' -f1 | tr -d '\\n'", "6fcf9dfbd479ed82697fee719b9f8c610a11ff2a", 0),
    ("shasum --text", "printf 'x\n' > f; shasum --text f | cut -d' ' -f1 | tr -d '\\n'", "6fcf9dfbd479ed82697fee719b9f8c610a11ff2a", 0),

    # --- od ---
    ("od -N limits bytes", "printf 'abcdef\n' | od -N 2 -c | head -1", "0000000   a   b\n", 0),
    # od -u is not a GNU flag; the phantom was removed from the table.
    ("od -t u2 unsigned decimal", "printf 'A\\n' | od -t u2 | head -1", "0000000  2625\n", 0),

    # --- uniq -g (grouped, GNU 9.0+) ---
    ("uniq -g group", "printf 'a\na\nb\n' | uniq -g", "a\na\n\nb\n", 0),

    # --- split -v, timeout -p, cat -S ---
    ("split -v verbose creates files", "printf 'a\nb\nc\nd\n' > f; split -v -l 2 f 2>/dev/null; ls xa* | wc -l", "2\n", 0),
    ("timeout -p preserve status", "timeout -p 5 true; echo rc=$?", "rc=0\n", 0),
    # FINDING: cat -S (squeeze blank lines) is not implemented.
    # cat -S is not a GNU flag either; -s (squeeze-blank) is.
    ("cat -s squeezes blanks", "printf 'a\\n\\n\\nb\\n' | cat -s", "a\n\nb\n", 0),

    # --- tail -f / --follow: needs a live file; documented, not run ---
    # (a blocking test is not a test; covered by the sandbox for ss/nc)
]
