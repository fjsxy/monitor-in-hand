package helm_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestHelmChart(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Helm Chart 测试套件")
}
