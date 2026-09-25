package main

import (
	"strings"
	"testing"
)

// GNU parity regressions for the edge cases found in the differential
// testing campaign (unish vs GNU coreutils 9.4 / bash 5.2).

// ---------- sort ----------

func TestParitySortKeys(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		{`printf 'a 2\na 1\nb 1\nb 2\n' | sort -k1,1 -k2,2n`, "a 1\na 2\nb 1\nb 2\n"},
		{`printf 'x 1\nb 1\na 1\n' | sort -k2,2 -k1,1`, "a 1\nb 1\nx 1\n"},
		// key-local -r must not reverse the last-resort whole-line tiebreak
		{`printf 'x 1\nb 1\na 1\n' | sort -k2,2r`, "a 1\nb 1\nx 1\n"},
		// global -r does reverse it
		{`printf 'x 1\nb 1\na 1\n' | sort -r -k2`, "x 1\nb 1\na 1\n"},
		// leading blanks belong to the field unless -b
		{`printf '1\n2\n y\n z\n' | sort -k1,1`, " y\n z\n1\n2\n"},
		{`printf '1\n2\n y\n z\n' | sort -b -k1,1`, "1\n2\n y\n z\n"},
		// F.C offsets count from the field start
		{`printf 'a x1\na y2\n' | sort -k2.2`, "a x1\na y2\n"},
		{`printf 'a 123\na 124\n' | sort -u -k2.2,2.3`, "a 123\n"},
		// -n never accepts exponents, and compares arbitrary precision
		{`printf '1e3\n9\n10\n' | sort -n`, "1e3\n9\n10\n"},
		{`printf '10000000000000000001\n9999999999999999999\n' | sort -n`, "9999999999999999999\n10000000000000000001\n"},
		{`printf -- '-.5\n-\n.\n-0\n+2\n' | sort -n`, "-.5\n+2\n-\n-0\n.\n"},
		// -cu reports duplicates as disorder
		{`printf 'a\na\n' | sort -cu 2>&1; echo rc=$?`, "sort: -:2: disorder: a\nrc=1\n"},
		// multi-char tab rejected
		{`printf 'a::1\n' | sort -t:: -k2 2>&1; echo rc=$?`, "sort: multi-character tab \u2018::\u2019\nrc=2\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
}

// ---------- sed ----------

func TestParitySedEdges(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		// N at EOF prints the pattern space and stops
		{`printf 'a\nb\nc\n' | sed 'N;s/^/> /'`, "> a\nb\nc\n"},
		// blocks must not auto-print; d inside a block ends the cycle
		{`printf 'a\nb\nc\n' | sed '$!{N};s/\n/-/'`, "a-b\nc\n"},
		{`printf 'a\nb\n' | sed '1{h;d};G'`, "b\na\n"},
		// c with a range emits once, when the range closes
		{`printf 'a\nb\nc\nd\n' | sed '2,3c TEXT'`, "a\nTEXT\nd\n"},
		{`printf 'a\nb\nc\n' | sed '/a/,/zz/c TEXT'`, ""},
		// qN honors the exit code
		{`printf 'a\nb\n' | sed '1q3'; echo rc=$?`, "a\nrc=3\n"},
		// empty regex reuses the previous one; without one it errors
		{`printf 'a\n' | sed '/a/{s//Y/}'`, "Y\n"},
		{`printf 'abc\n' | sed 's//X/' 2>&1; echo rc=$?`, "sed: -e expression #1, char 0: no previous regular expression\nrc=1\n"},
		// invalid backreference errors
		{`printf 'a\n' | sed 's/a/\1/' 2>&1; echo rc=$?`, "sed: -e expression #1, char 1: invalid reference \\1 on `s' command's RHS\nrc=1\n"},
		// addr2 forms +N and ~N
		{`printf 'a\nb\nc\nd\n' | sed -n '1,+2p'`, "a\nb\nc\n"},
		{`printf 'a\nb\nc\nd\ne\nf\n' | sed -n '/a/,~3p'`, "a\nb\nc\n"},
		// literal s/// must still report "not substituted" (t loops)
		{`printf 'aaa\n' | sed ':a;s/a//;ta'`, "\n"},
		{`printf 'zzz\n' | sed 's/a/X/;s/b/Y/'`, "zzz\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
}

// ---------- printf ----------

func TestParityPrintfEdges(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		// the format is reused only while operands remain
		{`printf '[%*s]\n' 5 ab; echo END`, "[   ab]\nEND\n"},
		// %d truncates floats (with a diagnostic) and rejects garbage
		{`printf '[%d]\n' 3.7 2>&1; echo rc=$?`, "printf: 3.7: invalid number\n[3]\nrc=1\n"},
		{`printf '[%d]\n' 0x1f`, "[31]\n"},
		// %u/%x wrap negatives like bash
		{`printf '[%x]\n' -1`, "[ffffffffffffffff]\n"},
		// overflow clamps with a warning, exit stays 0
		{`printf '%d\n' 9223372036854775808 2>&1; echo rc=$?`, "printf: warning: 9223372036854775808: Numerical result out of range\n9223372036854775807\nrc=0\n"},
		// format escapes
		{`printf 'a\"b\n' | od -An -tx1 | tr -s ' '`, " 61 22 62 0a\n"},
		{`printf '\e' | od -An -tx1 | tr -s ' '`, " 1b\n"},
		{`printf '\u0041\n'`, "A\n"},
		// -- ends the options
		{`printf -- '%d\n' -42`, "-42\n"},
		// %a hex floats, %q shell quoting
		{`printf '%a\n' 1.5`, "0x1.8p+0\n"},
		{`printf '%q\n' 'a b'`, "a\\ b\n"},
		// no operand: usage error with status 2
		{`printf; echo rc=$?`, "rc=2\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
}

// ---------- trailing newline / byte transparency ----------

func TestParityTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		{`printf 'abcdef\n' | head -c3 | od -An -tx1 | tr -s ' '`, " 61 62 63\n"},
		{`printf 'a\nb' | tail -1 | od -An -tx1 | tr -s ' '`, " 62\n"},
		{`printf 'ab' | cat -n | od -An -tx1 | tr -s ' '`, " 20 20 20 20 20 31 09 61 62\n"},
		{`printf 'abcd' | rev | od -An -tx1 | tr -s ' '`, " 64 63 62 61\n"},
		{`printf 'hi' | base64 -w0 | od -An -tx1 | tr -s ' '`, " 61 47 6b 3d\n"},
		// cat -A/-v render controls and CRs; input bytes stay intact
		{`printf 'a\r\nb\r' | cat -A | od -An -tx1 | tr -s ' '`, " 61 5e 4d 24 0a 62 5e 4d\n"},
		{`printf 'a\001b\n' | cat -v`, "a^Ab\n"},
		// cat -E marks terminated lines only
		{`printf 'a\n' | cat -E`, "a$\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
}

// ---------- wc / uniq ----------

func TestParityWcUniqEdges(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		{`printf '\tabc\n' | wc -L`, "11\n"},
		{`printf 'ab\tc\n' | wc -L`, "9\n"},
		{`printf 'ab\ncd\n' | uniq -w0`, "ab\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
}

// ---------- glob / parameter expansion / arithmetic ----------

func TestParityExpansionEdges(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		{`mkdir -p d/e; touch d/e/f.txt; echo d/*/f.*`, "d/e/f.txt\n"},
		{`u=abcabc; echo "<${u/#abc/X}> <${u/%abc/Y}>"`, "<Xabc> <abcY>\n"},
		{`v=abc; echo "<${v/b/[&]}> <${v/b/\&}>"`, "<a[b]c> <a&c>\n"},
		{`t=AbC; echo "<${t~}>"`, "<abC>\n"},
		{`echo "$((--5)) $((++5)) $((010))"`, "5 5 8\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
	// Arithmetic errors abort the script like bash, with status 1.
	for _, script := range []string{
		`x=08; echo $((x)); echo unreachable`,
		`x=$((1/0)); echo unreachable`,
	} {
		out, se, err := runParityScript(t, dir, script)
		if exitCode(err) != 1 || strings.Contains(out, "unreachable") {
			t.Errorf("%s = %q (rc %d, err %q); want abort with rc 1", script, out, exitCode(err), se)
		}
	}
}

// ---------- shell builtins ----------

func TestParityBuiltinEdges(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		{`[ -e /dev/null ] && echo yes || echo no`, "yes\n"},
		{`[ -c /dev/null ] && echo chr || echo nochr`, "chr\n"},
		{`[ 1 = 1 2>&1; echo rc=$?`, "[: missing `]'\nrc=2\n"},
		{`true | false; echo "${PIPESTATUS[0]}-${PIPESTATUS[1]}"`, "0-1\n"},
		{`exec 3> f; echo hi >&3; exec 3>&-; cat f`, "hi\n"},
		{`printf 'abcd\n' | { read -n 2 v; echo [$v]; }`, "[ab]\n"},
		{`trap 'echo int' INT TERM; echo ok`, "ok\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
}

// ---------- file tools ----------

func TestParityFileToolEdges(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		script, want string
	}{
		{`basename /a/b/c c; basename /; basename a/b///`, "c\n/\nb\n"},
		{`dirname 'a/b///'; dirname '///'; dirname ''`, "a\n/\n.\n"},
		{`touch f; find . -nosuchpred 2>&1; echo rc=$?`, "find: unknown predicate `-nosuchpred'\nrc=1\n"},
		{`mkdir d; echo x > d/f; tar -cf a.tar d; rm -rf d; tar -xf a.tar -C nosuch 2>&1; echo rc=$?`, "tar: nosuch: Cannot open: No such file or directory\ntar: Error is not recoverable: exiting now\nrc=2\n"},
		{`printf 'x\n' | xargs false; echo rc=$?`, "rc=123\n"},
		{`echo x > f; touch -d '2020-01-02 03:04:05' f; date -r f '+%Y-%m-%d'`, "2020-01-02\n"},
		{`printf 'hello x\n' | fold -3`, "hel\nlo \nx\n"},
		{`printf 'a::b\n' | cut -d:: -f2 2>&1; echo rc=$?`, "cut: the delimiter must be a single character\nTry 'cut --help' for more information.\nrc=1\n"},
		{`printf 'cba\n' | tr 'z-a' 'a-z' 2>&1; echo rc=$?`, "tr: range-endpoints of 'z-a' are in reverse collating sequence order\nrc=1\n"},
		{`printf 'a\x00b\n' | grep b 2>&1; echo rc=$?`, "grep: (standard input): binary file matches\nrc=0\n"},
		{`printf 'a\n' | grep -H a`, "(standard input):a\n"},
		// cp -p must exist and preserve the modification time
		{`echo x > f; touch -d '2020-01-02 03:04:05' f; cp -p f g; echo rc=$?; date -r g '+%Y-%m-%d'`, "rc=0\n2020-01-02\n"},
	}
	for _, c := range cases {
		out, se, err := runParityScript(t, dir, c.script)
		if exitCode(err) != 0 || out != c.want {
			t.Errorf("%s = %q (rc %d, err %q); want %q", c.script, out, exitCode(err), se, c.want)
		}
	}
}
