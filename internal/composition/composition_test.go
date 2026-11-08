// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package composition_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	comp "github.com/pkarakal/alloy-remote-config/internal/composition"
)

func TestComposition(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Composition Suite")
}

const baseDoc = "prometheus.remote_write \"base\" { }\n"

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var _ = Describe("Wrap", func() {
	It("renders the documented block format", func() {
		want := "\n// alloy-remote-config: additive pipeline from CollectorGroupBinding \"debug-1\"\n" +
			"declare \"arc_debug_1\" {\nlogging_probe \"x\" { }\n}\n" +
			"arc_debug_1 \"default\" { }\n"
		Expect(comp.Wrap("debug-1", "logging_probe \"x\" { }")).To(Equal(want))
	})
})

var _ = DescribeTable("Label",
	func(name, want string) { Expect(comp.Label(name)).To(Equal(want)) },
	Entry("plain", "debug", "arc_debug"),
	Entry("hyphen", "debug-pipeline", "arc_debug_pipeline"),
	Entry("dots", "team.a.debug", "arc_team_a_debug"),
	Entry("digits and underscores kept", "a_1", "arc_a_1"),
)

var _ = DescribeTable("Compose",
	func(base string, adds []comp.Additive, wantContent string, wantIncluded, wantCollided []string) {
		baseHash := sum(base)
		res := comp.Compose(base, baseHash, adds)
		Expect(res.Content).To(Equal(wantContent))
		Expect(res.Included).To(Equal(wantIncluded))
		Expect(res.Collided).To(Equal(wantCollided))
		if len(adds) == 0 {
			Expect(res.Hash).To(Equal(baseHash))
		} else {
			Expect(res.Hash).To(Equal(sum(wantContent)))
		}
	},
	Entry("no additives is byte-identical", baseDoc, nil, baseDoc, []string(nil), []string(nil)),
	Entry("one additive appended to the base", baseDoc,
		[]comp.Additive{{BindingName: "a", Content: "c1"}},
		baseDoc+comp.Wrap("a", "c1"), []string{"a"}, []string(nil)),
	Entry("name order, not input order", baseDoc,
		[]comp.Additive{{BindingName: "b", Content: "c2"}, {BindingName: "a", Content: "c1"}},
		baseDoc+comp.Wrap("a", "c1")+comp.Wrap("b", "c2"), []string{"a", "b"}, []string(nil)),
	Entry("hyphens and dots are escaped", baseDoc,
		[]comp.Additive{{BindingName: "x-y.z", Content: "c"}},
		baseDoc+comp.Wrap("x-y.z", "c"), []string{"x-y.z"}, []string(nil)),
	Entry("colliding labels keep the first by name", baseDoc,
		[]comp.Additive{{BindingName: "a.b", Content: "dot"}, {BindingName: "a-b", Content: "hyphen"}},
		baseDoc+comp.Wrap("a-b", "hyphen"), []string{"a-b"}, []string{"a.b"}),
	Entry("additives only", "",
		[]comp.Additive{{BindingName: "a", Content: "c1"}},
		comp.Wrap("a", "c1"), []string{"a"}, []string(nil)),
)

var _ = Describe("Compose hash", func() {
	It("changes with the additive set", func() {
		one := comp.Compose(baseDoc, sum(baseDoc), []comp.Additive{{BindingName: "a", Content: "c"}})
		two := comp.Compose(baseDoc, sum(baseDoc), []comp.Additive{{BindingName: "a", Content: "c"}, {BindingName: "b", Content: "c"}})
		Expect(one.Hash).NotTo(Equal(two.Hash))
		Expect(one.Hash).NotTo(Equal(sum(baseDoc)))
	})

	It("is stable for the same input", func() {
		adds := []comp.Additive{{BindingName: "b", Content: "c"}, {BindingName: "a", Content: "c"}}
		Expect(comp.Compose(baseDoc, "h", adds).Hash).To(Equal(comp.Compose(baseDoc, "h", adds).Hash))
	})
})

var _ = DescribeTable("Collisions",
	func(names, want []string) { Expect(comp.Collisions(names)).To(Equal(want)) },
	Entry("none", []string{"a", "b"}, []string(nil)),
	Entry("dot loses to hyphen", []string{"a.b", "a-b"}, []string{"a.b"}),
	Entry("three way", []string{"a_b", "a-b", "a.b"}, []string{"a.b", "a_b"}),
)

var _ = DescribeTable("CheckSnippet",
	func(content string, wantErr string) {
		err := comp.CheckSnippet(content)
		if wantErr == "" {
			Expect(err).NotTo(HaveOccurred())
			return
		}
		Expect(err).To(MatchError(ContainSubstring(wantErr)))
	},
	Entry("valid component", "prometheus.remote_write \"x\" {\n  endpoint {\n    url = \"http://x\"\n  }\n}", ""),
	Entry("empty body", "", ""),
	Entry("debug sample pipeline", "local.file_match \"debug\" {\n  path_targets = [{\"__path__\" = \"/var/log/pods/*/*/*.log\"}]\n}\n\nloki.write \"debug\" {\n  endpoint {\n    url = sys.env(\"DEBUG_LOKI_URL\")\n  }\n}\n", ""),
	Entry("syntax error", "prometheus.remote_write \"x\" {", "additive"),
	Entry("escaping brace", "}\nlocal.file \"x\" { filename = \"f\" }\ndeclare \"y\" {", "must not close"),
	Entry("logging block", "logging {\n  level = \"debug\"\n}", "not allowed inside a declare"),
	Entry("remotecfg block", "remotecfg { }", "not allowed inside a declare"),
)

var _ = DescribeTable("CheckConfig",
	func(content string, wantErr string) {
		err := comp.CheckConfig(content)
		if wantErr == "" {
			Expect(err).NotTo(HaveOccurred())
			return
		}
		Expect(err).To(MatchError(ContainSubstring(wantErr)))
	},
	Entry("valid component", "prometheus.remote_write \"x\" {\n  endpoint {\n    url = \"http://x\"\n  }\n}", ""),
	Entry("logging block is fine at top level", "logging {\n  level = \"debug\"\n}", ""),
	Entry("unknown component still parses", "no.such_component \"x\" { }", ""),
	Entry("blank parses as an empty config", "  \n", ""),
	Entry("unterminated block", "prometheus.remote_write \"x\" {", "content:"),
	Entry("bad expression", "a = = 1", "expected expression"),
	Entry("stray brace", "}", "content:"),
)

var _ = Describe("CheckDocument", func() {
	It("reports a failure and flags the first sighting only", func() {
		first, err := comp.CheckDocument("doc-bad", "broken {")
		Expect(err).To(HaveOccurred())
		Expect(first).To(BeTrue())
		first, err = comp.CheckDocument("doc-bad", "broken {")
		Expect(err).To(HaveOccurred())
		Expect(first).To(BeFalse())
	})

	It("accepts a composed document", func() {
		res := comp.Compose(baseDoc, sum(baseDoc), []comp.Additive{{BindingName: "a", Content: "local.file \"f\" {\n filename = \"x\"\n}"}})
		_, err := comp.CheckDocument(res.Hash, res.Content)
		Expect(err).NotTo(HaveOccurred())
	})
})
