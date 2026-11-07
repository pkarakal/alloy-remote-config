// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package composition

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sync"

	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
)

// maxCacheEntries bounds the verdict cache; it is cleared when full.
const maxCacheEntries = 2048

// forbiddenInDeclare are top-level config blocks Alloy rejects inside a declare body.
var forbiddenInDeclare = []string{"logging", "tracing", "http", "remotecfg", "livedebugging"}

// probeName is the binding name used to check snippets. Validity does not
// depend on the label, only on the content.
const probeName = "probe"

var errEscapesDeclare = errors.New("content must not close the declare block it is wrapped in")

// verdicts caches parse results by content hash, so polling does not re-parse.
var verdicts = struct {
	mu sync.Mutex
	m  map[string]error
}{m: map[string]error{}}

func cached(key string, compute func() error) (hit bool, err error) {
	verdicts.mu.Lock()
	if v, ok := verdicts.m[key]; ok {
		verdicts.mu.Unlock()
		return true, v
	}
	verdicts.mu.Unlock()
	err = compute()
	verdicts.mu.Lock()
	if len(verdicts.m) >= maxCacheEntries {
		clear(verdicts.m)
	}
	verdicts.m[key] = err
	verdicts.mu.Unlock()
	return false, err
}

func hashKey(prefix, s string) string {
	sum := sha256.Sum256([]byte(s))
	return prefix + hex.EncodeToString(sum[:])
}

// CheckSnippet reports whether content is valid inside an additive declare
// block. It checks syntax and forbidden top-level blocks only: references to
// base components still fail at load time in Alloy. Line numbers in the error
// count the wrapper header lines before the content. Results are cached by
// content hash.
func CheckSnippet(content string) error {
	_, err := cached(hashKey("s:", content), func() error { return checkSnippet(content) })
	return err
}

func checkSnippet(content string) error {
	f, err := parser.ParseFile("additive", []byte(Wrap(probeName, content)))
	if err != nil {
		return err
	}
	// A stray closing brace in the content would escape the declare block.
	if len(f.Body) != 2 {
		return errEscapesDeclare
	}
	decl, ok := f.Body[0].(*ast.BlockStmt)
	if !ok || len(decl.Name) != 1 || decl.Name[0] != "declare" {
		return errEscapesDeclare
	}
	for _, stmt := range decl.Body {
		if b, ok := stmt.(*ast.BlockStmt); ok && len(b.Name) == 1 && slices.Contains(forbiddenInDeclare, b.Name[0]) {
			return errors.New("block \"" + b.Name[0] + "\" is not allowed inside a declare body")
		}
	}
	return nil
}

// CheckDocument reports whether a composed document parses. hash identifies the
// document. first is true the first time this hash is seen, so callers can log
// a failure once.
func CheckDocument(hash, doc string) (first bool, err error) {
	hit, err := cached("d:"+hash, func() error {
		_, perr := parser.ParseFile("composed", []byte(doc))
		return perr
	})
	return !hit, err
}
