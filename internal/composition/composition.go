// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

// Package composition appends additive pipelines to a base config. It is pure:
// callers pick the base and the additives, this package builds the document.
package composition

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// LabelPrefix starts every derived declare label.
const LabelPrefix = "arc_"

// blockFormat is the exact text appended per additive: a comment naming the
// binding, a declare block holding the content, and one instantiation. Alloy
// loads the response as one document, so unchanged base components keep
// running when blocks are appended. Format args: binding name, label, content,
// label.
const blockFormat = "\n// alloy-remote-config: additive pipeline from CollectorGroupBinding %q\n" +
	"declare %q {\n%s\n}\n%s \"default\" { }\n"

var invalidLabelChars = regexp.MustCompile(`[^A-Za-z0-9_]`)

// Label derives the declare label for a binding name. Alloy labels allow only
// letters, digits and underscores, so every other character becomes "_".
func Label(bindingName string) string {
	return LabelPrefix + invalidLabelChars.ReplaceAllString(bindingName, "_")
}

// Wrap renders one additive as an appended block (see blockFormat).
func Wrap(bindingName, content string) string {
	label := Label(bindingName)
	return fmt.Sprintf(blockFormat, bindingName, label, content, label)
}

// Additive is one additive binding's content.
type Additive struct {
	BindingName string
	Content     string
}

// Result is a composed document.
type Result struct {
	Content string
	Hash    string
	// Included lists the bindings whose blocks are in Content, in name order.
	Included []string
	// Collided lists bindings dropped because an earlier one has the same label.
	Collided []string
}

// Compose appends the additives to the base in binding name order. Without
// additives it returns the base content and hash unchanged, so serving stays
// byte-identical to a plain base. An empty base composes the additives alone.
func Compose(baseContent, baseHash string, additives []Additive) Result {
	if len(additives) == 0 {
		return Result{Content: baseContent, Hash: baseHash}
	}
	sorted := slices.Clone(additives)
	slices.SortStableFunc(sorted, func(a, b Additive) int { return strings.Compare(a.BindingName, b.BindingName) })

	var sb strings.Builder
	sb.WriteString(baseContent)
	res := Result{}
	seen := make(map[string]struct{}, len(sorted))
	for _, a := range sorted {
		label := Label(a.BindingName)
		if _, dup := seen[label]; dup {
			res.Collided = append(res.Collided, a.BindingName)
			continue
		}
		seen[label] = struct{}{}
		sb.WriteString(Wrap(a.BindingName, a.Content))
		res.Included = append(res.Included, a.BindingName)
	}
	res.Content = sb.String()
	sum := sha256.Sum256([]byte(res.Content))
	res.Hash = hex.EncodeToString(sum[:])
	return res
}

// Collisions returns the names whose label equals the label of an earlier name
// in sorted order, which are the ones Compose drops.
func Collisions(names []string) []string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	seen := make(map[string]struct{}, len(sorted))
	var out []string
	for _, n := range sorted {
		l := Label(n)
		if _, dup := seen[l]; dup {
			out = append(out, n)
		}
		seen[l] = struct{}{}
	}
	return out
}
