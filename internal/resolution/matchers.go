// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

// Package resolution decides which binding (or the default config) serves a
// collector. It is pure: callers gather the candidates, this package ranks them.
package resolution

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var matcherSyntax = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_.]*)\s*(=~|!~|!=|=)\s*("(?s:.*)")\s*$`)

// Matcher is one compiled `key OP "value"` expression.
type Matcher struct {
	key    string
	value  string
	re     *regexp.Regexp // nil for plain (in)equality
	negate bool
}

// ParseMatcher compiles a single matcher expression. Regex values are fully
// anchored and the value is a Go-quoted string.
func ParseMatcher(expr string) (Matcher, error) {
	m := matcherSyntax.FindStringSubmatch(expr)
	if m == nil {
		return Matcher{}, fmt.Errorf("matcher %q must look like key=\"value\" (operators: =, !=, =~, !~)", expr)
	}
	value, err := strconv.Unquote(m[3])
	if err != nil {
		return Matcher{}, fmt.Errorf("matcher %q has an invalid quoted value: %w", expr, err)
	}
	out := Matcher{key: m[1], value: value, negate: strings.HasPrefix(m[2], "!")}
	if strings.HasSuffix(m[2], "~") {
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return Matcher{}, fmt.Errorf("matcher %q has an invalid regular expression: %w", expr, err)
		}
		out.re = re
	}
	return out, nil
}

// Matches reports whether attrs satisfy the matcher. A missing attribute
// counts as the empty string, so `env!="prod"` matches a collector without env.
func (m Matcher) Matches(attrs map[string]string) bool {
	got := attrs[m.key]
	var hit bool
	if m.re != nil {
		hit = m.re.MatchString(got)
	} else {
		hit = got == m.value
	}
	return hit != m.negate
}

// Matchers is a compiled matcher list; all entries must match.
type Matchers []Matcher

// ParseMatchers compiles all expressions, failing on the first invalid one.
func ParseMatchers(exprs []string) (Matchers, error) {
	out := make(Matchers, 0, len(exprs))
	for _, e := range exprs {
		m, err := ParseMatcher(e)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// MatchAll reports whether attrs satisfy every matcher. An empty list never
// matches, so a group without matchers keeps legacy-only membership.
func (ms Matchers) MatchAll(attrs map[string]string) bool {
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !m.Matches(attrs) {
			return false
		}
	}
	return true
}
