package main

// GNU sort parity: the "last-resort" comparison.
//
// When two lines compare equal under the active key(s) and -s is NOT
// given, GNU sort breaks the tie by comparing the *whole lines* byte
// for byte. unish historically behaved as if -s were always on, which
// is invisible for plain `sort` (key == whole line) but diverges for
// `-k`, `-t/-k`, `-n` key ties and, most visibly, `-f`.
//
// Oracles captured from GNU coreutils 9.x:
//   printf 'x 1\nb 1\na 1\n'   | sort -k2      -> a 1 / b 1 / x 1
//   printf 'b\nA\na\nB\n'      | sort -f       -> A / a / B / b
//   printf 'A\na\nB\nb\n'      | sort -fu      -> A / B
//   printf 'z:10\na:2\nb:02\n' | sort -t: -k2 -n -> a:2 / b:02 / z:10
// and, with -s, the last-resort comparison is disabled:
//   printf 'x 1\nb 1\na 1\n'   | sort -s -k2   -> x 1 / b 1 / a 1

import (
	"testing"
)

func TestParitySortLastResort(t *testing.T) {
	dir := t.TempDir()

	// Key ties fall back to a whole-line comparison.
	out, se, err := runParityScript(t, dir, "printf 'x 1\\nb 1\\na 1\\n' | sort -k2")
	if exitCode(err) != 0 || out != "a 1\nb 1\nx 1\n" {
		t.Errorf("sort -k2 = %q err %q code %d; want a/b/x", out, se, exitCode(err))
	}

	// -f folds case for the key, then the whole line (unfolded) breaks ties.
	out, se, err = runParityScript(t, dir, "printf 'b\\nA\\na\\nB\\n' | sort -f")
	if exitCode(err) != 0 || out != "A\na\nB\nb\n" {
		t.Errorf("sort -f = %q err %q code %d; want A/a/B/b", out, se, exitCode(err))
	}

	// -u compares the folded key (case-insensitively) plus last-resort.
	out, se, err = runParityScript(t, dir, "printf 'A\\na\\nB\\nb\\n' | sort -fu")
	if exitCode(err) != 0 || out != "A\nB\n" {
		t.Errorf("sort -fu = %q err %q code %d; want A/B", out, se, exitCode(err))
	}

	// Numeric key ties also fall back to the whole line.
	out, se, err = runParityScript(t, dir, "printf 'z:10\\na:2\\nb:02\\n' | sort -t: -k2 -n")
	if exitCode(err) != 0 || out != "a:2\nb:02\nz:10\n" {
		t.Errorf("sort -t: -k2 -n = %q err %q code %d; want a:2/b:02/z:10", out, se, exitCode(err))
	}

	// -s disables the last-resort comparison: stable, input order kept.
	out, se, err = runParityScript(t, dir, "printf 'x 1\\nb 1\\na 1\\n' | sort -s -k2")
	if exitCode(err) != 0 || out != "x 1\nb 1\na 1\n" {
		t.Errorf("sort -s -k2 = %q err %q code %d; want input order", out, se, exitCode(err))
	}
}

// GNU sort -V parity: a faithful filevercmp port. The "mysterious"
// corners are the file-suffix cut (a suffix starts at a dot followed by
// a letter/~ or by end-of-string) and gnulib's byte order inside a run:
// '~' < end-of-string < digits < letters < all other bytes.
//
// Oracles captured from GNU coreutils 8.32 (Git-Bash):
//
//	printf '9.\n9b\n9a\n9-\n9\nx~\n' | sort -V -> 9 / 9. / 9a / 9b / 9- / x~
//	printf 'file10\nfile9\nfile1\n'   | sort -V -> file1 / file9 / file10
//	printf '02\n2\n1.02\n1.2\n'      | sort -V -> 1.02 / 1.2 / 02 / 2
//	printf 'ab\na.b\na y\n'           | sort -V -> a.b / ab / a y
func TestParitySortVersion(t *testing.T) {
	dir := t.TempDir()
	out, se, err := runParityScript(t, dir, "printf '9.\\n9b\\n9a\\n9-\\n9\\nx~\\n' | sort -V")
	if exitCode(err) != 0 || out != "9\n9.\n9a\n9b\n9-\nx~\n" {
		t.Errorf("sort -V dots = %q err %q code %d", out, se, exitCode(err))
	}
	out, se, err = runParityScript(t, dir, "printf 'file10\\nfile9\\nfile1\\n' | sort -V")
	if exitCode(err) != 0 || out != "file1\nfile9\nfile10\n" {
		t.Errorf("sort -V files = %q err %q code %d", out, se, exitCode(err))
	}
	out, se, err = runParityScript(t, dir, "printf '02\\n2\\n1.02\\n1.2\\n' | sort -V")
	if exitCode(err) != 0 || out != "1.02\n1.2\n02\n2\n" {
		t.Errorf("sort -V zeros = %q err %q code %d", out, se, exitCode(err))
	}
	out, se, err = runParityScript(t, dir, "printf 'ab\\na.b\\na y\\n' | sort -V")
	if exitCode(err) != 0 || out != "a.b\nab\na y\n" {
		t.Errorf("sort -V suffix = %q err %q code %d", out, se, exitCode(err))
	}
}

// GNU sed parity: a numeric flag combined with 'g' (in either order,
// e.g. 2g or g2) replaces every match from the nth onward, not just the
// nth. Oracles: sed 's/a/X/2g' on "aaaa" -> aXXX; sed 's/[0-9]/#/2g'
// on "a1b2c3" -> a1b#c#.
func TestParitySedNthGlobal(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ src, want string }{
		{"printf 'aaaa\\n' | sed 's/a/X/2g'", "aXXX\n"},
		{"printf 'aaaa\\n' | sed 's/a/X/g2'", "aXXX\n"},
		{"printf 'aaaaa\\n' | sed 's/a/X/g3'", "aaXXX\n"},
		{"printf 'aaaa\\n' | sed 's/a/X/1g'", "XXXX\n"},
		{"printf 'aaaa\\n' | sed 's/a/X/2'", "aXaa\n"},
		{"printf 'a1b2c3\\n' | sed 's/[0-9]/#/2g'", "a1b#c#\n"},
	} {
		out, se, err := runParityScript(t, dir, tc.src)
		if exitCode(err) != 0 || out != tc.want {
			t.Errorf("%s = %q err %q code %d; want %q", tc.src, out, se, exitCode(err), tc.want)
		}
	}
}
