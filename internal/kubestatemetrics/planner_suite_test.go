package kubestatemetrics_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestKubeStateMetricsPlanning(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KubeStateMetrics Planning 测试套件")
}
