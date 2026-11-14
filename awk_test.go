package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	if lookupExtra("awk") == nil {
		extraCommands = append(extraCommands, extraCmd{"awk", cmdAwk})
	}
	if lookupExtra("gawk") == nil {
		extraCommands = append(extraCommands, extraCmd{"gawk", cmdAwk})
	}
}

func findExternalGawk() string {
	if runtime.GOOS == "windows" {
		candidates := []string{
			`C:\Program Files\Git\usr\bin\gawk.exe`,
			`C:\Program Files\Git\usr\bin\awk.exe`,
			`C:\msys64\usr\bin\gawk.exe`,
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	if p, err := exec.LookPath("gawk"); err == nil {
		return p
	}
	return ""
}

func TestAwkCLIAndFlags(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "f1.txt"), "alpha 10\nbeta 20\n")
	mustWrite(t, filepath.Join(dir, "f2.txt"), "gamma 30\n")
	mustWrite(t, filepath.Join(dir, "p1.awk"), "BEGIN { hdr = \"START\" }\n")
	mustWrite(t, filepath.Join(dir, "p2.awk"), "{ print hdr, $1, $2 * 2 }\n")

	cases := []struct {
		name   string
		script string
		want   string
		wantRC int
	}{
		{
			name:   "attached and detached -F",
			script: `printf 'a:b:c\n' | awk -F: '{print $2}'; printf 'x|y|z\n' | awk -F '|' '{print $3}'`,
			want:   "b\nz\n",
		},
		{
			name:   "-F tab and escaped dot",
			script: "printf 'a\\tb\\tc\\n' | awk -F '\\t' '{print $2, NF}'; printf 'u.v.w\\n' | awk -F '\\.' '{print $1, $3}'",
			want:   "b 3\nu w\n",
		},
		{
			name:   "-F regex and -F empty UTF-8 rune split",
			script: `printf 'a,b;c\n' | awk -F '[,;]' '{print $1, $2, $3}'; printf 'hél\n' | awk -F '' '{print NF, $1, $2, $3}'`,
			want:   "a b c\n3 h é l\n",
		},
		{
			name:   "-v C escapes and strnum",
			script: `awk -v sep='\t' -v a=10 -v b=2 'BEGIN { printf "%s%s%d\n", "ok", sep, (a > b) }'`,
			want:   "ok\t1\n",
		},
		{
			name:   "repeatable -f program files",
			script: `awk -f p1.awk -f p2.awk f1.txt`,
			want:   "START alpha 20\nSTART beta 40\n",
		},
		{
			name:   "inline var=val interspersed with file operands",
			script: `awk '{ print tag, FNR, NR, $1 }' tag=ONE f1.txt tag=TWO f2.txt`,
			want:   "ONE 1 1 alpha\nONE 2 2 beta\nTWO 1 3 gamma\n",
		},
		{
			name:   "stdin dash between files",
			script: `printf 'mid 99\n' | awk '{ print FILENAME, FNR, $1 }' f1.txt - f2.txt`,
			want:   "f1.txt 1 alpha\nf1.txt 2 beta\n- 1 mid\nf2.txt 1 gamma\n",
		},
		{
			name:   "gawk alias and compat flags",
			script: `printf '1 2\n' | gawk --posix -W compat '{ print $1 + $2 }'`,
			want:   "3\n",
		},
		{
			name:   "double-dash option terminator",
			script: `awk -- 'BEGIN { print ARGV[1], ARGC }' -v; awk 'BEGIN { print ARGV[1], ARGV[2], ARGC }' -- -v`,
			want:   "-v 2\n-- -v 3\n",
		},
		{
			name:   "BEGIN-only skips nonexistent file",
			script: `awk 'BEGIN { print "solo" }' nonexistent_xyz.txt`,
			want:   "solo\n",
		},
		{
			name:   "missing file with body rule exits 2",
			script: `awk '{ print }' nonexistent_xyz.txt`,
			want:   "",
			wantRC: 2,
		},
		{
			name:   "nextfile skips rest of file and ENVIRON reads shell exports",
			script: `MY_AWK_VAR=hello123 awk 'FNR == 1 { print ENVIRON["MY_AWK_VAR"], FILENAME, $1; nextfile } { print "SHOULD_SKIP" }' f1.txt f2.txt`,
			want:   "hello123 f1.txt alpha\nhello123 f2.txt gamma\n",
		},
		{
			name:   "ARGV clear skips file without reading stdin while var-only reads stdin",
			script: `printf 'from_stdin\n' | awk 'BEGIN { ARGV[1] = "" } { print } END { print "done", NR }' f1.txt; printf 'line\n' | awk '{ print k, $0 }' k=77`,
			want:   "done 0\n77 line\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, se, err := runParityScript(t, dir, tc.script)
			if gotRC := exitCode(err); gotRC != tc.wantRC {
				t.Fatalf("rc=%d, want %d (stderr=%q, out=%q)", gotRC, tc.wantRC, se, out)
			}
			if tc.wantRC == 0 && out != tc.want {
				t.Fatalf("out=%q, want %q (stderr=%q)", out, tc.want, se)
			}
		})
	}
}

func TestAwkLexerRegexVsDivisionAndGrammar(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name:   "chained division and slash-assign",
			script: `awk 'BEGIN { x = 100; x /= 5; print 10 / 2 / 5, x, (8 + 4) / 3 }'`,
			want:   "1 20 4\n",
		},
		{
			name:   "regex after print, operators, parens, and starting with =",
			script: `printf '=foo\nbar\na/b\n' | awk '/=foo/ { print "eq", /foo/ } /a[/]b/ { print "brk" } /a\/b/ { print "esc" }'`,
			want:   "eq 1\nbrk\nesc\n",
		},
		{
			name: "newline continuation rules and if-else with semicolon",
			script: `awk 'BEGIN {
				x = 1 + \
				    2
				if (x == 3) a = "yes"; else a = "no"
				y = (x > 2 &&
				     a == "yes") ?
				    100 :
				    200
				print x, a, y
			}'`,
			want: "3 yes 100\n",
		},
		{
			name:   "range patterns including single-line self-closing range",
			script: `printf '0\nS\n1\nE\n2\nS E\n3\n' | awk '/S/, /E/ { print NR ":" $0 }'`,
			want:   "2:S\n3:1\n4:E\n6:S E\n",
		},
		{
			name:   "precedence of unary minus vs exponentiation and field inc/dec",
			script: `awk 'BEGIN { $0 = "10 20"; i = 1; print -2^2, 2^-2, 2^3^2, $1++, $1, $i++, i, $++i }'`,
			want:   "-4 0.25 512 10 11 11 1 20\n",
		},
		{
			name:   "space disambiguates concatenation vs user call while bare length works",
			script: `printf 'hello\nhi\n' | awk 'length > 3 { x = "pre"; print x (1 + 2), length }'`,
			want:   "pre3 5\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, se, err := runParityScript(t, dir, tc.script)
			if exitCode(err) != 0 || out != tc.want {
				t.Fatalf("got %q (rc=%d, stderr=%q), want %q", out, exitCode(err), se, tc.want)
			}
		})
	}
}

func TestAwkStrnumFieldsAndRecords(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name: "strnum vs string literal vs uninit comparisons",
			script: `printf '10 2 010 0\n' | awk '{
				print ($1 > $2), ("10" > "2"), ($1 == $3), ($1 == "010"), ($4 ? 1 : 0), ("0" ? 1 : 0), (u == 0), (u == ""), (u == "0")
			}'`,
			want: "1 0 1 0 0 1 1 1 0\n",
		},
		{
			name: "bidirectional $0, $k, NF and OFS sync",
			script: `awk 'BEGIN {
				FS = ":"
				OFS = ","
				$0 = "a:b:c"
				print NF, $1, $2, $3
				$2 = "B"
				print $0
				$5 = "E"
				print NF, "<" $0 ">"
				NF = 2
				print NF, "<" $0 ">"
				print ($9 == ""), ($9 == 0), NF
			}'`,
			want: "3,a,b,c\na,B,c\n5,<a,B,c,,E>\n2,<a,B>\n1,0,2\n",
		},
		{
			name:   "paragraph mode RS empty",
			script: "printf '\\n\\na 1\\nb 2\\n\\n\\nc 3\\n' | awk 'BEGIN { RS = \"\"; OFS = \"|\" } { $1 = $1; print NR, NF, $0 }'",
			want:   "1|4|a|1|b|2\n2|2|c|3\n",
		},
		{
			name:   "exact large integer formatting vs OFMT/CONVFMT",
			script: `awk 'BEGIN { OFMT = "%.2f"; print 12345678, 3.14159 }'`,
			want:   "12345678 3.14\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, se, err := runParityScript(t, dir, tc.script)
			if exitCode(err) != 0 || out != tc.want {
				t.Fatalf("got %q (rc=%d, stderr=%q), want %q", out, exitCode(err), se, tc.want)
			}
		})
	}
}

func TestAwkArraysFunctionsExitAndRedirections(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "nums.txt"), "one\ntwo\nthree\n")

	// 1. Associative arrays, multi-dim SUBSEP, non-vivifying `in`, `delete arr`
	out, se, err := runParityScript(t, dir, `awk 'BEGIN {
		print ("x" in a), length(a)
		a[1, 2] = "cell"
		print ((1, 2) in a), ((1, 3) in a), length(a), a[1 SUBSEP 2]
		delete a[1, 2]
		print length(a)
		a["p"] = 1; a["q"] = 2
		delete a
		print length(a)
	}'`)
	if exitCode(err) != 0 || out != "0 0\n1 0 1 cell\n0\n0\n" {
		t.Fatalf("arrays failed: out=%q rc=%d se=%q", out, exitCode(err), se)
	}

	// 2. Recursion, pass-by-value scalar vs pass-by-ref array vs local array cleanup
	out, se, err = runParityScript(t, dir, `awk '
	function fib(n) {
		if (n <= 1) return n
		return fib(n - 1) + fib(n - 2)
	}
	function mutate(arr, sc,    loc) {
		arr["added"] = 99
		sc = 777
		loc["secret"] = 1
		return length(loc)
	}
	BEGIN {
		sc = 42
		loc = "global_preserved"
		r = mutate(myarr, sc)
		print fib(7), myarr["added"], sc, loc, r
	}'`)
	if exitCode(err) != 0 || out != "13 99 42 global_preserved 1\n" {
		t.Fatalf("functions failed: out=%q rc=%d se=%q", out, exitCode(err), se)
	}

	// 3. Exit inside BEGIN / body STILL runs END, and exit in END overrides or preserves code
	out, se, err = runParityScript(t, dir, `awk 'BEGIN { print "B"; exit 7 } { print "BODY" } END { print "E" }' nums.txt`)
	if exitCode(err) != 7 || out != "B\nE\n" {
		t.Fatalf("exit->END failed: out=%q rc=%d se=%q", out, exitCode(err), se)
	}
	out, se, err = runParityScript(t, dir, `awk 'BEGIN { exit 7 } END { print "E1"; exit 3; print "E2" }'`)
	if exitCode(err) != 3 || out != "E1\n" {
		t.Fatalf("END exit override failed: out=%q rc=%d se=%q", out, exitCode(err), se)
	}
	out, se, err = runParityScript(t, dir, `awk 'BEGIN { exit 7 } END { print "E1"; exit; print "E2" }'`)
	if exitCode(err) != 7 || out != "E1\n" {
		t.Fatalf("END bare exit preserve failed: out=%q rc=%d se=%q", out, exitCode(err), se)
	}

	// 4. Print redirections (`>`, `>>`, `print ($1 > 2) > file`), close(), and getline < file
	out, se, err = runParityScript(t, dir, `awk 'BEGIN {
		print "line1" > "out.txt"
		print "line2" > "out.txt"
		close("out.txt")
		print "fresh" > "out.txt"
		close("out.txt")
		print "appended" >> "out.txt"
		close("out.txt")
		while ((getline ln < "out.txt") > 0) {
			print "read:" ln
		}
		close("out.txt")
		print (5 > 2) > "cmp.txt"
		close("cmp.txt")
		getline c < "cmp.txt"
		print "cmp:" c
	}'`)
	if exitCode(err) != 0 || out != "read:fresh\nread:appended\ncmp:1\n" {
		t.Fatalf("redir/getline failed: out=%q rc=%d se=%q", out, exitCode(err), se)
	}
}

func TestAwkBuiltinsComprehensive(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name:   "UTF-8 length, substr, index, tolower, toupper",
			script: `awk 'BEGIN { s = "héllo世界"; print length(s), substr(s, 2, 4), substr(s, 0, 2), index(s, "世界"), toupper("abC"), tolower("XyZ") }'`,
			want:   "7 éllo hé 6 ABC xyz\n",
		},
		{
			name: "sub and gsub ampersand and backslash replacement rules",
			script: `awk 'BEGIN {
				a = "foo"; sub(/foo/, "[&]", a)
				b = "foo"; sub(/foo/, "\\&", b)
				c = "foo"; sub(/foo/, "\\\\&", c)
				d = "foo"; sub(/foo/, "\\\\\\&", d)
				e = "ab"; gsub(/^/, ">", e); gsub(/$/, "<", e)
				print a, b, c, d, e
			}'`,
			want: "[foo] & \\foo \\& >ab<\n",
		},
		{
			name: "gensub global and Nth occurrence with captures",
			script: `awk 'BEGIN {
				print gensub(/([a-z]+)([0-9]+)/, "\\2-\\1", "g", "ab12 cd34")
				print gensub(/a/, "X", 2, "abacada")
			}'`,
			want: "12-ab 34-cd\nabXcada\n",
		},
		{
			name: "match with RSTART/RLENGTH and gawk capture array",
			script: `awk 'BEGIN {
				pos = match("id=42:ok", /([a-z]+)=([0-9]+)/, m)
				print pos, RSTART, RLENGTH, m[0], m[1], m[2], m[2, "start"], m[2, "length"]
			}'`,
			want: "1 1 5 id=42 id 42 4 2\n",
		},
		{
			name: "split self-element, empty string, literal dot vs regex dot",
			script: `awk 'BEGIN {
				a[1] = "x:y:z"
				n1 = split(a[1], a, ":")
				n2 = split("", empty, ":")
				n3 = split("p.q", lit, ".")
				n4 = split("pq", rx, /./)
				print n1, a[2], n2, length(empty), n3, lit[1], lit[2], n4
			}'`,
			want: "3 y 0 0 2 p q 3\n",
		},
		{
			name: "printf and sprintf verbs, dynamic width/prec, char codes",
			script: `awk 'BEGIN {
				printf "%s|%05d|%u|%x|%X|%o|%.2f|%*.*s|%c|%c|%%\n", "hi", -7, 42, 255, 255, 8, 3.14159, 5, 3, "abcdef", 65, "Zebra"
			}'`,
			want: "hi|-0007|42|ff|FF|10|3.14|  abc|A|Z|%\n",
		},
		{
			name: "math and UTC strftime",
			script: `awk 'BEGIN {
				srand(123)
				r1 = rand()
				prev = srand(123)
				r2 = rand()
				print int(-3.9), sqrt(16), int(atan2(0, -1) * 100), (r1 == r2), prev, strftime("%Y-%m-%d %H:%M:%S", 0, 1)
			}'`,
			want: "-3 4 314 1 123 1970-01-01 00:00:00\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, se, err := runParityScript(t, dir, tc.script)
			if exitCode(err) != 0 || out != tc.want {
				t.Fatalf("got %q (rc=%d, stderr=%q), want %q", out, exitCode(err), se, tc.want)
			}
		})
	}
}

func TestAwkDifferentialParityWithGawk(t *testing.T) {
	gawkBin := findExternalGawk()
	if gawkBin == "" {
		t.Skip("external gawk not found; standalone parity tests already verified")
	}

	dir := t.TempDir()
	sampleInput := "alice 10 2.5\nbob 2 100\ncharlie 010 3.75\ndave 0 -4\neve 10 0\n"

	programs := []string{
		`{ print $1, $2 + $3 }`,
		`$2 > 5 { print NR, NF, $1 }`,
		`$2 == 10 { print "ten:", $1, $2 }`,
		`$2 == "10" { print "str10:", $1 }`,
		`$2 { print "truthy:", $1 }`,
		`BEGIN { OFS = ":" } { $2 = $2 * 10; print $0 }`,
		`{ sum += $2; cnt++ } END { printf "sum=%d avg=%.2f\n", sum, sum/cnt }`,
		`NR == 2, NR == 4 { print FNR, $1 }`,
		`/bob/, /dave/ { if ($1 == "charlie") next; print $1 }`,
		`BEGIN { for (i = 1; i <= 5; i++) { if (i == 3) continue; printf "%d ", i }; print "" }`,
		`BEGIN { i = 0; do { i++ } while (i < 4); print i }`,
		`{ a[$2] += 1 } END { print a[10], a[2], (999 in a), length(a) }`,
		`{ n = split($0, w, " "); print n, w[1], w[n] }`,
		`{ sub(/[aeiou]/, "X"); gsub(/0/, "Z"); print }`,
		`{ s = $1; sub(/.*/, "[&]\\&\\\\&", s); print s }`,
		`BEGIN { print 10 / 2 / 5, -2^2, 2^-2, 17 % 5 }`,
		`function f(x) { return x <= 1 ? 1 : x * f(x-1) } BEGIN { print f(6) }`,
		`BEGIN { exit 4 } { print "nope" } END { print "end-ran" }`,
		`BEGIN { exit 4 } END { print "override"; exit 2 }`,
		`length > 12 { print $1, length }`,
		`{ print substr($1, 2, 3), index($1, "e"), toupper($1), tolower("HELLO") }`,
		`{ if (match($1, /[aeiou]+/)) print $1, RSTART, RLENGTH }`,
		`{ print gensub(/([a-z])([a-z]+)/, "\\1_\\2", 1, $1) }`,
		`{ a[NR, $1] = $2 } END { print ((1, "alice") in a), ((2, "alice") in a), a[1, "alice"], length(a) }`,
		`function bump(arr, v,   loc) { arr["k"] += v; v = 999; loc[1] = 7; return arr["k"] } { x = $2; r = bump(acc, x) } END { print acc["k"], x, length(loc) }`,
		`{ if (getline nxt > 0) print $1 "+" nxt; else print "odd:" $1 }`,
		`BEGIN { CONVFMT = "%.2f"; OFMT = "%.1f"; x = 1.2345; print x, x "", 1000000 }`,
		`{ printf "%-8s %04d %x %.2f\n", $1, $2, $2 + 10, $3 }`,
		`BEGIN { FS = "" } NR == 1 { print NF, $1, $2, $5 }`,
		`{ $5 = "tail"; print NF, "<" $0 ">"; NF = 2; print NF, "<" $0 ">" }`,
		`BEGIN { exit 9 } END { print "in-end"; exit }`,
		`NR == 1 { $0 = "a,b c,d"; FS = ","; print "r1", $1, $2 } NR == 2 { $0 = "a,b c,d"; print "r2", $1, $2 }`,
		`BEGIN { print 1e16, 1e18, 2^62, 2^53+2, (2^62) "" }`,
		`BEGIN { printf "before "; rc = system("echo mid"); print "after", rc }`,
		`BEGIN { printf "c\na\nb\n" | "sort"; rc = close("sort"); print "rc=" rc }`,
		`BEGIN { cmd = "printf '10\n20\n30\n'"; while ((cmd | getline v) > 0) s += v; print s, close(cmd) }`,
		`BEGIN { inf = 1e300*1e300; printf "%s %s %g %G [%8f]\n", inf, -inf, inf, inf, inf }`,
	}

	for idx, prog := range programs {
		t.Run(fmt.Sprintf("case_%02d", idx), func(t *testing.T) {
			progPath := filepath.Join(dir, fmt.Sprintf("prog_%02d.awk", idx))
			mustWrite(t, progPath, prog)

			// Run external GNU gawk via -f so Windows MSYS2 argv escaping never mangles backslashes
			cmd := exec.Command(gawkBin, "-f", progPath)
			cmd.Dir = dir
			if runtime.GOOS == "windows" {
				// Ensure external gawk finds POSIX coreutils (e.g. sort) ahead of C:\Windows\System32
				cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(gawkBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			cmd.Stdin = strings.NewReader(sampleInput)
			var gOut, gErr bytes.Buffer
			cmd.Stdout = &gOut
			cmd.Stderr = &gErr
			gRunErr := cmd.Run()
			gRC := 0
			if gRunErr != nil {
				if ee, ok := gRunErr.(*exec.ExitError); ok {
					gRC = ee.ExitCode()
				} else {
					t.Fatalf("failed to execute gawk: %v", gRunErr)
				}
			}
			wantOut := strings.ReplaceAll(gOut.String(), "\r\n", "\n")

			// Run embedded unish cmdAwk directly
			var uOut, uErr bytes.Buffer
			hc := interp.HandlerContext{
				Dir:    dir,
				Stdin:  strings.NewReader(sampleInput),
				Stdout: &uOut,
				Stderr: &uErr,
			}
			uRunErr := cmdAwk(context.Background(), hc, []string{"awk", prog})
			uRC := exitCode(uRunErr)

			if uRC != gRC || uOut.String() != wantOut {
				t.Fatalf("prog %q mismatch:\n  unish (rc=%d): %q (stderr=%q)\n  gawk  (rc=%d): %q (stderr=%q)",
					prog, uRC, uOut.String(), uErr.String(), gRC, wantOut, gErr.String())
			}
		})
	}
}

func TestAwkCancellationAndBrokenPipe(t *testing.T) {
	dir := t.TempDir()

	// 1. Downstream broken pipe: `yes | awk '{print}' | head -n 1` must exit fast & cleanly
	out, se, err := runTimedScript(t, dir, 3*time.Second, `yes | awk '{ print $0 }' | head -n 1`)
	if exitCode(err) != 0 || out != "y\n" {
		t.Fatalf("broken pipe pipeline failed: out=%q rc=%d stderr=%q", out, exitCode(err), se)
	}

	// 2. Infinite loop under `timeout` must respect ctx.Done() and exit 124
	_, se, err = runTimedScript(t, dir, 3*time.Second, `timeout 0.15 awk 'BEGIN { while (1) { x++ } }'`)
	if got := exitCode(err); got != 124 {
		t.Fatalf("timeout awk infinite loop: rc=%d, want 124 (stderr=%q)", got, se)
	}

	// 3. Direct context cancellation on C-style `for (;;)`
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	hc := interp.HandlerContext{
		Dir:    dir,
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	start := time.Now()
	cerr := cmdAwk(ctx, hc, []string{"awk", "BEGIN { for (;;) { i++ } }"})
	if cerr == nil {
		t.Fatalf("expected context deadline error from infinite loop")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation took too long: %v", elapsed)
	}

	// 4. Long line (>128 KB) exercises ReadSlice ErrBufferFull seamlessly
	longToken := strings.Repeat("A", 150*1024)
	mustWrite(t, filepath.Join(dir, "long.txt"), longToken+" END_TOK\nshort line\n")
	out, se, err = runParityScript(t, dir, `awk '{ print NR, NF, length($1), $2 }' long.txt`)
	if exitCode(err) != 0 || out != "1 2 153600 END_TOK\n2 2 5 line\n" {
		t.Fatalf("long line failed: out=%q rc=%d se=%q", out, exitCode(err), se)
	}
}

func TestAwkPipesSystemFSAndLargeInts(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name   string
		script string
		want   string
		wantRC int
	}{
		{
			name:   "print pipe to sort closed on implicit exit",
			script: `printf 'cherry\napple\nbanana\n' | awk '{ print | "sort" }'`,
			want:   "apple\nbanana\ncherry\n",
		},
		{
			name:   "printf pipe with explicit close and reopen",
			script: `awk 'BEGIN { printf "%s\n%s\n", "z", "a" | "sort"; rc1 = close("sort"); print "mid", rc1; print "m" | "sort"; rc2 = close("sort"); print "end", rc2 }'`,
			want:   "a\nz\nmid 0\nm\nend 0\n",
		},
		{
			name:   "cmd pipe into getline target and $0 without bumping NR",
			script: `awk 'BEGIN { "echo alpha beta" | getline; close("echo alpha beta"); print $2, NR; cmd = "seq 4"; while ((cmd | getline l) > 0) sum += l; rc = close(cmd); print sum, rc }'`,
			want:   "beta 0\n10 0\n",
		},
		{
			name:   "unparenthesized cmd | getline binds tighter than comparison",
			script: `awk 'BEGIN { while ("seq 3" | getline x > 0) out = out x ","; close("seq 3"); print out }'`,
			want:   "1,2,3,\n",
		},
		{
			name:   "early close on streaming input pipe does not hang",
			script: `awk 'BEGIN { "yes" | getline first; close("yes"); print "got:" first }'`,
			want:   "got:y\n",
		},
		{
			name:   "system and close exit codes and stdout flush ordering",
			script: `awk 'BEGIN { printf "before "; rc1 = system("echo mid"); print "hi" | "exit 5"; rc2 = close("exit 5"); "exit 3" | getline x; rc3 = close("exit 3"); rc4 = system("exit 7"); print "after", rc1, rc2, rc3, rc4 }'`,
			want:   "before mid\nafter 0 5 3 7\n",
		},
		{
			name:   "FS assignment mid-record preserves current record fields",
			script: `printf 'a,b\nc,d\n' | awk '{ FS = ","; print $1 }'`,
			want:   "a,b\nc\n",
		},
		{
			name:   "FS assignment followed by $0=$0 re-splits with new FS",
			script: `printf 'a,b\n' | awk '{ FS = ","; $0 = $0; print $1, $2 }'`,
			want:   "a b\n",
		},
		{
			name:   "exact integers above 2^53 up to int64 in OFMT and CONVFMT",
			script: `echo 435423839299999999 | awk '{ print 1e16, 1e18, 2^62, 2^53+2, (2^62) "", $1+0 }'`,
			want:   "10000000000000000 1000000000000000000 4611686018427387904 9007199254740994 4611686018427387904 435423839300000000\n",
		},
		{
			name:   "printf %s and float verbs on Inf and NaN",
			script: `awk 'BEGIN { inf = 1e300*1e300; nan = log(-1); printf "%s %s %s %g %G [%6f]\n", inf, -inf, nan, inf, nan, inf }'`,
			want:   "+inf -inf nan +inf NAN [  +inf]\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, se, err := runTimedScript(t, dir, 5*time.Second, tc.script)
			rc := exitCode(err)
			if rc != tc.wantRC || out != tc.want {
				t.Fatalf("script: %s\n  got  (rc=%d): %q (stderr=%q)\n  want (rc=%d): %q",
					tc.script, rc, out, se, tc.wantRC, tc.want)
			}
		})
	}
}

func BenchmarkAwkThroughput(b *testing.B) {
	var buf strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&buf, "svc_%d INFO %d 200 /api/v1/items\n", i%8, (i*7)%100)
	}
	input := buf.String()
	prog := `/INFO/ { sum[$1] += $3; total += $3 } END { print total, length(sum) }`

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var out, errBuf bytes.Buffer
		hc := interp.HandlerContext{
			Stdin:  strings.NewReader(input),
			Stdout: &out,
			Stderr: &errBuf,
		}
		if err := cmdAwk(context.Background(), hc, []string{"awk", prog}); err != nil {
			b.Fatalf("awk failed: %v", err)
		}
	}
}
