package testenv_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDependencyBaselineCLI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "测试依赖基线 CLI 契约")
}
