// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

// Package validate holds the content-validation mode shared by the
// reconciler, the resolver and the admission webhook.
package validate

import "fmt"

// Mode says what happens to PipelineConfig content that does not parse.
type Mode string

const (
	// ModeOff does not parse content; every non-blank config is valid.
	ModeOff Mode = "off"
	// ModeWarn reports syntax errors but still serves the content.
	ModeWarn Mode = "warn"
	// ModeEnforce rejects invalid content at admission and keeps serving the
	// last valid content when an invalid edit gets through.
	ModeEnforce Mode = "enforce"
)

// ParseMode validates the --content-validation flag value.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeOff, ModeWarn, ModeEnforce:
		return m, nil
	}
	return "", fmt.Errorf("invalid content validation mode %q, must be one of off, warn, enforce", s)
}

// Parses reports whether content is parsed at all. The zero Mode counts as off.
func (m Mode) Parses() bool { return m == ModeWarn || m == ModeEnforce }
