package kubestatemetricssupport_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestKubeStateMetricsE2ESupport(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KubeStateMetrics E2E 支撑逻辑测试套件")
}
