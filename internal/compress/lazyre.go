package compress

import (
	"regexp"
	"sync"
)

// lazyRe is a lazily compiled regular expression. The zero value is usable.
type lazyRe struct {
	pattern string
	once    sync.Once
	re      *regexp.Regexp
}

func (r *lazyRe) compile() {
	r.once.Do(func() {
		r.re = regexp.MustCompile(r.pattern)
	})
}

func (r *lazyRe) MatchString(s string) bool {
	r.compile()
	return r.re.MatchString(s)
}

func (r *lazyRe) FindStringSubmatch(s string) []string {
	r.compile()
	return r.re.FindStringSubmatch(s)
}

func (r *lazyRe) FindStringIndex(s string) []int {
	r.compile()
	return r.re.FindStringIndex(s)
}

func (r *lazyRe) FindAllStringSubmatch(s string, n int) [][]string {
	r.compile()
	return r.re.FindAllStringSubmatch(s, n)
}

func (r *lazyRe) ReplaceAllString(src, repl string) string {
	r.compile()
	return r.re.ReplaceAllString(src, repl)
}
