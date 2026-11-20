// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package selection_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/selection"
)

func TestSelection(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Selection Suite")
}

func binding(name string, created time.Time) fleetv1alpha1.CollectorGroupBinding {
	return fleetv1alpha1.CollectorGroupBinding{
		Name: name, CreationTimestamp: metav1.NewTime(created),
	}
}

var _ = Describe("Pick", func() {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	DescribeTable("chooses the winner",
		func(items []fleetv1alpha1.CollectorGroupBinding, want string) {
			got := selection.Pick(items)
			if want == "" {
				Expect(got).To(BeNil())
				return
			}
			Expect(got).NotTo(BeNil())
			Expect(got.Name).To(Equal(want))
		},
		Entry("empty", nil, ""),
		Entry("single", []fleetv1alpha1.CollectorGroupBinding{binding("only", t0)}, "only"),
		Entry("oldest wins over name order",
			[]fleetv1alpha1.CollectorGroupBinding{binding("a", t0.Add(time.Hour)), binding("z", t0)}, "z"),
		Entry("name breaks a timestamp tie",
			[]fleetv1alpha1.CollectorGroupBinding{binding("b", t0), binding("a", t0), binding("c", t0)}, "a"),
		Entry("input order does not matter",
			[]fleetv1alpha1.CollectorGroupBinding{binding("c", t0), binding("b", t0), binding("a", t0)}, "a"),
	)

	It("returns a pointer into the slice", func() {
		items := []fleetv1alpha1.CollectorGroupBinding{binding("a", t0), binding("b", t0)}
		Expect(selection.Pick(items)).To(BeIdenticalTo(&items[0]))
	})
})
