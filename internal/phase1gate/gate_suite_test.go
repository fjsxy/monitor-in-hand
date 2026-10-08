package phase1gate_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPhase1Gate(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "一期发布门禁")
}
