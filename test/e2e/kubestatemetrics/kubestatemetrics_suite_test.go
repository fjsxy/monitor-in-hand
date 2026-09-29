package kubestatemetrics_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestKubeStateMetricsE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KubeStateMetrics E2E Suite")
}
