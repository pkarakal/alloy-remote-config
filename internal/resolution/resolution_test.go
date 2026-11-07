// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package resolution_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	. "github.com/pkarakal/alloy-remote-config/internal/resolution"
)

func TestResolution(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Resolution Suite")
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func binding(name string, age time.Duration, tenant, group string, prio *int32) fleetv1alpha1.CollectorGroupBinding {
	return fleetv1alpha1.CollectorGroupBinding{
		Name: name, CreationTimestamp: metav1.NewTime(epoch.Add(age)),
		Spec: fleetv1alpha1.CollectorGroupBindingSpec{
			TenantRef: tenant, CollectorGroupRef: group, Priority: prio, PipelineConfigRef: "pc",
		},
	}
}

func mustMatchers(exprs ...string) Matchers {
	m, err := ParseMatchers(exprs)
	if err != nil {
		panic(err)
	}
	return m
}

var _ = DescribeTable("Matcher semantics",
	func(expr string, attrs map[string]string, want bool) {
		m, err := ParseMatcher(expr)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Matches(attrs)).To(Equal(want))
	},
	Entry("equal hit", `org_id="org-1"`, map[string]string{"org_id": "org-1"}, true),
	Entry("equal miss", `org_id="org-1"`, map[string]string{"org_id": "org-2"}, false),
	Entry("equal missing attr", `org_id="org-1"`, map[string]string{}, false),
	Entry("equal empty matches missing attr", `org_id=""`, map[string]string{}, true),
	Entry("not equal hit", `env!="prod"`, map[string]string{"env": "dev"}, true),
	Entry("not equal miss", `env!="prod"`, map[string]string{"env": "prod"}, false),
	Entry("not equal matches missing attr", `env!="prod"`, map[string]string{}, true),
	Entry("regex alternation hit", `cluster_id=~"c-1|c-2"`, map[string]string{"cluster_id": "c-2"}, true),
	Entry("regex is anchored", `cluster_id=~"c-1|c-2"`, map[string]string{"cluster_id": "c-22"}, false),
	Entry("regex anchored on the left too", `cluster_id=~"c-1|c-2"`, map[string]string{"cluster_id": "xc-1"}, false),
	Entry("regex does not match missing attr", `cluster_id=~"c-.*"`, map[string]string{}, false),
	Entry("negative regex hit", `env!~"prod.*"`, map[string]string{"env": "dev"}, true),
	Entry("negative regex miss", `env!~"prod.*"`, map[string]string{"env": "production"}, false),
	Entry("negative regex matches missing attr", `env!~"prod.*"`, map[string]string{}, true),
	Entry("dotted key", `collector.os="linux"`, map[string]string{"collector.os": "linux"}, true),
	Entry("whitespace around operator", `  env  =  "x" `, map[string]string{"env": "x"}, true),
	Entry("escaped quote in value", `env="a\"b"`, map[string]string{"env": `a"b`}, true),
)

var _ = DescribeTable("Invalid matchers",
	func(expr string) {
		_, err := ParseMatcher(expr)
		Expect(err).To(HaveOccurred())
	},
	Entry("no operator", `env`),
	Entry("unquoted value", `env=prod`),
	Entry("bad operator", `env>"a"`),
	Entry("bad key", `1env="a"`),
	Entry("bad regex", `env=~"("`),
	Entry("bad escape", `env="\q"`),
)

var _ = DescribeTable("Members",
	func(attrs map[string]string, groups []Group, want []string) {
		Expect(Members(attrs, groups)).To(Equal(want))
	},
	Entry("legacy only, group object absent",
		map[string]string{"collector_group": "g1"}, nil, []string{"g1"}),
	Entry("matcher only",
		map[string]string{"org_id": "o"},
		[]Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`)}}, []string{"cohort"}),
	Entry("matcher miss",
		map[string]string{"org_id": "x"},
		[]Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`)}}, nil),
	Entry("both legacy and matcher",
		map[string]string{"collector_group": "g1", "org_id": "o"},
		[]Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`)}}, []string{"cohort", "g1"}),
	Entry("legacy and matcher on the same group are deduplicated",
		map[string]string{"collector_group": "g1", "org_id": "o"},
		[]Group{{Name: "g1", Matchers: mustMatchers(`org_id="o"`)}}, []string{"g1"}),
	Entry("invalid matchers (nil) give legacy membership only",
		map[string]string{"collector_group": "g1", "org_id": "o"},
		[]Group{{Name: "g1"}, {Name: "cohort"}}, []string{"g1"}),
	Entry("all matchers are ANDed",
		map[string]string{"org_id": "o", "env": "dev"},
		[]Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`, `env="prod"`)}}, nil),
	Entry("missing attribute with != matches",
		map[string]string{"org_id": "o"},
		[]Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`, `env!="prod"`)}}, []string{"cohort"}),
	Entry("missing attribute with !~ matches",
		map[string]string{"org_id": "o"},
		[]Group{{Name: "cohort", Matchers: mustMatchers(`env!~"prod.*"`)}}, []string{"cohort"}),
)

var _ = Describe("Resolve", func() {
	type tc struct {
		attrs      map[string]string
		groups     []Group
		bindings   []fleetv1alpha1.CollectorGroupBinding
		hasDefault bool
		wantKind   Kind
		wantName   string
		wantPrio   int32
	}
	DescribeTable("ranking",
		func(c tc) {
			d := Resolve(Input{Attrs: c.attrs, Groups: c.groups, Bindings: c.bindings, HasDefault: c.hasDefault})
			Expect(d.Kind).To(Equal(c.wantKind))
			Expect(d.Priority).To(Equal(c.wantPrio))
			if c.wantName == "" {
				Expect(d.Binding).To(BeNil())
			} else {
				Expect(d.Binding).NotTo(BeNil())
				Expect(d.Binding.Name).To(Equal(c.wantName))
			}
		},
		Entry("nothing matches and no default", tc{
			attrs: map[string]string{"tenant": "t"}, wantKind: KindNone,
		}),
		Entry("default only", tc{
			attrs: map[string]string{}, hasDefault: true, wantKind: KindDefault,
		}),
		Entry("tenant beats group and default by default priorities", tc{
			attrs: map[string]string{"tenant": "t", "collector_group": "g"},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("grp", 0, "", "g", nil), binding("ten", time.Hour, "t", "", nil),
			},
			hasDefault: true, wantKind: KindTenant, wantName: "ten", wantPrio: 200,
		}),
		Entry("legacy group beats default", tc{
			attrs:      map[string]string{"collector_group": "g"},
			bindings:   []fleetv1alpha1.CollectorGroupBinding{binding("grp", 0, "", "g", nil)},
			hasDefault: true, wantKind: KindGroup, wantName: "grp", wantPrio: 100,
		}),
		Entry("binding for another tenant is not a candidate", tc{
			attrs:      map[string]string{"tenant": "t"},
			bindings:   []fleetv1alpha1.CollectorGroupBinding{binding("other", 0, "x", "", nil)},
			hasDefault: true, wantKind: KindDefault,
		}),
		Entry("matcher cohort with priority 300 outranks tenant binding", tc{
			attrs:  map[string]string{"tenant": "t", "org_id": "o"},
			groups: []Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`)}},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("ten", 0, "t", "", nil), binding("canary", time.Hour, "", "cohort", new(int32(300))),
			},
			wantKind: KindGroup, wantName: "canary", wantPrio: 300,
		}),
		Entry("matcher cohort without priority loses to tenant binding", tc{
			attrs:  map[string]string{"tenant": "t", "org_id": "o"},
			groups: []Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`)}},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("ten", time.Hour, "t", "", nil), binding("canary", 0, "", "cohort", nil),
			},
			wantKind: KindTenant, wantName: "ten", wantPrio: 200,
		}),
		Entry("collector outside the cohort is not affected", tc{
			attrs:  map[string]string{"tenant": "t", "org_id": "other"},
			groups: []Group{{Name: "cohort", Matchers: mustMatchers(`org_id="o"`)}},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("ten", time.Hour, "t", "", nil), binding("canary", 0, "", "cohort", new(int32(300))),
			},
			wantKind: KindTenant, wantName: "ten", wantPrio: 200,
		}),
		Entry("invalid matchers: group binding only reachable by legacy attribute", tc{
			attrs:  map[string]string{"org_id": "o"},
			groups: []Group{{Name: "cohort"}},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("canary", 0, "", "cohort", new(int32(300))),
			},
			hasDefault: true, wantKind: KindDefault,
		}),
		Entry("invalid matchers still allow legacy membership", tc{
			attrs:  map[string]string{"collector_group": "cohort"},
			groups: []Group{{Name: "cohort"}},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("canary", 0, "", "cohort", new(int32(300))),
			},
			wantKind: KindGroup, wantName: "canary", wantPrio: 300,
		}),
		Entry("explicit zero priority still beats the default config", tc{
			attrs:      map[string]string{"collector_group": "g"},
			bindings:   []fleetv1alpha1.CollectorGroupBinding{binding("grp", 0, "", "g", new(int32(0)))},
			hasDefault: true, wantKind: KindGroup, wantName: "grp", wantPrio: 0,
		}),
		Entry("explicit low tenant priority loses to group default", tc{
			attrs: map[string]string{"tenant": "t", "collector_group": "g"},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("ten", 0, "t", "", new(int32(50))), binding("grp", time.Hour, "", "g", nil),
			},
			wantKind: KindGroup, wantName: "grp", wantPrio: 100,
		}),
		Entry("equal priority: oldest wins", tc{
			attrs: map[string]string{"tenant": "t"},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("a", time.Hour, "t", "", nil), binding("b", 0, "t", "", nil),
			},
			wantKind: KindTenant, wantName: "b", wantPrio: 200,
		}),
		Entry("equal priority and age: lowest name wins", tc{
			attrs: map[string]string{"tenant": "t"},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("b", 0, "t", "", nil), binding("a", 0, "t", "", nil),
			},
			wantKind: KindTenant, wantName: "a", wantPrio: 200,
		}),
		Entry("two member groups tie on priority: age then name", tc{
			attrs: map[string]string{"collector_group": "g1", "org_id": "o"},
			groups: []Group{
				{Name: "g1"}, {Name: "g2", Matchers: mustMatchers(`org_id="o"`)},
			},
			bindings: []fleetv1alpha1.CollectorGroupBinding{
				binding("on-g1", time.Hour, "", "g1", nil), binding("on-g2", 0, "", "g2", nil),
			},
			wantKind: KindGroup, wantName: "on-g2", wantPrio: 100,
		}),
	)
})
