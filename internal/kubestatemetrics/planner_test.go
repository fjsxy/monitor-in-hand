package kubestatemetrics_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
)

var _ = Describe("默认 Deployment 规划", func() {
	newInstance := func() *monitoringv1alpha1.KubeStateMetrics {
		return &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "primary", UID: types.UID("uid-51")},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: "monitoring"},
		}
	}

	DescribeTable("UT-KSM-001 为目标 Kubernetes minor 选择固定 profile 并规划完整资源",
		func(kubernetesVersion, wantVersion, wantImage string, wantRBACResource string, hardened bool) {
			plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{
				KubernetesVersion:     kubernetesVersion,
				TargetNamespaceExists: true,
			})

			Expect(problem).To(BeNil())
			Expect(plan.Profile.Version).To(Equal(wantVersion))
			Expect(plan.Profile.Image).To(Equal(wantImage))
			Expect(plan.InstanceID).To(Equal("primary"))
			Expect(plan.Name).To(Equal("ksm-primary"))
			Expect(plan.Resources).To(HaveLen(5))

			var deployment *appsv1.Deployment
			var service *corev1.Service
			var serviceAccount *corev1.ServiceAccount
			var clusterRole *rbacv1.ClusterRole
			var clusterRoleBinding *rbacv1.ClusterRoleBinding
			for _, resource := range plan.Resources {
				switch typed := resource.(type) {
				case *appsv1.Deployment:
					deployment = typed
				case *corev1.Service:
					service = typed
				case *corev1.ServiceAccount:
					serviceAccount = typed
				case *rbacv1.ClusterRole:
					clusterRole = typed
				case *rbacv1.ClusterRoleBinding:
					clusterRoleBinding = typed
				}
			}

			Expect(deployment).NotTo(BeNil())
			Expect(deployment.Namespace).To(Equal("monitoring"))
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(wantImage))
			Expect(container.Args).To(BeEmpty())
			Expect(container.Ports).To(ConsistOf(
				corev1.ContainerPort{Name: "http-metrics", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
				corev1.ContainerPort{Name: "telemetry", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
			))
			Expect(container.LivenessProbe.HTTPGet).To(SatisfyAll(HaveField("Path", "/healthz"), HaveField("Port.StrVal", "http-metrics")))
			Expect(container.ReadinessProbe.HTTPGet).To(SatisfyAll(HaveField("Path", "/"), HaveField("Port.StrVal", "telemetry")))
			for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe} {
				Expect(probe).To(SatisfyAll(
					HaveField("InitialDelaySeconds", int32(5)),
					HaveField("TimeoutSeconds", int32(5)),
					HaveField("PeriodSeconds", int32(10)),
					HaveField("SuccessThreshold", int32(1)),
					HaveField("FailureThreshold", int32(3)),
				))
			}
			Expect(container.TerminationMessagePath).To(Equal(corev1.TerminationMessagePathDefault))
			Expect(container.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageReadFile))
			Expect(container.SecurityContext.RunAsUser).NotTo(BeNil())
			Expect(*container.SecurityContext.RunAsUser).To(Equal(int64(65534)))
			if hardened {
				Expect(serviceAccount.AutomountServiceAccountToken).NotTo(BeNil())
				Expect(*serviceAccount.AutomountServiceAccountToken).To(BeFalse())
				Expect(deployment.Spec.Template.Spec.AutomountServiceAccountToken).NotTo(BeNil())
				Expect(*deployment.Spec.Template.Spec.AutomountServiceAccountToken).To(BeTrue())
				Expect(container.SecurityContext).To(SatisfyAll(
					HaveField("AllowPrivilegeEscalation", Not(BeNil())),
					HaveField("ReadOnlyRootFilesystem", Not(BeNil())),
					HaveField("RunAsNonRoot", Not(BeNil())),
					HaveField("Capabilities.Drop", ConsistOf(corev1.Capability("ALL"))),
					HaveField("SeccompProfile.Type", corev1.SeccompProfileTypeRuntimeDefault),
				))
			} else {
				Expect(serviceAccount.AutomountServiceAccountToken).To(BeNil())
				Expect(deployment.Spec.Template.Spec.AutomountServiceAccountToken).To(BeNil())
				Expect(container.SecurityContext.AllowPrivilegeEscalation).To(BeNil())
			}
			Expect(service).NotTo(BeNil())
			Expect(service.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(serviceAccount).NotTo(BeNil())
			Expect(clusterRole).NotTo(BeNil())
			Expect(clusterRole.Rules).To(ContainElement(HaveField("Resources", ContainElement(wantRBACResource))))
			Expect(clusterRoleBinding).NotTo(BeNil())
			Expect(clusterRoleBinding.Subjects).To(ContainElement(SatisfyAll(
				HaveField("Name", "ksm-primary"),
				HaveField("Namespace", "monitoring"),
			)))

			for _, resource := range plan.Resources {
				Expect(resource.GetLabels()).To(SatisfyAll(
					HaveKeyWithValue("app.kubernetes.io/name", "kube-state-metrics"),
					HaveKeyWithValue("app.kubernetes.io/instance", "primary"),
					HaveKeyWithValue("app.kubernetes.io/managed-by", "monitor-in-hand"),
					HaveKeyWithValue("app.kubernetes.io/version", wantVersion),
				))
				owners := resource.GetOwnerReferences()
				Expect(owners).To(HaveLen(1))
				Expect(owners[0]).To(SatisfyAll(
					HaveField("APIVersion", monitoringv1alpha1.GroupVersion.String()),
					HaveField("Kind", "KubeStateMetrics"),
					HaveField("Name", "primary"),
					HaveField("UID", types.UID("uid-51")),
				))
				Expect(owners[0].Controller).NotTo(BeNil())
				Expect(*owners[0].Controller).To(BeTrue())
			}
		},
		Entry("Kubernetes 1.19", "v1.19.16", "2.3.0", "k8s.gcr.io/kube-state-metrics/kube-state-metrics:v2.3.0", "endpoints", false),
		Entry("Kubernetes 1.28", "v1.28.15", "2.11.0", "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.11.0", "endpointslices", true),
	)

	It("UT-KSM-002 对未知 minor 返回 UnsupportedKubernetesVersion 且零资源", func() {
		plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{
			KubernetesVersion:     "v1.27.9",
			TargetNamespaceExists: true,
		})

		Expect(plan).To(BeNil())
		Expect(problem).To(SatisfyAll(
			HaveField("Reason", "UnsupportedKubernetesVersion"),
			HaveField("Message", ContainSubstring("1.27")),
		))
	})

	It("UT-KSM-003 对缺失目标 Namespace 返回 DependencyMissing 且零资源", func() {
		plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{KubernetesVersion: "v1.28.15"})

		Expect(plan).To(BeNil())
		Expect(problem).To(SatisfyAll(
			HaveField("Reason", "DependencyMissing"),
			HaveField("Message", ContainSubstring("monitoring")),
		))
	})

	It("UT-KSM-004 首次应用后拒绝 targetNamespace 变化", func() {
		instance := newInstance()
		instance.Status.AppliedTargetNamespace = "previous"
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
			KubernetesVersion:     "v1.28.15",
			TargetNamespaceExists: true,
		})

		Expect(plan).To(BeNil())
		Expect(problem).To(SatisfyAll(
			HaveField("Reason", "InvalidSpec"),
			HaveField("Message", ContainSubstring("previous")),
		))
	})
})
