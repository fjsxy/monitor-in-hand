package operator_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"

	operatorruntime "github.com/fjsxy/monitor-in-hand/internal/operator"
)

type fakeServerVersioner struct {
	err error
}

func (f fakeServerVersioner) ServerVersion() (*version.Info, error) {
	return &version.Info{GitVersion: "v1.28.15"}, f.err
}

var _ = Describe("manager readiness", func() {
	It("始终启用 Lease leader election 并关闭无关端点", func() {
		options := operatorruntime.ManagerOptions(runtime.NewScheme(), ":9081")

		Expect(options.LeaderElection).To(BeTrue())
		Expect(options.LeaderElectionResourceLock).To(Equal("leases"))
		Expect(options.LeaderElectionID).To(Equal("monitor-in-hand.monitoring.fjsxy.github.io"))
		Expect(options.LeaderElectionReleaseOnCancel).To(BeTrue())
		Expect(options.HealthProbeBindAddress).To(Equal(":9081"))
		Expect(options.Metrics.BindAddress).To(Equal("0"))
		Expect(options.PprofBindAddress).To(Equal("0"))
	})

	It("要求 manager 已启动且 Kubernetes API 可访问", func() {
		state := operatorruntime.NewParticipationState()
		Expect(state.NeedLeaderElection()).To(BeFalse())
		check := operatorruntime.NewReadyCheck(fakeServerVersioner{}, state)

		Expect(check(nil)).To(MatchError(ContainSubstring("leader election")))

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- state.Start(ctx) }()
		Eventually(func() error { return check(nil) }).Should(Succeed())
		cancel()
		Eventually(done).Should(Receive(BeNil()))
	})

	It("Kubernetes API 不可访问时变为 unready", func() {
		state := operatorruntime.NewParticipationState()
		state.MarkParticipating()
		check := operatorruntime.NewReadyCheck(
			fakeServerVersioner{err: errors.New("connection refused")},
			state,
		)

		Expect(check(nil)).To(MatchError(ContainSubstring("connection refused")))
	})
})
