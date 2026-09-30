package operator_test

import (
	"context"
	"errors"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	It("[UT-BOUNDARY-GH60-003] managed informer 不读取 Agent workload 与 runtime RBAC", func() {
		By("构造生产 manager 配置并枚举 KSM controller 的 Owns 资源类型")
		options := operatorruntime.ManagerOptions(runtime.NewScheme(), ":9081")
		expectedTypes := []client.Object{
			&appsv1.Deployment{}, &appsv1.StatefulSet{},
			&corev1.Service{}, &corev1.ServiceAccount{},
			&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}, &rbacv1.Role{}, &rbacv1.RoleBinding{},
		}

		By("验证 informer 只 list/watch 本项目 managed-by 标签，不匹配 Prometheus Operator 或管理员对象")
		Expect(options.Cache.ReaderFailOnMissingInformer).To(BeTrue())
		Expect(options.Cache.ByObject).To(HaveLen(len(expectedTypes)))
		configuredTypes := map[reflect.Type]struct{}{}
		for object, byObject := range options.Cache.ByObject {
			configuredTypes[reflect.TypeOf(object)] = struct{}{}
			Expect(byObject.Label.Matches(labels.Set{"app.kubernetes.io/managed-by": "monitor-in-hand"})).To(BeTrue())
			Expect(byObject.Label.Matches(labels.Set{"app.kubernetes.io/managed-by": "prometheus-operator"})).To(BeFalse())
			Expect(byObject.Label.Matches(labels.Set{})).To(BeFalse())
		}
		for _, object := range expectedTypes {
			Expect(configuredTypes).To(HaveKey(reflect.TypeOf(object)))
		}

		By("验证控制器对自身精确规划身份使用 API 直读，以保留无标签同名冲突检测")
		Expect(options.Client.Cache).NotTo(BeNil())
		directTypes := map[reflect.Type]struct{}{}
		for _, object := range options.Client.Cache.DisableFor {
			directTypes[reflect.TypeOf(object)] = struct{}{}
		}
		for _, object := range expectedTypes {
			Expect(directTypes).To(HaveKey(reflect.TypeOf(object)))
		}
	})
})
