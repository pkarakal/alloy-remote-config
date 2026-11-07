//go:build e2e

// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/pkarakal/alloy-remote-config/test/utils"
)

const (
	// testNamespace is where the chart is installed and every spec runs. The
	// manager serves a single namespace, so specs share it and isolate
	// themselves with unique names and collector attributes.
	testNamespace = "arc-e2e"
	// helmRelease and the names below follow from fullnameOverride "arc" in
	// testdata/chart-values.yaml.
	helmRelease    = "arc"
	apiServiceName = "arc-api"
	managerName    = "arc-controller-manager"
	chartDir       = "charts/fleet-management"
	chartValues    = "test/e2e/testdata/chart-values.yaml"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/alloy-remote-config:v0.0.1"
	// alloyImage is the Alloy image the specs run. It is pinned (>= v1.13) and
	// pre-loaded into Kind so specs never pull in-cluster. Override with ALLOY_IMAGE.
	alloyImage = "grafana/alloy:v1.21.3"
	// curlImage runs the in-cluster metrics request; pinned and pre-loaded like the others.
	curlImage = "curlimages/curl:8.15.0"
)

func init() {
	if v := os.Getenv("ALLOY_IMAGE"); v != "" {
		alloyImage = v
	}
}

// TestE2E runs the e2e suite against a dedicated Kind cluster: the chart is
// installed with the freshly built image and real Alloys fetch their config.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting alloy-remote-config e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// First, before anything touches a cluster: refuse any context but the
	// dedicated Kind one.
	By("verifying the current kube context is the dedicated Kind cluster")
	current, err := utils.Run(exec.Command("kubectl", "config", "current-context"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the current kube context")
	Expect(strings.TrimSpace(current)).To(Equal(kubeContext()),
		"e2e tests must only run against the dedicated Kind cluster %q", kindCluster())

	By("building the manager image")
	_, err = utils.Run(exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage)))
	Expect(err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed(), "Failed to load the manager image into Kind")

	By("pulling and loading the Alloy and curl images on Kind")
	for _, img := range []string{alloyImage, curlImage} {
		if _, err = utils.Run(exec.Command("docker", "image", "inspect", img)); err != nil {
			_, err = utils.Run(exec.Command("docker", "pull", img))
			Expect(err).NotTo(HaveOccurred(), "Failed to pull %s", img)
		}
		Expect(utils.LoadImageToKindClusterWithName(img)).To(Succeed(), "Failed to load %s into Kind", img)
	}

	By("creating the namespace with the restricted Pod Security level")
	_, err = kubectl("create", "namespace", testNamespace)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the namespace")
	_, err = kubectl("label", "--overwrite", "namespace", testNamespace,
		"pod-security.kubernetes.io/enforce=restricted")
	Expect(err).NotTo(HaveOccurred(), "Failed to label the namespace")

	By("installing the Helm chart (CRDs come from the chart's crds directory)")
	installChart()
})

// installChart installs or upgrades the release with the e2e values plus any
// extra helm arguments, then waits for the manager to be rolled out.
func installChart(extra ...string) {
	GinkgoHelper()
	repo, tag, _ := strings.Cut(managerImage, ":")
	args := append([]string{"upgrade", "--install", helmRelease, chartDir,
		"--namespace", testNamespace,
		"--values", chartValues,
		"--set", "manager.image.registry=",
		"--set", "manager.image.repository=" + repo,
		"--set", "manager.image.tag=" + tag}, extra...)
	out, err := helm(args...)
	Expect(err).NotTo(HaveOccurred(), out)

	out, err = kubectl("rollout", "status", "deployment/"+managerName, "-n", testNamespace, "--timeout=180s")
	Expect(err).NotTo(HaveOccurred(), out)
}

var _ = AfterSuite(func() {
	// Best effort: the Makefile deletes the whole cluster, but a reused
	// cluster should not keep the chart or CRDs behind.
	By("removing fleet resources, the chart, the CRDs and the namespace")
	for _, kind := range []string{"collectorgroupbindings", "collectorgroups", "pipelineconfigs"} {
		_, _ = kubectl("delete", kind+".fleet.pkarakal.com", "--all", "-n", testNamespace,
			"--wait=true", "--timeout=60s")
	}
	_, _ = helm("uninstall", helmRelease, "--namespace", testNamespace)
	_, _ = kubectl("delete", "crd", "--ignore-not-found",
		"collectorgroupbindings.fleet.pkarakal.com",
		"collectorgroups.fleet.pkarakal.com",
		"pipelineconfigs.fleet.pkarakal.com")
	_, _ = kubectl("delete", "namespace", testNamespace, "--ignore-not-found", "--wait=false")
})

// Dump diagnostics on failure. JustAfterEach runs before DeferCleanup, so the
// spec's objects still exist.
var _ = JustAfterEach(func() {
	if !CurrentSpecReport().Failed() {
		return
	}
	dump := func(title string, args ...string) {
		out, err := kubectl(args...)
		if err != nil {
			out += "\n(error: " + err.Error() + ")"
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "\n===== %s =====\n%s\n", title, out)
	}
	dump("manager logs", "logs", "-n", testNamespace, "deployment/"+managerName, "--tail=300")
	dump("alloy logs", "logs", "-n", testNamespace, "-l", "e2e-alloy", "--all-containers", "--tail=200", "--prefix")
	for _, kind := range []string{"pipelineconfigs", "collectorgroups", "collectorgroupbindings"} {
		dump(kind, "get", kind+".fleet.pkarakal.com", "-n", testNamespace, "-o", "yaml")
	}
	dump("pods", "get", "pods", "-n", testNamespace, "-o", "wide")
	dump("events", "get", "events", "-n", testNamespace, "--sort-by=.lastTimestamp")
})
