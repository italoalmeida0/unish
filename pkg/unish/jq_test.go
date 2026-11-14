package unish

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	if lookupExtra("jq") == nil {
		extraCommands = append(extraCommands, extraCmd{"jq", cmdJq})
	}
}

func TestJqExact64BitIntAndNumbers(t *testing.T) {
	dir := t.TempDir()

	// Exact 64-bit GitHub run / Snowflake IDs that 53-bit float64 would corrupt
	out, stderr, err := runParityScript(t, dir, `
echo '{"run_id": 435423839299999999, "max_i64": 9223372036854775807, "min_i64": -9223372036854775808}' |
  jq -c '{
    same: .run_id,
    next: (.run_id + 1),
    prev: (.run_id - 1),
    eq_false: (.run_id == 435423839299999998),
    gt_true: (.run_id > 435423839299999998),
    max_i64: .max_i64,
    min_i64: .min_i64
  }'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := `{"same":435423839299999999,"next":435423839300000000,"prev":435423839299999998,"eq_false":false,"gt_true":true,"max_i64":9223372036854775807,"min_i64":-9223372036854775808}` + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	// Ultra-large >64-bit uint64 pass-through + integer vs float division + float formatting + negative number filter
	out2, stderr2, err2 := runParityScript(t, dir, `
echo '{"u64": 18446744073709551615}' | jq -c '{u64: .u64, div_int: (10 / 2), div_flt: (5 / 2), big_flt: (1000000 + 0.5)}'
jq -nc -42
`)
	if err2 != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err2, stderr2)
	}
	want2 := `{"u64":18446744073709551615,"div_int":5,"div_flt":2.5,"big_flt":1000000.5}` + "\n-42\n"
	if out2 != want2 {
		t.Fatalf("got %q, want %q", out2, want2)
	}
}

func TestJqKeyOrderAndSortKeys(t *testing.T) {
	dir := t.TempDir()

	// Preserve insertion order by default, update in-place preserves key position, -S sorts keys, keywords as keys
	out, stderr, err := runParityScript(t, dir, `
echo '{"z": 1, "a": 2, "m": 3}' | jq -c '.'
echo '{"z": 1, "a": 2, "m": 3}' | jq -c '.a = 20 | .b = 4'
echo '{"z": 1, "a": 2, "m": 3}' | jq -Sc '.'
jq -nc '{z: 10, a: 20, k: 30} | [., keys_unsorted, keys, (. == {a: 20, k: 30, z: 10})]'
echo '{"if": "cond", "then": "ok", "end": 99, "foo-bar": "baz"}' | jq -c '{type: .if, in: .then, end: .end, fb: ."foo-bar", out: (.if | ascii_upcase)}'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`{"z":1,"a":2,"m":3}`,
		`{"z":1,"a":20,"m":3,"b":4}`,
		`{"a":2,"m":3,"z":1}`,
		`[{"z":10,"a":20,"k":30},["z","a","k"],["a","k","z"],true]`,
		`{"type":"cond","in":"ok","end":99,"fb":"baz","out":"COND"}`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestJqPathUpdatesAndConfigEditing(t *testing.T) {
	dir := t.TempDir()
	pkgJSON := `{
  "name": "demo-app",
  "version": "1.0.0",
  "scripts": {
    "build": "go build .",
    "test": "echo todo"
  },
  "dependencies": {
    "foo": "^1.0.0"
  },
  "devDependencies": {
    "eslint": "^9.0.0"
  }
}
`
	mustWrite(t, filepath.Join(dir, "package.json"), pkgJSON)

	out, stderr, err := runParityScript(t, dir, `
jq -c '.version = "1.1.0" | .scripts.test = "go test ./..." | del(.devDependencies) | .scripts.lint = "golangci-lint run"' package.json
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := `{"name":"demo-app","version":"1.1.0","scripts":{"build":"go build .","test":"go test ./...","lint":"golangci-lint run"},"dependencies":{"foo":"^1.0.0"}}` + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	// Compound updates (+=, -=, *=, /=, //=), |= with empty deletion, recursive secret redaction, auto-vivification, pick
	out2, stderr2, err2 := runParityScript(t, dir, `
echo '{"count": 10, "delta": 5, "x": null, "y": "keep"}' | jq -c '.count += .delta | .x //= "default" | .y //= "other"'
echo '[1, 2, 3, 4, 5, 6]' | jq -c '.[] |= select(. % 2 == 0)'
echo '{"user": {"name": "alice", "password": "secret"}, "items": [{"password": "p2"}, 42]}' | jq -c '(.. | objects | select(has("password")) | .password) |= "***"'
echo '{}' | jq -c '.a.b.c = 42'
echo '{"a": 1, "b": 2}' | jq -c '.a = .b'
echo '{"a": 1, "b": 2, "c": {"x": 10, "y": 20}}' | jq -c 'pick(.a, .c.y)'
`)
	if err2 != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err2, stderr2)
	}
	want2 := strings.Join([]string{
		`{"count":15,"delta":5,"x":"default","y":"keep"}`,
		`[2,4,6]`,
		`{"user":{"name":"alice","password":"***"},"items":[{"password":"***"},42]}`,
		`{"a":{"b":{"c":42}}}`,
		`{"a":2,"b":2}`,
		`{"a":1,"c":{"y":20}}`,
	}, "\n") + "\n"
	if out2 != want2 {
		t.Fatalf("got:\n%s\nwant:\n%s", out2, want2)
	}
}

func TestJqGeneratorsAndOperators(t *testing.T) {
	dir := t.TempDir()

	out, stderr, err := runParityScript(t, dir, `
jq -nc '(1, 2) + (3, 4)'
jq -nc '[ [1,2] + [3,4], [(1,2) + (3,4)] ]'
jq -nc '[ [1,2,2,3,4] - [2,4], "ab" * 3, "a,b,c" / "," ]'
jq -nc '{a: {x: 1, y: 2}, b: 1} * {a: {y: 20, z: 30}, c: 2}'
jq -nc '[ {a: (1, 2), ("k" + ("1", "2")): true} ]'
jq -nc '[ {}, [], "b", "a", 2, 1, true, false, null ] | sort'
jq -nc '[ (null // 42), (false // 42), (0 // 42), ("" // 42), (empty // 42), [(false, 10, null, 20) // 99] ]'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`4`,
		`5`,
		`5`,
		`6`,
		`[[1,2,3,4],[4,5,5,6]]`,
		`[[1,3],"ababab",["a","b","c"]]`,
		`{"a":{"x":1,"y":20,"z":30},"b":1,"c":2}`,
		`[{"a":1,"k1":true},{"a":1,"k2":true},{"a":2,"k1":true},{"a":2,"k2":true}]`,
		`[null,false,true,1,2,"a","b",[],{}]`,
		`[42,42,0,"",42,[10,20]]`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestJqControlBindingsDefsReduceForeach(t *testing.T) {
	dir := t.TempDir()

	out, stderr, err := runParityScript(t, dir, `
jq -nc '[1, 2, 3] | map(if . == 1 then "one" elif . == 2 then "two" else "other" end)'
jq -nc '[1, 2, 3] | map(if . == 2 then 20 end)'
jq -nc '[ (try (1 / 0) catch "div0"), (try error({code: 500}) catch .code), ([1, "x", 3][] | tonumber?) ]'
jq -nc '{id: 7, pair: [10, 20], name: "jq"} | . as {id: $id, pair: [$x, $y], $name} | "\($name)#\($id)=\($x + $y)"'
jq -nc 'reduce range(1; 6) as $x (0; . + $x)'
jq -nc '[ foreach range(1; 5) as $x (0; . + $x; . * 10) ]'
jq -nc 'def fact: if . <= 1 then 1 else . * ((. - 1) | fact) end; def apply(f; $k): (f) + $k; [ (5 | fact), (3 | apply(. * 10; 4)) ]'
jq -nc '[ label $out | foreach range(10) as $x (0; . + 1; if . > 3 then break $out else . end) ]'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`["one","two","other"]`,
		`[1,20,3]`,
		`["div0",500,1,3]`,
		`"jq#7=30"`,
		`15`,
		`[10,30,60,100]`,
		`[120,34]`,
		`[1,2,3]`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestJqFormatStringsAndInterpolation(t *testing.T) {
	dir := t.TempDir()

	out, stderr, err := runParityScript(t, dir, `
echo '["alice", "a,b", "q\"w", 42, null, true]' | jq -r '@csv'
echo '["a\tb", "c\nd", 10, null]' | jq -r '@tsv'
echo '"hello world"' | jq -r '@base64 | . + ":" + (@base64d)'
echo '{"q": "a b&c=1", "p": 2}' | jq -r '@uri "https://example.com/search?q=\(.q)&p=\(.p)"'
echo '["git", "commit", "-m", "fix '\''bug'\'' now"]' | jq -r '@sh'
echo '"<b class=\"x\">a & '\''b'\''</b>"' | jq -r '@html'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`"alice","a,b","q""w",42,,true`,
		`a\tb` + "\t" + `c\nd` + "\t10\t",
		`aGVsbG8gd29ybGQ=:hello world`,
		`https://example.com/search?q=a%20b%26c%3D1&p=2`,
		`'git' 'commit' '-m' 'fix '\''bug'\'' now'`,
		`&lt;b class=&quot;x&quot;&gt;a &amp; &apos;b&apos;&lt;/b&gt;`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestJqRegexBuiltinsAndUTF8Offsets(t *testing.T) {
	dir := t.TempDir()

	out, stderr, err := runParityScript(t, dir, `
jq -nc '"Error: 404 Not Found" | [ test("error"; "i"), test("^200") ]'
jq -nc '"éà=42" | match("(?<num>[0-9]+)") | {offset, length, string, cap: .captures[0].name}'
jq -nc '"2026-11-14" | capture("(?<y>[0-9]{4})-(?<m>[0-9]{2})-(?<d>[0-9]{2})")'
jq -nc '"v1 and v22" | gsub("v(?<n>[0-9]+)"; "ver[\(.n)]")'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`[true,false]`,
		`{"offset":3,"length":2,"string":"42","cap":"num"}`,
		`{"y":"2026","m":"11","d":"14"}`,
		`"ver[1] and ver[22]"`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestJqStdlibBuiltinsComprehensive(t *testing.T) {
	dir := t.TempDir()

	out, stderr, err := runParityScript(t, dir, `
jq -nc '[ (null | length), (-7 | length), ("héllo" | length), ("héllo" | utf8bytelength) ]'
jq -nc '{a: 1, b: 2} | with_entries(.key |= "k_" + . | .value *= 10)'
jq -nc '[ {dept: "eng", n: "b"}, {dept: "ops", n: "a"}, {dept: "eng", n: "a"} ] | [ sort_by(.dept, .n), (group_by(.dept) | map(length)), (unique_by(.dept) | length) ]'
jq -nc '[[1, [2, [3]]], [1, [2, [3]]]] | [ (.[0] | flatten), (.[1] | flatten(1)) ]'
jq -nc '[ ([1,2,3,1,2] | indices([1,2])), ("ababa" | indices("aba")), ("ababa" | index("ba")), ("ababa" | rindex("ba")) ]'
jq -nc '[ ("  hello \t\n" | [trim, ltrim, rtrim]), ("foobar" | [startswith("foo"), endswith("bar"), ltrimstr("foo"), rtrimstr("bar")]), (["a", null, 3, true] | join("-")) ]'
jq -nc '{a: {b: 1}, c: [2, "z"]} | [ [paths], [leaf_paths], getpath(["c", 0]), (setpath(["a", "x"]; 9) | .a.x), delpaths([["a", "b"], ["c", 1]]) ]'
jq -nc '[ (3.7 | [floor, ceil, round]), (16 | sqrt), (-9 | abs), first(range(10; 1000000)), [limit(3; range(5; 100))], nth(4; range(100; 200)) ]'
jq -nc '{"name": "root", "child": {"name": "mid", "child": {"name": "leaf"}}} | [ recurse(.child) | .name ]'
jq -nc '{a: [1, 2], b: {c: 3}} | walk(if type == "number" then . * 10 else . end)'
jq -nc '[ ([[1,2],[3,4]] | transpose), ([10,20,30] | bsearch(20)), ("Az" | explode | implode) ]'
jq -nc '[ [last(empty)], [last(1, null)], [nth(5; range(3))], ([{Name: "flag", Value: false}] | from_entries), (2 | IN(1, 2, 3)), ([{id: 1, v: "a"}, {id: 2, v: "b"}] | INDEX(.id) | ."2".v) ]'
jq -nc 'builtins | length > 50'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`[0,7,5,6]`,
		`{"k_a":10,"k_b":20}`,
		`[[{"dept":"eng","n":"a"},{"dept":"eng","n":"b"},{"dept":"ops","n":"a"}],[2,1],2]`,
		`[[1,2,3],[1,2,[3]]]`,
		`[[0,3],[0,2],1,3]`,
		`[["hello","hello \t\n","  hello"],[true,true,"bar","foo"],"a--3-true"]`,
		`[[["a"],["a","b"],["c"],["c",0],["c",1]],[["a","b"],["c",0],["c",1]],2,9,{"a":{},"c":[2]}]`,
		`[[3,4,4],4,9,10,[5,6,7],104]`,
		`["root","mid","leaf"]`,
		`{"a":[10,20],"b":{"c":30}}`,
		`[[[1,3],[2,4]],1,"Az"]`,
		`[[],[null],[],{"flag":false},true,"b"]`,
		`true`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestJqCLIFlagsStreamingAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "items.jsonl"), "{\"id\":1}{\"id\":2}\n{\"id\":3}\n")
	mustWrite(t, filepath.Join(dir, "extra.json"), "{\"tag\":\"v1\"}\n{\"tag\":\"v2\"}\n")
	mustWrite(t, filepath.Join(dir, "raw.txt"), "hello\nworld")
	mustWrite(t, filepath.Join(dir, "filter.jq"), "# comment in filter file\nmap(.id) | add\n")

	// Concatenated + JSONL stream, -s + -f, --arg, --argjson, --slurpfile, --rawfile, -R, -Rs, -j, --tab, --indent, inputs, --args, default '.' filter, $ENV
	out, stderr, err := runParityScript(t, dir, `
jq -c '.id' items.jsonl
jq -s -f filter.jq items.jsonl
jq -nc --arg env "prod" --argjson retries 3 --slurpfile tags extra.json --rawfile txt raw.txt '{$env, $retries, tags: ($tags | map(.tag)), $txt}'
printf 'line1\nline2\n' | jq -Rc 'ascii_upcase'
printf 'line1\nline2' | jq -Rsc 'split("\n")'
jq -nj '("a", "b", "c")'
echo ''
echo '{"x":1}' | jq --tab .
echo '{"x":1}' | jq --indent 4 .
echo '{"ok":true}' | jq -c
jq -nc 'reduce inputs.id as $i (0; . + $i)' items.jsonl
jq -nc '$ARGS' --arg k v --args pos1 pos2
MY_SECRET=xyz123 jq -nr '$ENV.MY_SECRET + ":" + env.MY_SECRET'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`1`,
		`2`,
		`3`,
		`6`,
		`{"env":"prod","retries":3,"tags":["v1","v2"],"txt":"hello\nworld"}`,
		`"LINE1"`,
		`"LINE2"`,
		`["line1","line2"]`,
		`abc`,
		"{",
		"\t\"x\": 1",
		"}",
		"{",
		"    \"x\": 1",
		"}",
		`{"ok":true}`,
		`6`,
		`{"positional":["pos1","pos2"],"named":{"k":"v"}}`,
		`xyz123:xyz123`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	// Exit codes: -e (0, 1, 4), empty stream without -e (0), invalid JSON (4), compile error (3), runtime error (5), missing file (2)
	exitCases := []struct {
		script string
		want   int
	}{
		{`echo 'true' | jq -e '.' >/dev/null`, 0},
		{`echo '0' | jq -e '.' >/dev/null`, 0},
		{`echo '""' | jq -e '.' >/dev/null`, 0},
		{`echo 'false' | jq -e '.' >/dev/null`, 1},
		{`echo 'null' | jq -e '.' >/dev/null`, 1},
		{`echo '{"a":1}' | jq -e '.missing' >/dev/null`, 1},
		{`echo '{"a":1}' | jq -e 'empty' >/dev/null`, 4},
		{`printf '' | jq -e '.' >/dev/null`, 4},
		{`jq -e '.' /dev/null >/dev/null`, 4},
		{`printf '' | jq '.' >/dev/null`, 0},
		{`echo '{bad json' | jq '.' 2>/dev/null`, 4},
		{`jq -n 'if true then 1' 2>/dev/null`, 3},
		{`jq -n 'error("boom")' 2>/dev/null`, 5},
		{`jq '.' nonexistent_file.json 2>/dev/null`, 2},
	}
	for _, tc := range exitCases {
		_, errOut, rerr := runParityScript(t, dir, tc.script)
		if got := exitCode(rerr); got != tc.want {
			t.Errorf("%s: got exit %d, want %d (stderr=%q)", tc.script, got, tc.want, errOut)
		}
	}
}

func TestJqPipelineCancellationAndContextTimeout(t *testing.T) {
	dir := t.TempDir()

	// Downstream closes pipe early (`head -n 3`): upstream `yes` + `jq` or infinite `range`/`repeat` must exit cleanly without hanging!
	out, stderr, err := runTimedScript(t, dir, 3*time.Second, `
yes '{"n": 7}' | jq -c '.n * 6' | head -n 3
jq -nc 'repeat(1)' | head -n 2
jq -nc 'range(0; 1000000000)' | head -n 3
`)
	if err != nil {
		t.Fatalf("pipeline failed: %v (stderr=%s)", err, stderr)
	}
	want := "42\n42\n42\n1\n1\n0\n1\n2\n"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	// Infinite loop inside jq (`while(true; . + 1) | select(. < 0)`) must honor context deadline promptly, even inside `try ... catch`!
	_, _, errTimeout := runTimedScript(t, dir, 150*time.Millisecond, `
jq -n 'def spin: try (while(true; . + 1) | empty) catch spin; 0 | spin'
`)
	if code := exitCode(errTimeout); code != 124 && errTimeout == nil {
		t.Fatalf("expected non-zero timeout cancellation, got err=%v", errTimeout)
	}
}

func TestJqAdversarialEdgeCases(t *testing.T) {
	dir := t.TempDir()

	// Large >40KB string spanning streaming decoder chunks + UTF-16 surrogate pairs + slice assignment + multi-index del + map_values(values)
	bigStr := strings.Repeat("abcdefghij", 4500) // 45,000 bytes
	mustWrite(t, filepath.Join(dir, "big.json"), `{"big":"`+bigStr+`","emoji":"\uD83D\uDE00"}`)

	out, stderr, err := runParityScript(t, dir, `
jq -c '{big_len: (.big | length), emoji: .emoji, emoji_runes: (.emoji | length), emoji_bytes: (.emoji | utf8bytelength)}' big.json
jq -nc '[10, 20, 30, 40] | [ del(.[1, -1]), (.[1:3] = [99]) ]'
jq -nc '{z: 1, drop: null, a: 2, keep_false: false} | map_values(values)'
jq -nc '"héllo"[1:4]'
`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	want := strings.Join([]string{
		`{"big_len":45000,"emoji":"😀","emoji_runes":1,"emoji_bytes":4}`,
		`[[10,30],[10,99,40]]`,
		`{"z":1,"a":2,"keep_false":false}`,
		`"éll"`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}
