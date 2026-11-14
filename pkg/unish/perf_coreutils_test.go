package unish

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func runPerfScript(t testing.TB, dir, src string) (string, string, int) {
	t.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	r, err := interp.New(
		interp.Dir(dir),
		interp.StdIO(nil, &stdout, &stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatalf("interpreter error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = r.Run(ctx, prog)
	code := 0
	if err != nil {
		if st, ok := err.(interp.ExitStatus); ok {
			code = int(st)
		} else if ee, ok := err.(exitError); ok {
			code = ee.code
		} else {
			code = -1
		}
	}
	return stdout.String(), stderr.String(), code
}

func writePerfFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPerfWcFastPaths(t *testing.T) {
	dir := t.TempDir()

	// 1. Regular file -c O(1) stat vs partial-read stdin offset vs pipe.
	payload := bytes.Repeat([]byte("alpha beta gamma\n"), 5000) // 85,000 bytes (> 64 KB)
	f1 := filepath.Join(dir, "big.txt")
	writePerfFile(t, f1, payload)
	writePerfFile(t, filepath.Join(dir, "empty.txt"), nil)

	out, _, code := runPerfScript(t, dir, "wc -c big.txt empty.txt")
	wantTotal := fmt.Sprintf("%d", len(payload))
	if code != 0 || !strings.Contains(out, wantTotal+" big.txt\n") || !strings.Contains(out, "total\n") {
		t.Fatalf("wc -c multiple files = %q (code %d)", out, code)
	}

	// Redirected stdin already positioned past byte 0 must count ONLY remaining bytes.
	out, _, code = runPerfScript(t, dir, "( head -c 17 >/dev/null; wc -c ) < big.txt")
	if code != 0 || strings.TrimSpace(out) != strconv.Itoa(len(payload)-17) {
		t.Fatalf("wc -c after head -c 17 = %q (code %d), want %d", out, code, len(payload)-17)
	}

	// Pipe -c and -lc fast paths.
	out, _, code = runPerfScript(t, dir, "cat big.txt | wc -lc")
	if code != 0 || strings.Fields(out)[0] != "5000" || strings.Fields(out)[1] != strconv.Itoa(len(payload)) {
		t.Fatalf("cat | wc -lc = %q (code %d)", out, code)
	}

	// 2. Chunk boundary (65536) split tests for ASCII words and multi-byte UTF-8 runes.
	const chunkSz = 64 * 1024
	// Case A: 3-byte UTF-8 non-space rune '€' (0xE2 0x82 0xAC) split across byte 65535..65537 inside one word.
	var buf bytes.Buffer
	prefixLen := chunkSz - 4 // "ab" + first 2 bytes of '€' land at 65534..65535, 3rd byte at 65536!
	buf.Write(bytes.Repeat([]byte("x "), prefixLen/2))
	for buf.Len() < prefixLen {
		buf.WriteByte(' ')
	}
	// At offset 65532: write " ab€cd\n" (1 word of 5 chars, 7 bytes + leading space + trailing \n).
	buf.Truncate(chunkSz - 4)
	buf.WriteByte(' ')
	buf.WriteString("ab\xe2\x82\xaccd\n")
	// Also test 3-byte UTF-8 EM SPACE U+2003 (\xe2\x80\x83) split across the next 64 KB boundary!
	for buf.Len() < 2*chunkSz-2 {
		buf.WriteByte('a')
	}
	// At 2*chunkSz - 2: write 'w', then 3-byte EM SPACE straddling 2*chunkSz, then "z\n".
	buf.WriteByte('w')
	buf.WriteString("\xe2\x80\x83z\n")

	utfPath := filepath.Join(dir, "utf_boundary.txt")
	writePerfFile(t, utfPath, buf.Bytes())

	// Reference counts computed purely in Go over the whole slice.
	wantBytes := buf.Len()
	wantLines := bytes.Count(buf.Bytes(), []byte{'\n'})
	wantChars := len([]rune(buf.String()))
	wantWords := len(strings.Fields(buf.String()))

	out, _, code = runPerfScript(t, dir, "wc utf_boundary.txt")
	if code != 0 {
		t.Fatalf("wc utf_boundary failed: code %d", code)
	}
	gotFields := strings.Fields(out)
	if len(gotFields) != 4 ||
		gotFields[0] != strconv.Itoa(wantLines) ||
		gotFields[1] != strconv.Itoa(wantWords) ||
		gotFields[2] != strconv.Itoa(wantBytes) {
		t.Fatalf("wc utf_boundary = %q; want %d %d %d", out, wantLines, wantWords, wantBytes)
	}

	outM, _, code := runPerfScript(t, dir, "wc -m utf_boundary.txt")
	if code != 0 || strings.Fields(outM)[0] != strconv.Itoa(wantChars) {
		t.Fatalf("wc -m utf_boundary = %q; want %d", outM, wantChars)
	}
}

func TestPerfGrepFastPaths(t *testing.T) {
	dir := t.TempDir()
	const chunkSz = 64 * 1024

	// Construct >150 KB file where "NEEDLE_TOKEN" straddles the 65536-byte chunk boundary,
	// plus a line with TWO occurrences (must count as 1 line in -c!).
	var b bytes.Buffer
	line := "filler_text_without_any_target_0123456789abcdef\n"
	for b.Len() < chunkSz-20 {
		b.WriteString(line)
	}
	pad := chunkSz - 6 - b.Len()
	if pad > 0 {
		b.WriteString(strings.Repeat("p", pad-1) + "\n")
	}
	// Now right near 65536: straddle NEEDLE_TOKEN across 65536!
	b.WriteString("prefix_NEEDLE_TOKEN_suffix\n")
	b.WriteString("double NEEDLE_TOKEN and NEEDLE_TOKEN again\n")
	for b.Len() < 2*chunkSz+5000 {
		b.WriteString(line)
	}
	b.WriteString("unterminated_NEEDLE_TOKEN_end")

	writePerfFile(t, filepath.Join(dir, "haystack.txt"), b.Bytes())

	// Implicit literal SIMD fast path (no -F passed): -c, -q, -l, plain, -n, -v.
	out, _, code := runPerfScript(t, dir, "grep -c NEEDLE_TOKEN haystack.txt")
	if code != 0 || out != "3\n" {
		t.Fatalf("grep -c = %q (code %d), want 3", out, code)
	}

	out, _, code = runPerfScript(t, dir, "grep -q NEEDLE_TOKEN haystack.txt")
	if code != 0 || out != "" {
		t.Fatalf("grep -q = %q (code %d)", out, code)
	}

	out, _, code = runPerfScript(t, dir, "grep -l NEEDLE_TOKEN haystack.txt")
	if code != 0 || out != "haystack.txt\n" {
		t.Fatalf("grep -l = %q (code %d)", out, code)
	}

	out, _, code = runPerfScript(t, dir, "grep NEEDLE_TOKEN haystack.txt")
	wantPlain := "prefix_NEEDLE_TOKEN_suffix\n" +
		"double NEEDLE_TOKEN and NEEDLE_TOKEN again\n" +
		"unterminated_NEEDLE_TOKEN_end\n"
	if code != 0 || out != wantPlain {
		t.Fatalf("grep plain = %q (code %d), want %q", out, code, wantPlain)
	}

	// Long >200 KB single line.
	longLine := strings.Repeat("a", 200*1024) + "FINDME" + strings.Repeat("b", 1024) + "\n"
	writePerfFile(t, filepath.Join(dir, "long.txt"), []byte(longLine))
	out, _, code = runPerfScript(t, dir, "grep -c FINDME long.txt")
	if code != 0 || out != "1\n" {
		t.Fatalf("grep long line -c = %q (code %d)", out, code)
	}

	// Regex escapes & metacharacters must NOT trigger literal fast path falsely.
	writePerfFile(t, filepath.Join(dir, "re.txt"), []byte("a.b\naxb\na+b\naab\n"))
	out, _, code = runPerfScript(t, dir, `grep 'a\.b' re.txt`)
	if code != 0 || out != "a.b\n" {
		t.Fatalf("grep escaped dot = %q (code %d), want a.b", out, code)
	}
	out, _, code = runPerfScript(t, dir, `grep '^axb$' re.txt`)
	if code != 0 || out != "axb\n" {
		t.Fatalf("grep anchored = %q (code %d), want axb", out, code)
	}

	// EPIPE termination in pipeline.
	out, _, code = runPerfScript(t, dir, "yes NEEDLE | grep NEEDLE | head -n 2")
	if code != 0 || out != "NEEDLE\nNEEDLE\n" {
		t.Fatalf("yes | grep | head = %q (code %d)", out, code)
	}
}

func TestPerfSedFastPaths(t *testing.T) {
	dir := t.TempDir()
	const chunkSz = 64 * 1024

	// Build >140 KB input with "aaa" straddling the 65536-byte chunk boundary.
	var b bytes.Buffer
	for b.Len() < chunkSz-10 {
		b.WriteString("0123456789_aaa_xyz_aaa_end\n")
	}
	rem := chunkSz - 1 - b.Len()
	if rem > 1 {
		b.WriteString(strings.Repeat("x", rem-1) + "\n")
	}
	// Straddle "aaa" right across 65536!
	b.WriteString("aaa_boundary_aaa\n")
	for b.Len() < 2*chunkSz+2048 {
		b.WriteString("line_aaa_middle\n")
	}
	orig := b.String()
	writePerfFile(t, filepath.Join(dir, "in.txt"), b.Bytes())

	// 1. Same-length global in-place SIMD fast path.
	out, _, code := runPerfScript(t, dir, "sed 's/aaa/zzz/g' in.txt")
	if code != 0 || out != strings.ReplaceAll(orig, "aaa", "zzz") {
		t.Fatalf("sed same-len global mismatch (code %d, len %d vs %d)", code, len(out), len(orig))
	}

	// 2. First-match-only (non-global) same-length & different-length.
	writePerfFile(t, filepath.Join(dir, "small.txt"), []byte("aaa_1_aaa_2\nno_match\naaa\n"))
	out, _, code = runPerfScript(t, dir, "sed 's/aaa/Z/' small.txt")
	if code != 0 || out != "Z_1_aaa_2\nno_match\nZ\n" {
		t.Fatalf("sed non-global = %q (code %d)", out, code)
	}

	// 3. Expanding, deleting, and '&' replacement.
	out, _, code = runPerfScript(t, dir, "sed 's/aaa/<<<<&>>>>/g' small.txt")
	if code != 0 || out != "<<<<aaa>>>>_1_<<<<aaa>>>>_2\nno_match\n<<<<aaa>>>>\n" {
		t.Fatalf("sed ampersand repl = %q (code %d)", out, code)
	}
	out, _, code = runPerfScript(t, dir, "sed 's/aaa//g' small.txt")
	if code != 0 || out != "_1__2\nno_match\n\n" {
		t.Fatalf("sed empty repl = %q (code %d)", out, code)
	}

	// 4. Multi-op literal substitution chain (Mode 2 dual scratch buffer).
	out, _, code = runPerfScript(t, dir, "sed -e 's/aaa/bbb/g' -e 's/bbb/cc/' -e 's/cc/[&]/g' small.txt")
	if code != 0 || out != "[cc]_1_bbb_2\nno_match\n[cc]\n" {
		t.Fatalf("sed multi-op chain = %q (code %d)", out, code)
	}

	// 5. Multi-file unterminated line + missing file parity.
	writePerfFile(t, filepath.Join(dir, "u1.txt"), []byte("aa"))
	writePerfFile(t, filepath.Join(dir, "u2.txt"), []byte("a\n"))
	writePerfFile(t, filepath.Join(dir, "empty.txt"), nil)

	// "aa" (no \n) followed by "a\n" must emit "aa\na\n" and NOT merge into "aaa"!
	out, _, code = runPerfScript(t, dir, "sed 's/aaa/XXX/g' u1.txt u2.txt")
	if code != 0 || out != "aa\na\n" {
		t.Fatalf("sed multi-file boundary = %q (code %d), want %q", out, code, "aa\na\n")
	}
	// "aa" (no \n) followed by empty.txt must stay "aa" WITHOUT trailing \n!
	out, _, code = runPerfScript(t, dir, "sed 's/aa/BB/g' u1.txt empty.txt")
	if code != 0 || out != "BB" {
		t.Fatalf("sed unterminated + empty = %q (code %d), want %q", out, code, "BB")
	}
	// Missing file among valid files sets exit code 1 while still processing remaining files.
	out, errOut, code := runPerfScript(t, dir, "sed 's/aa/BB/g' u1.txt missing_nope.txt u2.txt")
	if code != 1 || out != "BB\na\n" || errOut == "" {
		t.Fatalf("sed missing file = %q, err=%q, code=%d", out, errOut, code)
	}

	// 6. Regex escapes fall back to full engine cleanly.
	writePerfFile(t, filepath.Join(dir, "dots.txt"), []byte("a.b aXb\n"))
	out, _, code = runPerfScript(t, dir, `sed 's/a\.b/HIT/g' dots.txt`)
	if code != 0 || out != "HIT aXb\n" {
		t.Fatalf("sed escaped dot = %q (code %d)", out, code)
	}
}

func TestPerfSortAndUniqFastPaths(t *testing.T) {
	dir := t.TempDir()

	// 1. Whole-line sortSpan (8-byte prefix + full tie-break + -u + -r + -z).
	lines := strings.Join([]string{
		"shared8_zzz",
		"shared8_aaa",
		"shared8_aaa",
		"short",
		"",
		"exact8ch",
		"shared8_",
	}, "\n") + "\n"
	writePerfFile(t, filepath.Join(dir, "spans.txt"), []byte(lines))

	out, _, code := runPerfScript(t, dir, "sort spans.txt")
	want := "\nexact8ch\nshared8_\nshared8_aaa\nshared8_aaa\nshared8_zzz\nshort\n"
	if code != 0 || out != want {
		t.Fatalf("sort spans = %q (code %d), want %q", out, code, want)
	}

	out, _, code = runPerfScript(t, dir, "sort -u spans.txt")
	wantU := "\nexact8ch\nshared8_\nshared8_aaa\nshared8_zzz\nshort\n"
	if code != 0 || out != wantU {
		t.Fatalf("sort -u spans = %q (code %d), want %q", out, code, wantU)
	}

	out, _, code = runPerfScript(t, dir, "sort -r -u spans.txt")
	wantRU := "short\nshared8_zzz\nshared8_aaa\nshared8_\nexact8ch\n\n"
	if code != 0 || out != wantRU {
		t.Fatalf("sort -ru spans = %q (code %d), want %q", out, code, wantRU)
	}

	// 2. Schwartzian single-key numeric sort: negatives, -0 vs 0, decimals, >20-digit ints,
	// shared 4-byte line prefixes for last-resort tie-break, -s stable, and -u key dedup.
	numInput := strings.Join([]string{
		"same_z 10",
		"same_a 10",
		"beta 10.5",
		"alpha 10.05",
		"neg -100",
		"neg2 -2",
		"zero_b 0",
		"zero_a -0",
		"nonnum abc",
		"huge2 99999999999999999999999",
		"huge1 99999999999999999999998",
	}, "\n") + "\n"
	writePerfFile(t, filepath.Join(dir, "nums.txt"), []byte(numInput))

	out, _, code = runPerfScript(t, dir, "sort -k2,2n nums.txt")
	wantNum := strings.Join([]string{
		"neg -100",
		"neg2 -2",
		"nonnum abc",
		"zero_a -0",
		"zero_b 0",
		"same_a 10",
		"same_z 10",
		"alpha 10.05",
		"beta 10.5",
		"huge1 99999999999999999999998",
		"huge2 99999999999999999999999",
	}, "\n") + "\n"
	if code != 0 || out != wantNum {
		t.Fatalf("sort -k2,2n =\n%s\nwant:\n%s", out, wantNum)
	}

	// Stable (-s) keeps original input order on equal numeric keys (0 == -0 == abc, 10 == 10).
	out, _, code = runPerfScript(t, dir, "sort -k2,2n -s nums.txt")
	wantStable := strings.Join([]string{
		"neg -100",
		"neg2 -2",
		"zero_b 0",
		"zero_a -0",
		"nonnum abc",
		"same_z 10",
		"same_a 10",
		"alpha 10.05",
		"beta 10.5",
		"huge1 99999999999999999999998",
		"huge2 99999999999999999999999",
	}, "\n") + "\n"
	if code != 0 || out != wantStable {
		t.Fatalf("sort -k2,2n -s =\n%s\nwant:\n%s", out, wantStable)
	}

	// Human-numeric (-h) and general-numeric (-g) and byte-key (-t: -k2,2).
	writePerfFile(t, filepath.Join(dir, "human.txt"), []byte("a 2M\nb 500\nc 1K\nd 1.5G\n"))
	out, _, code = runPerfScript(t, dir, "sort -k2,2h human.txt")
	if code != 0 || out != "b 500\nc 1K\na 2M\nd 1.5G\n" {
		t.Fatalf("sort -k2,2h = %q (code %d)", out, code)
	}

	writePerfFile(t, filepath.Join(dir, "colon.txt"), []byte("x:shared8_b:1\ny:shared8_a:2\nz:alpha:3\n"))
	out, _, code = runPerfScript(t, dir, "sort -t: -k2,2 colon.txt")
	if code != 0 || out != "z:alpha:3\ny:shared8_a:2\nx:shared8_b:1\n" {
		t.Fatalf("sort -t: -k2,2 = %q (code %d)", out, code)
	}

	// 3. uniq fast byte-streaming path (-c, -d, -u).
	writePerfFile(t, filepath.Join(dir, "uniq.txt"), []byte("aa\naa\naa\nbb\ncc\ncc\n"))
	out, _, code = runPerfScript(t, dir, "uniq -c uniq.txt")
	if code != 0 || out != "      3 aa\n      1 bb\n      2 cc\n" {
		t.Fatalf("uniq -c = %q (code %d)", out, code)
	}
	out, _, code = runPerfScript(t, dir, "uniq -d uniq.txt")
	if code != 0 || out != "aa\ncc\n" {
		t.Fatalf("uniq -d = %q (code %d)", out, code)
	}
	out, _, code = runPerfScript(t, dir, "uniq -u uniq.txt")
	if code != 0 || out != "bb\n" {
		t.Fatalf("uniq -u = %q (code %d)", out, code)
	}
}

func TestPerfCutTrSeqHeadFastPaths(t *testing.T) {
	dir := t.TempDir()

	// cut single-byte delimiter fast path + -s + >64 fields.
	var wide strings.Builder
	for i := 1; i <= 70; i++ {
		if i > 1 {
			wide.WriteByte(',')
		}
		wide.WriteString(fmt.Sprintf("f%d", i))
	}
	wide.WriteString("\nno_comma_line\n")
	writePerfFile(t, filepath.Join(dir, "wide.csv"), []byte(wide.String()))

	out, _, code := runPerfScript(t, dir, "cut -s -d, -f2,70 --output-delimiter='|' wide.csv")
	if code != 0 || out != "f2|f70\n" {
		t.Fatalf("cut -s -d, -f2,70 = %q (code %d)", out, code)
	}

	// tr single-buffer 8x table fast path, -d, and in-place -s squeeze.
	trIn := bytes.Repeat([]byte("hello   world\r\n"), 6000)
	writePerfFile(t, filepath.Join(dir, "tr.txt"), trIn)
	out, _, code = runPerfScript(t, dir, `tr -d '\r' < tr.txt | tr -s ' ' | tr a-z A-Z | head -n 2`)
	if code != 0 || out != "HELLO WORLD\nHELLO WORLD\n" {
		t.Fatalf("tr pipeline = %q (code %d)", out, code)
	}

	// seq integer fast path.
	out, _, code = runPerfScript(t, dir, "seq 100000 | wc -l")
	if code != 0 || strings.TrimSpace(out) != "100000" {
		t.Fatalf("seq 100000 | wc -l = %q (code %d)", out, code)
	}
	out, _, code = runPerfScript(t, dir, "seq -s : 10 -3 1")
	if code != 0 || out != "10:7:4:1\n" {
		t.Fatalf("seq -s : 10 -3 1 = %q (code %d)", out, code)
	}

	// head 64 KB chunked fast path + seekable stdin rewind!
	var seqBuf bytes.Buffer
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&seqBuf, "line_%04d_padding_for_multi_chunk_test\n", i)
	}
	writePerfFile(t, filepath.Join(dir, "lines.txt"), seqBuf.Bytes())

	out, _, code = runPerfScript(t, dir, "( head -n 2; head -n 2 ) < lines.txt")
	wantHead := "line_0001_padding_for_multi_chunk_test\n" +
		"line_0002_padding_for_multi_chunk_test\n" +
		"line_0003_padding_for_multi_chunk_test\n" +
		"line_0004_padding_for_multi_chunk_test\n"
	if code != 0 || out != wantHead {
		t.Fatalf("head seekable rewind = %q (code %d), want %q", out, code, wantHead)
	}
}

func TestPerfFindAndTarFastPaths(t *testing.T) {
	dir := t.TempDir()

	// Build directory tree with empty, small, and >100 KB files.
	writePerfFile(t, filepath.Join(dir, "tree", "a", "one.txt"), []byte("hello\n"))
	writePerfFile(t, filepath.Join(dir, "tree", "a", "empty.txt"), nil)
	writePerfFile(t, filepath.Join(dir, "tree", "b", "skip.log"), []byte("ignore\n"))
	bigData := bytes.Repeat([]byte("tar_chunk_payload_0123456789\n"), 4000) // ~116 KB (> 64 KB copyBuf)
	writePerfFile(t, filepath.Join(dir, "tree", "b", "big.bin"), bigData)

	// find -type f / -type d / -name / -empty without per-file Lstat.
	out, _, code := runPerfScript(t, dir, "find tree -type f -name '*.txt' | sort")
	if code != 0 || out != "tree/a/empty.txt\ntree/a/one.txt\n" {
		t.Fatalf("find -type f -name = %q (code %d)", out, code)
	}
	out, _, code = runPerfScript(t, dir, "find tree -type f -empty")
	if code != 0 || out != "tree/a/empty.txt\n" {
		t.Fatalf("find -empty = %q (code %d)", out, code)
	}

	// tar -cf with --exclude + tar -xf round-trip verification of 0-byte and >64 KB files.
	out, errOut, code := runPerfScript(t, dir, "tar -cf out.tar --exclude='*.log' tree && mkdir extracted && tar -xf out.tar -C extracted")
	if code != 0 {
		t.Fatalf("tar round-trip failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "extracted", "tree", "b", "skip.log")); !os.IsNotExist(err) {
		t.Fatalf("expected excluded skip.log to be absent, got err=%v", err)
	}
	gotEmpty, err := os.ReadFile(filepath.Join(dir, "extracted", "tree", "a", "empty.txt"))
	if err != nil || len(gotEmpty) != 0 {
		t.Fatalf("extracted empty.txt: len=%d err=%v", len(gotEmpty), err)
	}
	gotBig, err := os.ReadFile(filepath.Join(dir, "extracted", "tree", "b", "big.bin"))
	if err != nil || !bytes.Equal(gotBig, bigData) {
		t.Fatalf("extracted big.bin mismatch: len=%d (want %d) err=%v", len(gotBig), len(bigData), err)
	}

	// tar -cf self.tar . inside directory must skip self.tar and keep clean entry names.
	subDir := filepath.Join(dir, "tree", "a")
	_, errOut, code = runPerfScript(t, subDir, "tar -cf self.tar . && tar -tf self.tar | sort")
	if code != 0 {
		t.Fatalf("tar self.tar failed: code=%d err=%q", code, errOut)
	}
}

func TestPerfConcurrentPoolSafety(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("alpha,100,needle_aaa\nbeta,20,other_bbb\n"), 1500)
	writePerfFile(t, filepath.Join(dir, "input.txt"), data)

	scripts := []string{
		"wc input.txt",
		"grep -c needle input.txt",
		"sed 's/aaa/zzz/g' input.txt | wc -c",
		"sort -t, -k2,2n input.txt | head -n 5",
		"cut -d, -f1,3 input.txt | tr a-z A-Z | uniq | wc -l",
		"seq 5000 | tail -n 1",
		"find . -type f",
	}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				sc := scripts[(id+i)%len(scripts)]
				_, errOut, code := runPerfScript(t, dir, sc)
				if code != 0 {
					t.Errorf("goroutine %d script %q failed: code=%d err=%q", id, sc, code, errOut)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// --- Go Benchmarks (in-process handler throughput & allocations) -----------

func benchHC(dir string, stdin io.Reader, stdout io.Writer) interp.HandlerContext {
	return interp.HandlerContext{
		Dir:    dir,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: io.Discard,
	}
}

func BenchmarkPerfCoreutils_Seq100k(b *testing.B) {
	ctx := context.Background()
	hc := benchHC(".", bytes.NewReader(nil), io.Discard)
	args := []string{"seq", "100000"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cmdSeq(ctx, hc, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfCoreutils_WcDefault4M(b *testing.B) {
	dir := b.TempDir()
	payload := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog 1234567890\n"), 75000) // ~4.1 MB
	p := filepath.Join(dir, "big.txt")
	writePerfFile(b, p, payload)
	ctx := context.Background()
	hc := benchHC(dir, bytes.NewReader(nil), io.Discard)
	args := []string{"wc", "big.txt"}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cmdWc(ctx, hc, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfCoreutils_GrepLiteralCount4M(b *testing.B) {
	dir := b.TempDir()
	var buf bytes.Buffer
	for i := 0; i < 80000; i++ {
		if i%97 == 0 {
			buf.WriteString("prefix_data_needle_match_token_suffix_0123456789\n")
		} else {
			buf.WriteString("ordinary_log_entry_without_any_hit_0123456789abc\n")
		}
	}
	p := filepath.Join(dir, "big.txt")
	writePerfFile(b, p, buf.Bytes())
	ctx := context.Background()
	hc := benchHC(dir, bytes.NewReader(nil), io.Discard)
	args := []string{"grep", "-c", "needle_match", "big.txt"}
	b.SetBytes(int64(buf.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cmdGrep(ctx, hc, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfCoreutils_SedLiteralGlobal4M(b *testing.B) {
	dir := b.TempDir()
	payload := bytes.Repeat([]byte("alpha_aaa_beta_aaa_gamma_0123456789abcdef\n"), 95000) // ~4 MB
	p := filepath.Join(dir, "big.txt")
	writePerfFile(b, p, payload)
	ctx := context.Background()
	hc := benchHC(dir, bytes.NewReader(nil), io.Discard)
	args := []string{"sed", "s/aaa/zzz/g", "big.txt"}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cmdSed(ctx, hc, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfCoreutils_SortKeyNumeric50k(b *testing.B) {
	dir := b.TempDir()
	var buf bytes.Buffer
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&buf, "item_%05d %d tail\n", (i*7919)%50000, (i*104729)%100000-25000)
	}
	p := filepath.Join(dir, "pairs.txt")
	writePerfFile(b, p, buf.Bytes())
	ctx := context.Background()
	hc := benchHC(dir, bytes.NewReader(nil), io.Discard)
	args := []string{"sort", "-k", "2,2n", "pairs.txt"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cmdSort(ctx, hc, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfCoreutils_CutAndTr4M(b *testing.B) {
	payload := bytes.Repeat([]byte("col1,col2_target_value,col3,col4_extra\n"), 100000)
	ctx := context.Background()
	var pipeBuf bytes.Buffer
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pipeBuf.Reset()
		hc1 := benchHC(".", bytes.NewReader(payload), &pipeBuf)
		if err := cmdCut(ctx, hc1, []string{"cut", "-d", ",", "-f", "2"}); err != nil {
			b.Fatal(err)
		}
		hc2 := benchHC(".", bytes.NewReader(pipeBuf.Bytes()), io.Discard)
		if err := cmdTr(ctx, hc2, []string{"tr", "a-z", "A-Z"}); err != nil {
			b.Fatal(err)
		}
	}
}
