// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

// scenario is one fixture directory served from its own namespace.
type scenario struct {
	Dir string
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
	// Two bindings for tenant "acme" created in the same second: the oldest
	// creationTimestamp wins and the name breaks the tie, so acme-binding-a.
	{Dir: "04-two-tenant-bindings"},
	// Tenant binding points at a PipelineConfig that does not exist: Unavailable
	// with no fallthrough to the group or default config.
	{Dir: "05-binding-missing-config"},
}

// outcome is what GetConfig answered for one attribute set.
type outcome struct {
	Code    string `json:"code"` // "ok" or the lowercase Connect code
	Content string `json:"content,omitempty"`
	Hash    string `json:"hash,omitempty"`
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

// resolve records the outcome for one attribute set and checks that replaying
// the recorded hash yields NotModified.
func (h *harness) resolve(sc scenario, attrs map[string]string) outcome {
	GinkgoHelper()
	resp, code := h.call(sc, attrs, "")
	if code != "ok" {
		return outcome{Code: code}
	}
	Expect(resp.NotModified).To(BeFalse())
	Expect(resp.Hash).NotTo(BeEmpty())

	replay, replayCode := h.call(sc, attrs, resp.Hash)
	Expect(replayCode).To(Equal("ok"))
	Expect(replay.NotModified).To(BeTrue(), "sending the served hash must yield NotModified")
	return outcome{Code: "ok", Content: resp.Content, Hash: resp.Hash}
}

// expectMatchesGolden compares a live outcome to the golden one.
func (h *harness) expectMatchesGolden(sc scenario, attrs map[string]string, want outcome) {
	GinkgoHelper()
	Expect(h.resolve(sc, attrs)).To(Equal(want))
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
						recorded[as.Name] = current.resolve(sc, as.Attrs)
						return
					}
					current.expectMatchesGolden(sc, as.Attrs, readGolden(sc)[as.Name])
				})
			}
		})
	}
})

// The Overlap condition goes through the real manager, including the watch
// that re-reconciles the loser when the winner disappears.
var _ = Describe("binding Overlap condition", func() {
	const ns = "compat-overlap"

	overlapStatus := func(name string) metav1.ConditionStatus {
		b := &fleetv1alpha1.CollectorGroupBinding{}
		if err := current.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, b); err != nil {
			return ""
		}
		if c := meta.FindStatusCondition(b.Status.Conditions, fleetv1alpha1.ConditionTypeOverlap); c != nil {
			return c.Status
		}
		return ""
	}

	It("flags the loser and clears it once the winner is deleted", func() {
		Expect(client.IgnoreAlreadyExists(current.client.Create(ctx, &corev1.Namespace{Name: ns}))).To(Succeed())
		Expect(current.client.Create(ctx, &fleetv1alpha1.PipelineConfig{
			Name: "pc", Namespace: ns, Spec: fleetv1alpha1.PipelineConfigSpec{Content: "logging {}"},
		})).To(Succeed())
		// bind-a is created first, so it wins on both creationTimestamp and name.
		for _, name := range []string{"bind-a", "bind-b"} {
			Expect(current.client.Create(ctx, &fleetv1alpha1.CollectorGroupBinding{
				Name: name, Namespace: ns,
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{TenantRef: "dup", PipelineConfigRef: "pc"},
			})).To(Succeed())
		}

		Eventually(func() metav1.ConditionStatus { return overlapStatus("bind-b") }, 30*time.Second).Should(Equal(metav1.ConditionTrue))
		Expect(overlapStatus("bind-a")).To(Equal(metav1.ConditionFalse))

		winner := &fleetv1alpha1.CollectorGroupBinding{Name: "bind-a", Namespace: ns}
		Expect(current.client.Delete(ctx, winner)).To(Succeed())
		Eventually(func() metav1.ConditionStatus { return overlapStatus("bind-b") }, 30*time.Second).Should(Equal(metav1.ConditionFalse))
	})
})
