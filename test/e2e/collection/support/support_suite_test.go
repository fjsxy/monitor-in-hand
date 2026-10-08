package collectionsupport_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCollectionSupport(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Collection E2E 支撑逻辑测试套件")
}
