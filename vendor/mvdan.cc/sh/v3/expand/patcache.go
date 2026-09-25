package expand

import (
	"regexp"
	"sync"

	"mvdan.cc/sh/v3/pattern"
)

// Shell code tends to run the same parameter-expansion patterns in hot
// loops (think ${var#pat} inside a while loop), and recompiling the
// pattern translation and regexp dominates those loops. Memoize both
// stages; the caches are bounded so pathological scripts can't grow
// them forever.

const patCacheMax = 2048

var (
	patMu    sync.Mutex
	patCache = map[string]string{}
	reMu     sync.Mutex
	reCache  = map[string]*regexp.Regexp{}
)

// cachedPattern memoizes pattern.Regexp (shell pattern -> regex source).
func cachedPattern(pat string, mode pattern.Mode) (string, error) {
	key := string(rune(mode)) + "\x00" + pat
	patMu.Lock()
	re, ok := patCache[key]
	patMu.Unlock()
	if ok {
		return re, nil
	}
	re, err := pattern.Regexp(pat, mode)
	if err != nil {
		return "", err
	}
	patMu.Lock()
	if len(patCache) >= patCacheMax {
		patCache = map[string]string{}
	}
	patCache[key] = re
	patMu.Unlock()
	return re, nil
}

// cachedRegexp memoizes regexp.MustCompile of the final regex source.
func cachedRegexp(re string) *regexp.Regexp {
	reMu.Lock()
	rx, ok := reCache[re]
	reMu.Unlock()
	if ok {
		return rx
	}
	rx = regexp.MustCompile(re)
	reMu.Lock()
	if len(reCache) >= patCacheMax {
		reCache = map[string]*regexp.Regexp{}
	}
	reCache[re] = rx
	reMu.Unlock()
	return rx
}
