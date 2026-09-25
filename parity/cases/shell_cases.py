"""Shell parity cases (captured oracles from GNU coreutils 9.4 / bash 5.2).

Each entry is a shell script run under both unish and the oracle shell;
stdout and the exit code must match, and so must the resulting files in
the (fresh) working directory. An entry may be a (script, xfail_reason)
pair when the delta is known cosmetic.
"""

CASES = [

    'u=abcabc; echo "<${u/#abc/X}> <${u/%abc/Y}>"',
    'u=aXbXc; echo "<${u/X*/S}> <${u/*X/E}>"',
    'v=abc; echo "<${v/b/[&]}> <${v/b/\\&}> <${v//b/&}>"',
    't=AbC; echo "<${t~}>"; s=aBc; echo "<${s~}>"',
    'p=aaa; echo "<${p/a/X}> <${p//a/Y}> <${p/#a/Z}> <${p/%a/W}>"',
    'u=abcdef; echo "${u:2} ${u:1:3} ${u:0-2} ${u:0-3:2}"',
    'x=y; y=hello; echo ${!x}',
    'u=aXbXc; echo ${u#*X} ${u##*X} ${u%X*} ${u%%X*}',
    'u=abc; echo "${u^} ${u^^} ${u,} ${u,,}"',
    'echo "$((--5)) $((++5)) $((010)) $((0x1f))"',
    'x=08; echo "08: $((x))"; echo rc=$?',
    'echo "o10: $((0o10))"; echo rc=$?',
    'x=$((1/0)); echo "rc=$? val=[$x]"',
    'echo "mod: $((1%0))"; echo rc=$?',
    'x=5; echo "ok: $((x*2)) $((x++))"; echo "x=$x"',
    'echo "$((7/2)) $((-7/2)) $((7%3)) $((-7%3))"',
    'echo "$((1<<4)) $((256>>4)) $((-1>>1))"',
    'echo "$((12&10)) $((12|10)) $((12^10)) $((~5))"',
    'echo "$((1?2:3)) $((0?2:3)) $((2+3*4))"',
    'echo "$((9223372036854775807))" ; echo "$((9223372036854775807+1))"',
    'mkdir -p d/e; touch d/e/f.txt g1 g2; echo d/*/f.*; echo */; echo ./*/',
    'touch a1 a2 az; echo a[12]; echo a[!12]; echo a?',
    'touch .hidden vis; echo *',
    'mkdir sub; touch sub/x.txt; echo */*.txt',
    'exec 3> f; echo hi >&3; echo more 1>&3; exec 3>&-; echo after; cat f',
    'exec 3> f; (echo sub >&3); echo done >> f; cat f',
    'exec 3> f; exec 4>&3; echo dup >&4; cat f',
    'true | false; echo "${PIPESTATUS[0]}-${PIPESTATUS[1]}"',
    'false; echo "${PIPESTATUS[0]}"',
    'echo x | cat >/dev/null; echo "${PIPESTATUS[@]}"',
    "trap 'echo caught-exit' EXIT; echo mid",
    "trap 'echo int' INT TERM; echo ok; trap - INT; echo reset",
    "trap 'echo err' ERR; false; echo after",
    "printf 'abcd\\n' | { read -n 2 v; echo [$v]; }",
    "printf 'a:b\\n' | { IFS=: read x y; echo [$x][$y]; }",
    "printf 'ab:rest\\n' | { read -d: v; echo [$v]; }",
    "printf 'hello world\\n' | { read -r a b; echo [$a][$b]; }",
    "printf 'x y\\n' | { read; echo [$REPLY]; }",
    '[ -e /dev/null ] && echo yes || echo no',
    '[ -c /dev/null ] && echo chr || echo nochr',
    '[ 5 -gt 3 ] && echo gt; [ abc = abc ] && echo eq',
    'for i in 1 2 3; do echo $i; done',
    'x=$(false); echo "rc=$? x=[$x]"',
    'f() { echo fn-$1; }; f a',
    'case abc in a*) echo match;; *) echo no;; esac',
    'echo "${#HOME}" | grep -c "^[1-9]"',
]
