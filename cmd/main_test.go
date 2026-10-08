package main

import (
	"flag"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("manager leader election CLI", func() {
	DescribeTable("保持兼容入口且始终启用 leader election",
		func(arguments []string) {
			options := defaultCLIOptions()
			flags := flag.NewFlagSet("manager", flag.ContinueOnError)
			options.bindFlags(flags)

			Expect(flags.Parse(arguments)).To(Succeed())
			Expect(options.validate()).To(Succeed())
			Expect(options.leaderElection).To(BeTrue())
		},
		Entry("default remains enabled", nil),
		Entry("bare scaffold flag", []string{"--leader-elect"}),
		Entry("explicit true", []string{"--leader-elect=true"}),
	)

	It("拒绝关闭 leader election", func() {
		options := defaultCLIOptions()
		flags := flag.NewFlagSet("manager", flag.ContinueOnError)
		options.bindFlags(flags)

		Expect(flags.Parse([]string{"--leader-elect=false"})).To(Succeed())
		Expect(options.validate()).To(MatchError(ContainSubstring("--leader-elect=false")))
	})
})
