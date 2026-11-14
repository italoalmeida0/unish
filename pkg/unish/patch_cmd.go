package unish

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

type patchOptions struct {
	strip            int // -1 when not specified
	inputFile        string
	outputFile       string
	hasOutput        bool
	workDir          string
	rejectFile       string
	reverse          bool
	forward          bool
	dryRun           bool
	silent           bool
	removeEmptyFiles bool
	backup           bool
	ignoreWhitespace bool
	maxFuzz          int
}

type patchLine struct {
	op   byte // ' ', '-', '+'
	text string
}

type patchHunk struct {
	header     string
	oldStart   int
	oldCount   int
	newStart   int
	newCount   int
	lines      []patchLine
	oldNoEOFNL bool
	newNoEOFNL bool
}

type filePatch struct {
	oldPath       string
	newPath       string
	gitOldPath    string
	gitNewPath    string
	isGitDiff     bool
	isNewFile     bool
	isDeletedFile bool
	newMode       os.FileMode
	hunks         []patchHunk
}

func cmdPatch(ctx context.Context, hc interp.HandlerContext, args []string) error {
	opts := patchOptions{
		strip:   -1,
		maxFuzz: 2,
	}
	positionals, err := patchOpts.parse(hc.Stderr, args, func(idx int, val string) error {
		n := 0
		if idx == 0 || idx == 5 {
			var perr error
			if n, perr = strconv.Atoi(val); perr != nil || n < 0 {
				return toolBadArg(hc.Stderr, "patch", patchOpts.opts[idx].long, val)
			}
		}
		switch idx {
		case 0:
			opts.strip = n
		case 1:
			opts.inputFile = val
		case 2:
			opts.outputFile, opts.hasOutput = val, true
		case 3:
			opts.workDir = val
		case 4:
			opts.rejectFile = val
		case 5:
			opts.maxFuzz = n
		case 6:
			opts.reverse = true
		case 7:
			opts.forward = true
		case 8:
			opts.dryRun = true
		case 9:
			opts.silent = true
		case 10:
			opts.removeEmptyFiles = true
		case 11:
			opts.backup = true
		case 12:
			opts.ignoreWhitespace = true
		case 14:
			fmt.Fprintln(hc.Stdout, "Usage: patch [OPTIONS] [ORIGFILE [PATCHFILE]]")
			return errToolHelp
		}
		return nil
	})
	if err != nil {
		return toolOptErr(err)
	}
	if len(positionals) > 2 {
		fmt.Fprintln(hc.Stderr, "patch: **** extra operand")
		return exitError{2}
	}

	origTargetArg := ""
	if len(positionals) >= 1 {
		origTargetArg = positionals[0]
	}
	if len(positionals) == 2 && opts.inputFile == "" {
		opts.inputFile = positionals[1]
	}

	baseDir := hc.Dir
	if opts.workDir != "" {
		baseDir = resolve(hc.Dir, opts.workDir)
		fi, serr := os.Stat(baseDir)
		if serr != nil || !fi.IsDir() {
			if serr != nil {
				fmt.Fprintf(hc.Stderr, "patch: **** Can't change to directory %s : %v\n", opts.workDir, serr)
			} else {
				fmt.Fprintf(hc.Stderr, "patch: **** %s is not a directory\n", opts.workDir)
			}
			return exitError{2}
		}
	}

	// Read patch input (stdin by default, or -i / positional patchfile).
	var patchData []byte
	if opts.inputFile == "" || opts.inputFile == "-" || opts.inputFile == "/dev/stdin" {
		patchData, err = readAllCtx(ctx, hc.Stdin)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(hc.Stderr, "patch: **** read error: %v\n", err)
			return exitError{2}
		}
	} else if isToolDevNull(opts.inputFile) {
		patchData = nil
	} else {
		pPath := resolve(baseDir, opts.inputFile)
		if _, serr := os.Stat(pPath); serr != nil && opts.workDir != "" {
			if alt := resolve(hc.Dir, opts.inputFile); alt != pPath {
				if _, serr2 := os.Stat(alt); serr2 == nil {
					pPath = alt
				}
			}
		}
		patchData, err = readShellFile(pPath)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "patch: **** Can't open patch file %s : %v\n", opts.inputFile, err)
			return exitError{2}
		}
	}

	if len(bytes.TrimSpace(patchData)) == 0 {
		return nil
	}

	patches, err := parseUnifiedDiff(patchData)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "patch: **** %v\n", err)
		return exitError{2}
	}
	if len(patches) == 0 {
		fmt.Fprintln(hc.Stderr, "patch: **** Only garbage was found in the patch input.")
		return exitError{2}
	}

	exitStatus := 0
	for idx := range patches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fp := patches[idx]
		if opts.reverse {
			fp = reverseFilePatch(fp)
		}

		displayRel, targetAbs, rerr := resolvePatchTarget(&fp, origTargetArg, baseDir, opts.strip)
		if rerr != nil {
			fmt.Fprintf(hc.Stderr, "patch: **** %v\n", rerr)
			return exitError{2}
		}

		st, aerr := applySingleFilePatch(ctx, hc, &fp, displayRel, targetAbs, baseDir, &opts)
		if aerr != nil {
			if isPipeClosed(aerr) {
				return aerr
			}
			fmt.Fprintf(hc.Stderr, "patch: **** %v\n", aerr)
			return exitError{2}
		}
		if st > exitStatus {
			exitStatus = st
		}
	}

	if exitStatus != 0 {
		return exitError{exitStatus}
	}
	return nil
}

var patchOpts = toolOptParser{tool: "patch", opts: []toolOpt{
	{"p", "strip", true},               // 0
	{"i", "input", true},               // 1
	{"o", "output", true},              // 2
	{"d", "directory", true},           // 3
	{"r", "reject-file", true},         // 4
	{"F", "fuzz", true},                // 5
	{"R", "reverse", false},            // 6
	{"N", "forward", false},            // 7
	{"", "dry-run check", false},       // 8
	{"s", "silent quiet", false},       // 9
	{"E", "remove-empty-files", false}, // 10
	{"b", "backup", false},             // 11
	{"l", "ignore-whitespace", false},  // 12
	{"uft", "no-backup-if-mismatch backup-if-mismatch unified force batch posix binary", false}, // 13: accepted
	{"h", "help", false}, // 14
}}

// ---- Unified Diff Parser ----

func parseUnifiedDiff(data []byte) ([]filePatch, error) {
	rawLines := strings.Split(string(data), "\n")
	// Drop single final empty element after trailing \n.
	if len(rawLines) > 0 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}
	lines := make([]string, len(rawLines))
	for i, l := range rawLines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}

	var patches []filePatch
	var cur *filePatch

	flushCur := func() {
		if cur != nil && (len(cur.hunks) > 0 || cur.isNewFile || cur.isDeletedFile) {
			patches = append(patches, *cur)
		}
		cur = nil
	}

	i := 0
	for i < len(lines) {
		line := lines[i]

		if strings.HasPrefix(line, "diff --git ") {
			flushCur()
			oldP, newP := parseGitDiffHeaderLine(line[len("diff --git "):])
			cur = &filePatch{
				gitOldPath: oldP,
				gitNewPath: newP,
				oldPath:    oldP,
				newPath:    newP,
				isGitDiff:  true,
			}
			i++
			// Parse extended git diff headers before --- / +++ or next diff --git
			for i < len(lines) {
				gl := lines[i]
				if strings.HasPrefix(gl, "diff --git ") || strings.HasPrefix(gl, "--- ") || strings.HasPrefix(gl, "@@ ") {
					break
				}
				if strings.HasPrefix(gl, "new file mode ") {
					cur.isNewFile = true
					cur.oldPath = "/dev/null"
					if m, err := strconv.ParseUint(strings.TrimSpace(gl[len("new file mode "):]), 8, 32); err == nil {
						cur.newMode = os.FileMode(m) & 0o777
					}
				} else if strings.HasPrefix(gl, "deleted file mode ") {
					cur.isDeletedFile = true
					cur.newPath = "/dev/null"
				} else if strings.HasPrefix(gl, "rename from ") {
					cur.oldPath = unquotePatchPath(strings.TrimSpace(gl[len("rename from "):]))
				} else if strings.HasPrefix(gl, "rename to ") {
					cur.newPath = unquotePatchPath(strings.TrimSpace(gl[len("rename to "):]))
				}
				i++
			}
			continue
		}

		if strings.HasPrefix(line, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
			if cur == nil || len(cur.hunks) > 0 {
				flushCur()
				cur = &filePatch{}
			}
			cur.oldPath = parsePatchHeaderPath(line[4:])
			cur.newPath = parsePatchHeaderPath(lines[i+1][4:])
			if cur.oldPath == "/dev/null" {
				cur.isNewFile = true
			}
			if cur.newPath == "/dev/null" {
				cur.isDeletedFile = true
			}
			i += 2
			// Read all @@ hunks belonging to this file
			for i < len(lines) && strings.HasPrefix(lines[i], "@@ ") {
				h, nextIdx, err := parseOneHunk(lines, i)
				if err != nil {
					return nil, err
				}
				cur.hunks = append(cur.hunks, h)
				i = nextIdx
			}
			continue
		}

		if strings.HasPrefix(line, "@@ ") && cur != nil {
			h, nextIdx, err := parseOneHunk(lines, i)
			if err != nil {
				return nil, err
			}
			cur.hunks = append(cur.hunks, h)
			i = nextIdx
			continue
		}

		i++
	}
	flushCur()
	return patches, nil
}

func parseGitDiffHeaderLine(rest string) (string, string) {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "\"") {
		p1, rem := parseQuotedToken(rest)
		rem = strings.TrimSpace(rem)
		p2, _ := parseQuotedToken(rem)
		return p1, p2
	}
	// Standard unquoted "a/path b/path": look for " b/" split if present, else space split.
	if idx := strings.Index(rest, " b/"); idx > 0 {
		return rest[:idx], rest[idx+1:]
	}
	parts := strings.Fields(rest)
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return rest, rest
}

func parseQuotedToken(s string) (string, string) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "\"") {
		if sp := strings.IndexByte(s, ' '); sp >= 0 {
			return s[:sp], s[sp+1:]
		}
		return s, ""
	}
	var raw []byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return string(raw), s[i+1:]
		}
		if c == '\\' && i+1 < len(s) {
			i++
			esc := s[i]
			switch esc {
			case 'n':
				raw = append(raw, '\n')
			case 't':
				raw = append(raw, '\t')
			case 'r':
				raw = append(raw, '\r')
			case '\\', '"':
				raw = append(raw, esc)
			default:
				if esc >= '0' && esc <= '7' {
					end := i + 1
					for end < len(s) && end < i+3 && s[end] >= '0' && s[end] <= '7' {
						end++
					}
					if v, err := strconv.ParseUint(s[i:end], 8, 8); err == nil {
						raw = append(raw, byte(v))
						i = end - 1
						continue
					}
				}
				raw = append(raw, esc)
			}
			continue
		}
		raw = append(raw, c)
	}
	return string(raw), ""
}

func unquotePatchPath(s string) string {
	if strings.HasPrefix(s, "\"") {
		p, _ := parseQuotedToken(s)
		return p
	}
	return s
}

func parsePatchHeaderPath(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "\"") {
		p, _ := parseQuotedToken(s)
		return p
	}
	// Tab separates filename from timestamp in unified diffs (`--- a/foo.go\t2026-...`).
	if tab := strings.IndexByte(s, '\t'); tab >= 0 {
		s = s[:tab]
	} else if sp := findTimestampSplit(s); sp > 0 {
		s = s[:sp]
	}
	return strings.TrimSpace(s)
}

func findTimestampSplit(s string) int {
	// Handle space-padded timestamps like `--- a/foo.txt  2026-01-01 00:00:00`
	idx := strings.Index(s, "  ")
	if idx > 0 {
		tail := strings.TrimSpace(s[idx:])
		if len(tail) >= 4 && tail[0] >= '0' && tail[0] <= '9' {
			return idx
		}
	}
	return -1
}

func parseOneHunk(lines []string, startIdx int) (patchHunk, int, error) {
	hdr := lines[startIdx]
	endAt := strings.Index(hdr[3:], " @@")
	if endAt < 0 {
		// Also accept "@@ -1,2 +1,2@@"
		endAt = strings.Index(hdr[3:], "@@")
		if endAt < 0 {
			return patchHunk{}, startIdx + 1, fmt.Errorf("malformed hunk header: %s", hdr)
		}
	}
	rangeSpec := strings.TrimSpace(hdr[3 : 3+endAt])
	fields := strings.Fields(rangeSpec)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return patchHunk{}, startIdx + 1, fmt.Errorf("malformed hunk range: %s", hdr)
	}
	oldStart, oldCount, err := parseHunkRange(fields[0][1:])
	if err != nil {
		return patchHunk{}, startIdx + 1, fmt.Errorf("malformed old range in %s: %v", hdr, err)
	}
	newStart, newCount, err := parseHunkRange(fields[1][1:])
	if err != nil {
		return patchHunk{}, startIdx + 1, fmt.Errorf("malformed new range in %s: %v", hdr, err)
	}

	h := patchHunk{
		header:   hdr,
		oldStart: oldStart,
		oldCount: oldCount,
		newStart: newStart,
		newCount: newCount,
	}

	remOld := oldCount
	remNew := newCount
	i := startIdx + 1
	var lastOp byte

	for i < len(lines) && (remOld > 0 || remNew > 0) {
		l := lines[i]
		if strings.HasPrefix(l, `\`) {
			// "\ No newline at end of file"
			switch lastOp {
			case '-':
				h.oldNoEOFNL = true
			case '+':
				h.newNoEOFNL = true
			case ' ':
				h.oldNoEOFNL = true
				h.newNoEOFNL = true
			}
			i++
			continue
		}
		if l == "" {
			// Editor-stripped empty context line inside hunk
			if remOld > 0 && remNew > 0 {
				h.lines = append(h.lines, patchLine{op: ' ', text: ""})
				remOld--
				remNew--
				lastOp = ' '
				i++
				continue
			}
			break
		}
		switch l[0] {
		case ' ':
			if remOld <= 0 || remNew <= 0 {
				return patchHunk{}, i, fmt.Errorf("unexpected context line in hunk at line %d", i+1)
			}
			h.lines = append(h.lines, patchLine{op: ' ', text: l[1:]})
			remOld--
			remNew--
			lastOp = ' '
			i++
		case '-':
			if remOld <= 0 {
				return patchHunk{}, i, fmt.Errorf("unexpected '-' line in hunk at line %d", i+1)
			}
			h.lines = append(h.lines, patchLine{op: '-', text: l[1:]})
			remOld--
			lastOp = '-'
			i++
		case '+':
			if remNew <= 0 {
				return patchHunk{}, i, fmt.Errorf("unexpected '+' line in hunk at line %d", i+1)
			}
			h.lines = append(h.lines, patchLine{op: '+', text: l[1:]})
			remNew--
			lastOp = '+'
			i++
		default:
			return patchHunk{}, i, fmt.Errorf("malformed patch at line %d: %s", i+1, l)
		}
	}

	if remOld != 0 || remNew != 0 {
		return patchHunk{}, i, fmt.Errorf("premature end of hunk %s (missing %d old, %d new lines)", hdr, remOld, remNew)
	}

	// Check if immediately following line is `\ No newline at end of file` for the final line of the hunk!
	for i < len(lines) && strings.HasPrefix(lines[i], `\`) {
		switch lastOp {
		case '-':
			h.oldNoEOFNL = true
		case '+':
			h.newNoEOFNL = true
		case ' ':
			h.oldNoEOFNL = true
			h.newNoEOFNL = true
		}
		i++
	}

	return h, i, nil
}

func parseHunkRange(spec string) (start, count int, err error) {
	if comma := strings.IndexByte(spec, ','); comma >= 0 {
		start, err = strconv.Atoi(spec[:comma])
		if err != nil {
			return 0, 0, err
		}
		count, err = strconv.Atoi(spec[comma+1:])
		if err != nil {
			return 0, 0, err
		}
		return start, count, nil
	}
	start, err = strconv.Atoi(spec)
	if err != nil {
		return 0, 0, err
	}
	return start, 1, nil
}

func reverseFilePatch(fp filePatch) filePatch {
	rev := filePatch{
		oldPath:       fp.newPath,
		newPath:       fp.oldPath,
		gitOldPath:    fp.gitNewPath,
		gitNewPath:    fp.gitOldPath,
		isGitDiff:     fp.isGitDiff,
		isNewFile:     fp.isDeletedFile,
		isDeletedFile: fp.isNewFile,
		newMode:       fp.newMode,
		hunks:         make([]patchHunk, len(fp.hunks)),
	}
	for i, h := range fp.hunks {
		rev.hunks[i] = reverseHunk(h)
	}
	return rev
}

func reverseHunk(h patchHunk) patchHunk {
	rh := patchHunk{
		header:     h.header,
		oldStart:   h.newStart,
		oldCount:   h.newCount,
		newStart:   h.oldStart,
		newCount:   h.oldCount,
		lines:      make([]patchLine, len(h.lines)),
		oldNoEOFNL: h.newNoEOFNL,
		newNoEOFNL: h.oldNoEOFNL,
	}
	for i, pl := range h.lines {
		op := pl.op
		if op == '-' {
			op = '+'
		} else if op == '+' {
			op = '-'
		}
		rh.lines[i] = patchLine{op: op, text: pl.text}
	}
	return rh
}

// ---- Path Stripping & Target Resolution ----

func stripPatchPath(raw string, strip int) (string, bool) {
	if raw == "" || raw == "/dev/null" || raw == "dev/null" {
		return "/dev/null", true
	}
	s := strings.ReplaceAll(raw, `\`, "/")
	if strip < 0 {
		return s, true
	}
	if strip == 0 {
		return s, true
	}
	slashes := 0
	pos := 0
	for pos < len(s) && slashes < strip {
		idx := strings.IndexByte(s[pos:], '/')
		if idx < 0 {
			return "", false
		}
		pos += idx + 1
		for pos < len(s) && s[pos] == '/' {
			pos++
		}
		slashes++
	}
	if slashes < strip || pos >= len(s) {
		return "", false
	}
	return s[pos:], true
}

func resolvePatchTarget(fp *filePatch, explicitTarget, baseDir string, strip int) (displayRel, targetAbs string, err error) {
	if explicitTarget != "" {
		return explicitTarget, resolve(baseDir, explicitTarget), nil
	}

	candidates := []string{}
	if !fp.isNewFile && fp.oldPath != "" && fp.oldPath != "/dev/null" {
		candidates = append(candidates, fp.oldPath)
	}
	if !fp.isDeletedFile && fp.newPath != "" && fp.newPath != "/dev/null" {
		candidates = append(candidates, fp.newPath)
	}
	if fp.isNewFile && fp.newPath != "" && fp.newPath != "/dev/null" {
		candidates = []string{fp.newPath}
	} else if fp.isDeletedFile && fp.oldPath != "" && fp.oldPath != "/dev/null" {
		candidates = []string{fp.oldPath}
	}
	if len(candidates) == 0 && fp.gitNewPath != "" {
		candidates = append(candidates, fp.gitNewPath)
	}

	if strip >= 0 {
		var stripped []string
		for _, c := range candidates {
			if sp, ok := stripPatchPath(c, strip); ok && sp != "/dev/null" {
				stripped = append(stripped, sp)
			}
		}
		if len(stripped) == 0 {
			return "", "", fmt.Errorf("can't find file to patch (unable to strip %d path components from %q / %q)", strip, fp.oldPath, fp.newPath)
		}
		for _, sp := range stripped {
			abs := resolve(baseDir, sp)
			if _, serr := os.Stat(abs); serr == nil {
				return sp, abs, nil
			}
		}
		// Creation or missing target: use last (newPath) candidate
		pick := stripped[len(stripped)-1]
		return pick, resolve(baseDir, pick), nil
	}

	// Default when -p was NOT explicitly specified:
	// 1. Check exact path relative to baseDir
	// 2. If git-style a/... or b/..., check with 1 component stripped if exists
	// 3. Check basename (classic POSIX/GNU patch default when -p is omitted)
	for _, c := range candidates {
		s := strings.ReplaceAll(c, `\`, "/")
		abs := resolve(baseDir, s)
		if _, serr := os.Stat(abs); serr == nil {
			return s, abs, nil
		}
	}
	for _, c := range candidates {
		if sp, ok := stripPatchPath(c, 1); ok && sp != "/dev/null" {
			abs := resolve(baseDir, sp)
			if _, serr := os.Stat(abs); serr == nil {
				return sp, abs, nil
			}
		}
	}
	for _, c := range candidates {
		base := gnuBasename(c)
		if base != "" && base != "/" {
			abs := resolve(baseDir, base)
			if _, serr := os.Stat(abs); serr == nil {
				return base, abs, nil
			}
		}
	}

	// File does not exist yet (e.g. new file creation):
	pick := strings.ReplaceAll(candidates[len(candidates)-1], `\`, "/")
	if fp.isGitDiff && (strings.HasPrefix(pick, "a/") || strings.HasPrefix(pick, "b/")) {
		pick = pick[2:]
	} else if !strings.Contains(pick, "/") {
		// Already basename
	} else {
		// If parent dir of full pick doesn't exist and strip wasn't given, GNU patch uses basename.
		if _, serr := os.Stat(filepath.Dir(resolve(baseDir, pick))); serr != nil {
			pick = gnuBasename(pick)
		}
	}
	return pick, resolve(baseDir, pick), nil
}

// ---- Hunk Matching & Application ----

func splitPatchTargetBytes(data []byte) (lines []string, trailingNL bool, useCRLF bool) {
	if len(data) == 0 {
		return nil, false, false
	}
	crlfCount := 0
	lfCount := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			lfCount++
			if i > 0 && data[i-1] == '\r' {
				crlfCount++
			}
		}
	}
	useCRLF = lfCount > 0 && crlfCount*2 >= lfCount

	trailingNL = data[len(data)-1] == '\n'
	s := string(data)
	if trailingNL {
		s = s[:len(s)-1]
	}
	raw := strings.Split(s, "\n")
	for i := range raw {
		raw[i] = strings.TrimSuffix(raw[i], "\r")
	}
	return raw, trailingNL, useCRLF
}

func joinPatchTargetLines(lines []string, trailingNL, useCRLF bool) []byte {
	if len(lines) == 0 {
		return nil
	}
	eol := "\n"
	if useCRLF {
		eol = "\r\n"
	}
	var b bytes.Buffer
	for i, l := range lines {
		b.WriteString(l)
		if i < len(lines)-1 || trailingNL {
			b.WriteString(eol)
		}
	}
	return b.Bytes()
}

func applySingleFilePatch(
	ctx context.Context,
	hc interp.HandlerContext,
	fp *filePatch,
	displayRel, targetAbs, baseDir string,
	opts *patchOptions,
) (int, error) {
	_ = ctx
	origFi, statErr := os.Stat(targetAbs)
	origExists := statErr == nil

	var origData []byte
	origPerm := os.FileMode(0o644)
	if fp.newMode != 0 {
		origPerm = fp.newMode
	}

	if origExists {
		if origFi.IsDir() {
			return 2, fmt.Errorf("File %s is a directory", displayRel)
		}
		origPerm = origFi.Mode().Perm()
		var rerr error
		origData, rerr = os.ReadFile(targetAbs)
		if rerr != nil {
			return 2, fmt.Errorf("Can't read file %s : %v", displayRel, rerr)
		}
	} else if !fp.isNewFile {
		// Check -N forward mode: if a deleted-file patch was ALREADY applied, target no longer exists!
		if opts.forward && fp.isDeletedFile {
			if !opts.silent {
				fmt.Fprintf(hc.Stdout, "Reversed (or previously applied) patch detected!  Skipping patch.\n")
			}
			return 1, nil
		}
		// Also check if hunks purely add from empty (oldCount == 0 across all hunks)
		allFromEmpty := len(fp.hunks) > 0
		for _, h := range fp.hunks {
			if h.oldCount != 0 {
				allFromEmpty = false
				break
			}
		}
		if !allFromEmpty {
			return 2, fmt.Errorf("Can't find file to patch: %s", displayRel)
		}
	}

	if !opts.silent && (!opts.hasOutput || opts.outputFile != "-") {
		fmt.Fprintf(hc.Stdout, "patching file %s\n", displayRel)
	}

	lines, trailingNL, useCRLF := splitPatchTargetBytes(origData)
	if !origExists && len(lines) == 0 {
		trailingNL = true
	}

	// Check -N (--forward): if file already matches the reversed patch cleanly when forward fails!
	if opts.forward && !opts.reverse && len(fp.hunks) > 0 {
		if canApplyAllHunks(lines, fp.hunks, opts) {
			// Normal forward application works — proceed normally!
		} else {
			revFP := reverseFilePatch(*fp)
			if (!origExists && revFP.isNewFile) || (origExists && canApplyAllHunks(lines, revFP.hunks, opts)) {
				if !opts.silent {
					fmt.Fprintf(hc.Stdout, "Reversed (or previously applied) patch detected!  Skipping patch.\n")
				}
				return 1, nil
			}
		}
	}

	lineDelta := 0
	minPos := 0
	failedHunks := 0
	var rejectedHunks []patchHunk

	for hIdx, h := range fp.hunks {
		newLines, newDelta, nextMinPos, offset, fuzz, appliedNL, hasNLChange, ok := applyOneHunkFuzzy(
			lines, h, lineDelta, minPos, opts.maxFuzz, opts.ignoreWhitespace,
		)
		if !ok {
			failedHunks++
			rejectedHunks = append(rejectedHunks, h)
			fmt.Fprintf(hc.Stderr, "Hunk #%d FAILED at %d.\n", hIdx+1, max(h.oldStart+lineDelta, 1))
			continue
		}
		lines = newLines
		lineDelta = newDelta
		minPos = nextMinPos
		if hasNLChange {
			trailingNL = appliedNL
		}
		if !opts.silent && (offset != 0 || fuzz > 0) && (!opts.hasOutput || opts.outputFile != "-") {
			switch {
			case offset != 0 && fuzz > 0:
				fmt.Fprintf(hc.Stdout, "Hunk #%d succeeded at %d with fuzz %d (offset %+d lines).\n", hIdx+1, nextMinPos, fuzz, offset)
			case fuzz > 0:
				fmt.Fprintf(hc.Stdout, "Hunk #%d succeeded at %d with fuzz %d.\n", hIdx+1, nextMinPos, fuzz)
			case offset != 0:
				fmt.Fprintf(hc.Stdout, "Hunk #%d succeeded at %d (offset %+d lines).\n", hIdx+1, max(h.newStart+offset, 1), offset)
			}
		}
	}

	if failedHunks > 0 {
		fmt.Fprintf(hc.Stderr, "%d out of %d hunk%s FAILED\n", failedHunks, len(fp.hunks), pluralS(len(fp.hunks)))
		if opts.rejectFile != "" && opts.rejectFile != "-" && !opts.dryRun {
			_ = writeRejectFile(resolve(baseDir, opts.rejectFile), rejectedHunks)
		}
	}

	if opts.dryRun {
		if failedHunks > 0 {
			return 1, nil
		}
		return 0, nil
	}

	// If ALL hunks failed and there were hunks, do not clobber the original file.
	if failedHunks > 0 && failedHunks == len(fp.hunks) {
		return 1, nil
	}

	resultBytes := joinPatchTargetLines(lines, trailingNL, useCRLF)

	// Output override (-o outfile or -o -)
	if opts.hasOutput {
		if opts.outputFile == "-" || opts.outputFile == "/dev/stdout" {
			if _, werr := hc.Stdout.Write(resultBytes); werr != nil {
				return 2, werr
			}
		} else if !isToolDevNull(opts.outputFile) {
			outAbs := resolve(baseDir, opts.outputFile)
			if werr := atomicWriteFile(outAbs, resultBytes, origPerm); werr != nil {
				return 2, werr
			}
		}
		if failedHunks > 0 {
			return 1, nil
		}
		return 0, nil
	}

	// Backup original file if -b / --backup requested and original existed.
	if opts.backup && origExists {
		if berr := atomicWriteFile(targetAbs+".orig", origData, origPerm); berr != nil {
			return 2, fmt.Errorf("can't create backup %s.orig: %v", displayRel, berr)
		}
	}

	shouldDelete := len(resultBytes) == 0 && (fp.isDeletedFile || opts.removeEmptyFiles) && failedHunks == 0
	if shouldDelete {
		if origExists {
			if runtime.GOOS == "windows" {
				_ = os.Chmod(targetAbs, 0o666)
			}
			if rerr := os.Remove(targetAbs); rerr != nil && !os.IsNotExist(rerr) {
				return 2, fmt.Errorf("can't remove file %s: %v", displayRel, rerr)
			}
		}
	} else {
		if werr := atomicWriteFile(targetAbs, resultBytes, origPerm); werr != nil {
			return 2, fmt.Errorf("can't write file %s: %v", displayRel, werr)
		}
	}

	if failedHunks > 0 {
		return 1, nil
	}
	return 0, nil
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func canApplyAllHunks(origLines []string, hunks []patchHunk, opts *patchOptions) bool {
	lines := append([]string(nil), origLines...)
	delta := 0
	minPos := 0
	for _, h := range hunks {
		var ok bool
		lines, delta, minPos, _, _, _, _, ok = applyOneHunkFuzzy(lines, h, delta, minPos, opts.maxFuzz, opts.ignoreWhitespace)
		if !ok {
			return false
		}
	}
	return true
}

func applyOneHunkFuzzy(
	lines []string,
	h patchHunk,
	lineDelta, minPos, maxFuzz int,
	ignoreWS bool,
) (outLines []string, newDelta, nextMinPos, offset, usedFuzz int, newTrailingNL, hasNLChange, ok bool) {
	// Separate hunk into leading context count, trailing context count, oldLines, newLines.
	var oldSide, newSide []string
	for _, pl := range h.lines {
		if pl.op == ' ' || pl.op == '-' {
			oldSide = append(oldSide, pl.text)
		}
		if pl.op == ' ' || pl.op == '+' {
			newSide = append(newSide, pl.text)
		}
	}

	// Special case: pure insertion hunk (oldSide is empty, e.g. creating a file or inserting at top/line).
	if len(oldSide) == 0 {
		// In unified diff, @@ -0,0 +1,N @@ creates from an empty file; never duplicate onto a non-empty file.
		if h.oldStart == 0 && len(lines) > 0 {
			return nil, 0, 0, 0, 0, false, false, false
		}
		insPos := 0
		if h.oldStart > 0 {
			// In unified diff, @@ -L,0 +N,C @@ inserts AFTER 1-based line L (0-based index L).
			insPos = h.oldStart + lineDelta
		}
		if insPos < minPos {
			insPos = minPos
		}
		if insPos > len(lines) {
			insPos = len(lines)
		}
		res := make([]string, 0, len(lines)+len(newSide))
		res = append(res, lines[:insPos]...)
		res = append(res, newSide...)
		res = append(res, lines[insPos:]...)

		atEOF := insPos == len(lines)
		nlChanged := atEOF && len(newSide) > 0
		nlVal := !h.newNoEOFNL
		return res, lineDelta + len(newSide), insPos + len(newSide), 0, 0, nlVal, nlChanged, true
	}

	leadCtx := 0
	for leadCtx < len(h.lines) && h.lines[leadCtx].op == ' ' {
		leadCtx++
	}
	trailCtx := 0
	for trailCtx < len(h.lines)-leadCtx && h.lines[len(h.lines)-1-trailCtx].op == ' ' {
		trailCtx++
	}

	expected := (h.oldStart - 1) + lineDelta
	if expected < minPos {
		expected = minPos
	}

	for fuzz := 0; fuzz <= maxFuzz; fuzz++ {
		trimLead := min(fuzz, leadCtx)
		trimTrail := min(fuzz, trailCtx)
		if fuzz > 0 && trimLead == min(fuzz-1, leadCtx) && trimTrail == min(fuzz-1, trailCtx) {
			continue
		}
		subOld := oldSide[trimLead : len(oldSide)-trimTrail]
		subNew := newSide[trimLead : len(newSide)-trimTrail]
		if len(subOld) == 0 {
			// Never trim away all context lines when there are no '-' lines!
			continue
		}

		targetPos := expected + trimLead
		matchPos, found := searchHunkPosition(lines, subOld, targetPos, minPos, ignoreWS)
		if !found {
			continue
		}

		res := make([]string, 0, len(lines)-len(subOld)+len(subNew))
		res = append(res, lines[:matchPos]...)
		res = append(res, subNew...)
		res = append(res, lines[matchPos+len(subOld):]...)

		effStart := matchPos - trimLead
		off := effStart - ((h.oldStart - 1) + lineDelta)
		atEOF := matchPos+len(subOld) == len(lines) && trimTrail == 0
		nlChanged := false
		nlVal := true
		if atEOF {
			nlChanged = true
			nlVal = !h.newNoEOFNL
		}
		return res, lineDelta + (len(subNew) - len(subOld)), matchPos + len(subNew), off, fuzz, nlVal, nlChanged, true
	}

	return nil, lineDelta, minPos, 0, 0, false, false, false
}

func searchHunkPosition(lines, needle []string, expected, minPos int, ignoreWS bool) (int, bool) {
	maxPos := len(lines) - len(needle)
	if maxPos < minPos {
		return -1, false
	}
	start := expected
	if start < minPos {
		start = minPos
	}
	if start > maxPos {
		start = maxPos
	}

	if hunkSliceEqual(lines[start:start+len(needle)], needle, ignoreWS) {
		return start, true
	}

	maxDist := max(start-minPos, maxPos-start)
	for dist := 1; dist <= maxDist; dist++ {
		// Prefer forward then backward (or nearby)
		if fwd := start + dist; fwd <= maxPos {
			if hunkSliceEqual(lines[fwd:fwd+len(needle)], needle, ignoreWS) {
				return fwd, true
			}
		}
		if bwd := start - dist; bwd >= minPos {
			if hunkSliceEqual(lines[bwd:bwd+len(needle)], needle, ignoreWS) {
				return bwd, true
			}
		}
	}
	return -1, false
}

func hunkSliceEqual(a, b []string, ignoreWS bool) bool {
	if len(a) != len(b) {
		return false
	}
	if !ignoreWS {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for i := range a {
		if collapseWS(a[i]) != collapseWS(b[i]) {
			return false
		}
	}
	return true
}

func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func writeRejectFile(path string, hunks []patchHunk) error {
	var b bytes.Buffer
	for _, h := range hunks {
		b.WriteString(h.header)
		b.WriteByte('\n')
		for _, pl := range h.lines {
			b.WriteByte(pl.op)
			b.WriteString(pl.text)
			b.WriteByte('\n')
		}
	}
	return atomicWriteFile(path, b.Bytes(), 0o644)
}

func atomicWriteFile(targetPath string, data []byte, perm os.FileMode) error {
	if runtime.GOOS == "windows" && strings.EqualFold(targetPath, "NUL") || targetPath == "/dev/null" {
		return nil
	}
	// Resolve symlinks if target already exists as a symlink so atomic replace updates the link target.
	if lfi, lerr := os.Lstat(targetPath); lerr == nil && lfi.Mode()&os.ModeSymlink != 0 {
		if resolved, rerr := filepath.EvalSymlinks(targetPath); rerr == nil {
			targetPath = resolved
		}
	}
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".unish-atomic-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if perm != 0 {
		_ = tmp.Chmod(perm)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Ensure read-only destination on Windows doesn't block Rename.
		if dfi, derr := os.Stat(targetPath); derr == nil && dfi.Mode()&0o200 == 0 {
			_ = os.Chmod(targetPath, 0o666)
			defer func() { _ = os.Chmod(targetPath, perm) }()
		}
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return err
	}
	if runtime.GOOS != "windows" && perm != 0 {
		_ = os.Chmod(targetPath, perm)
	}
	cleanup = false
	return nil
}
