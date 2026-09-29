package preflight_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDeploymentPreflightE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "严格部署预检 E2E 测试套件")
}
