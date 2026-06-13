package mcp

import (
	"regexp"
	"sync"
)

// lazyRe wraps regexp.Regexp and defers compilation to first use.
// Package-level vars that use lazyRe pay zero startup cost for compress
// regexps; the pattern is compiled once on the first call to any method.
type lazyRe struct {
	once    sync.Once
	pattern string
	re      *regexp.Regexp
}

func (l *lazyRe) get() *regexp.Regexp {
	l.once.Do(func() { l.re = regexp.MustCompile(l.pattern) })
	return l.re
}

func (l *lazyRe) MatchString(s string) bool {
	return l.get().MatchString(s)
}

func (l *lazyRe) FindStringSubmatch(s string) []string {
	return l.get().FindStringSubmatch(s)
}

func (l *lazyRe) FindStringIndex(s string) []int {
	return l.get().FindStringIndex(s)
}

func (l *lazyRe) ReplaceAllString(src, repl string) string {
	return l.get().ReplaceAllString(src, repl)
}
