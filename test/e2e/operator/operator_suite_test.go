package operator_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestOperatorLifecycle(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Operator 故障恢复与卸载 E2E")
}
