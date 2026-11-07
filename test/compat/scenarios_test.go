// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

// scenario is one fixture directory served from its own namespace.
type scenario struct {
	Dir string
	// Ambiguous lists the attribute sets whose winner depends on cache list
	// order today. For those, the golden records Candidates instead of one answer.
	Ambiguous []string
	// CandidateConfigs names the PipelineConfigs the ambiguous lookups may serve.
	CandidateConfigs []string
}

func (s scenario) namespace() string { return "compat-" + s.Dir }

// attrSets are the local_attributes sent for every scenario.
var attrSets = []struct {
	Name  string
	Attrs map[string]string
}{
	{"tenant-only", map[string]string{"tenant": "acme"}},
	{"group-only", map[string]string{"collector_group": "prod"}},
	{"both", map[string]string{"tenant": "acme", "collector_group": "prod"}},
	{"neither", map[string]string{}},
	{"unknown", map[string]string{"tenant": "nope", "collector_group": "nope"}},
}

var scenarios = []scenario{
	// Only the default-labeled PipelineConfig exists.
	{Dir: "01-default-only"},
	// Group binding plus default.
	{Dir: "02-group-and-default"},
	// Tenant binding, group binding and default.
	{Dir: "03-tenant-group-default"},
	// Two bindings for tenant "acme". Today the winner is whichever the cache
	// lists first, which is nondeterministic, so the golden pins the candidate
	// set. A later change (WS-S) makes the winner deterministic by
	// creationTimestamp, then name; the golden should then be tightened.
	{
		Dir:              "04-two-tenant-bindings",
		Ambiguous:        []string{"tenant-only", "both"},
		CandidateConfigs: []string{"acme-config-a", "acme-config-b"},
	},
	// Tenant binding points at a PipelineConfig that does not exist. Today this
	// is CodeInternal with no fallthrough. WS-S intentionally changes it to
	// Unavailable; that golden diff is justified and expected.
	{Dir: "05-binding-missing-config"},
	// Default PipelineConfig has empty spec.content, so the controller never
	// sets a hash and the resolver answers Unavailable. WS-S adds CEL that
	// rejects empty content, so this fixture will then have to be created
	// through a pre-existing-object path (created before the CRD update).
	{Dir: "06-not-ready-default"},
}

// candidate is one servable config.
type candidate struct {
	Content string `json:"content"`
	Hash    string `json:"hash"`
}

// outcome is what GetConfig answered for one attribute set.
type outcome struct {
	Code       string      `json:"code"` // "ok" or the lowercase Connect code
	Content    string      `json:"content,omitempty"`
	Hash       string      `json:"hash,omitempty"`
	Candidates []candidate `json:"candidates,omitempty"` // ambiguous lookups only
}

func (sc scenario) isAmbiguous(attr string) bool {
	return slices.Contains(sc.Ambiguous, attr)
}

func goldenPath(sc scenario) string { return filepath.Join("golden", sc.Dir+".json") }

func readGolden(sc scenario) map[string]outcome {
	GinkgoHelper()
	raw, err := os.ReadFile(goldenPath(sc))
	Expect(err).NotTo(HaveOccurred(), "missing golden; generate with: go test ./test/compat/... -args -update")
	g := map[string]outcome{}
	Expect(json.Unmarshal(raw, &g)).To(Succeed())
	return g
}

func writeGolden(sc scenario, g map[string]outcome) {
	GinkgoHelper()
	raw, err := json.MarshalIndent(g, "", "  ")
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(goldenPath(sc), append(raw, '\n'), 0o644)).To(Succeed())
}

// call performs one GetConfig and returns the response or the Connect code.
func (h *harness) call(sc scenario, attrs map[string]string, hash string) (*collectorv1.GetConfigResponse, string) {
	GinkgoHelper()
	resp, err := h.serviceFor(sc.namespace()).GetConfig(ctx, connect.NewRequest(&collectorv1.GetConfigRequest{
		Id:              "compat-collector",
		LocalAttributes: attrs,
		Hash:            hash,
	}))
	if err != nil {
		return nil, connect.CodeOf(err).String()
	}
	return resp.Msg, "ok"
}

// candidates reads the servable content of the scenario's candidate configs.
func (h *harness) candidates(sc scenario) []candidate {
	GinkgoHelper()
	out := make([]candidate, 0, len(sc.CandidateConfigs))
	for _, name := range sc.CandidateConfigs {
		pc := &fleetv1alpha1.PipelineConfig{}
		Expect(h.client.Get(ctx, client.ObjectKey{Namespace: sc.namespace(), Name: name}, pc)).To(Succeed())
		out = append(out, candidate{Content: pc.Spec.Content, Hash: pc.Status.ContentHash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// resolve records the outcome for one attribute set and checks that replaying
// the recorded hash yields NotModified.
func (h *harness) resolve(sc scenario, attrName string, attrs map[string]string) outcome {
	GinkgoHelper()
	resp, code := h.call(sc, attrs, "")
	if code != "ok" {
		return outcome{Code: code}
	}
	Expect(resp.NotModified).To(BeFalse())
	Expect(resp.Hash).NotTo(BeEmpty())

	replay, replayCode := h.call(sc, attrs, resp.Hash)
	Expect(replayCode).To(Equal("ok"))
	if sc.isAmbiguous(attrName) {
		// The replay may land on the other binding and then serve its content.
		if !replay.NotModified {
			Expect(h.candidates(sc)).To(ContainElement(candidate{Content: replay.Content, Hash: replay.Hash}))
		}
		return outcome{Code: "ok", Candidates: h.candidates(sc)}
	}
	Expect(replay.NotModified).To(BeTrue(), "sending the served hash must yield NotModified")
	return outcome{Code: "ok", Content: resp.Content, Hash: resp.Hash}
}

// expectMatchesGolden compares a live outcome to the golden one.
func (h *harness) expectMatchesGolden(sc scenario, attrName string, attrs map[string]string, want outcome) {
	GinkgoHelper()
	got := h.resolve(sc, attrName, attrs)
	if !sc.isAmbiguous(attrName) {
		Expect(got).To(Equal(want))
		return
	}
	// Ambiguous: the served answer must be one of the pinned candidates.
	Expect(got.Code).To(Equal("ok"))
	Expect(got.Candidates).To(Equal(want.Candidates))
	resp, code := h.call(sc, attrs, "")
	Expect(code).To(Equal("ok"))
	Expect(want.Candidates).To(ContainElement(candidate{Content: resp.Content, Hash: resp.Hash}))
}

var _ = Describe("served behavior", func() {
	for _, sc := range scenarios {
		Describe(sc.Dir, Ordered, func() {
			recorded := map[string]outcome{}

			BeforeAll(func() { current.install(sc) })

			AfterAll(func() {
				if updateGoldens() {
					writeGolden(sc, recorded)
				}
			})

			for _, as := range attrSets {
				It("serves the golden answer for "+as.Name, func() {
					if updateGoldens() {
						recorded[as.Name] = current.resolve(sc, as.Name, as.Attrs)
						return
					}
					current.expectMatchesGolden(sc, as.Name, as.Attrs, readGolden(sc)[as.Name])
				})
			}
		})
	}
})
