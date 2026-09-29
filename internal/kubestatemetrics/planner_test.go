package kubestatemetrics_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

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

	It("UT-KSM-GH53-001 从 CR name 派生稳定且可冲突规避的实例身份", func() {
		By("验证未转换的短名保持可读身份")
		Expect(ksm.ManagedResourceName("primary-example")).To(Equal("ksm-primary-example"))

		By("验证点号转换使用原始 CR name 的 SHA-256 前 8 位区分潜在重名")
		Expect(ksm.ManagedResourceName("primary.example")).To(Equal("ksm-primary-example-16ac24d9"))
		Expect(ksm.ManagedResourceName("primary.example")).NotTo(Equal(ksm.ManagedResourceName("primary-example")))

		By("验证超长名称保留固定可读前缀和原始名称摘要")
		name := "abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuv"
		Expect(ksm.ManagedResourceName(name)).To(Equal("ksm-abcdefghijklmnopqrstuvwxyz-abcdefg-5e100601"))
		Expect(ksm.ManagedResourceName(name)).To(HaveLen(47))

		By("验证 targetNamespace 不参与实例身份计算")
		instance := newInstance()
		first, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"monitoring"}})
		Expect(problem).To(BeNil())
		instance.Spec.TargetNamespace = "replacement"
		second, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"replacement"}})
		Expect(problem).To(BeNil())
		Expect(second.InstanceID).To(Equal(first.InstanceID))
		Expect(second.Name).To(Equal(first.Name))
	})

	It("UT-KSM-GH53-002 为全部直接受管资源和 Pod 下发稳定标签与版本无关的 Service selector", func() {
		By("执行最小受管实例的纯规划")
		plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{
			KubernetesVersion:     "v1.28.15",
			TargetNamespaceExists: true,
			NamespaceNames:        []string{"monitoring"},
		})
		Expect(problem).To(BeNil())

		By("验证全部直接受管资源使用完整稳定标签")
		wantLabels := map[string]string{
			"app.kubernetes.io/name":         "kube-state-metrics",
			"app.kubernetes.io/instance":     "primary",
			"app.kubernetes.io/managed-by":   "monitor-in-hand",
			"app.kubernetes.io/version":      "2.11.0",
			"monitor-in-hand/role":           "collector",
			"monitor-in-hand/endpoint-group": "default",
		}
		for _, resource := range plan.Resources {
			Expect(resource.GetLabels()).To(Equal(wantLabels))
		}

		By("验证 Pod 继承完整标签，Service selector 只使用不随版本和角色变化的身份")
		deployment := deploymentFrom(plan)
		Expect(deployment.Spec.Template.Labels).To(Equal(wantLabels))
		service := serviceFrom(plan)
		Expect(service.Spec.Selector).To(Equal(map[string]string{
			"app.kubernetes.io/name":         "kube-state-metrics",
			"app.kubernetes.io/instance":     "primary",
			"monitor-in-hand/endpoint-group": "default",
		}))
		Expect(service.Spec.Selector).NotTo(HaveKey("app.kubernetes.io/version"))
		Expect(service.Spec.Selector).NotTo(HaveKey("monitor-in-hand/role"))
	})

	DescribeTable("UT-KSM-001 为目标 Kubernetes minor 选择固定 profile 并规划完整资源",
		func(kubernetesVersion, wantVersion, wantImage string, wantRBACResource string, hardened bool) {
			plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{
				KubernetesVersion:     kubernetesVersion,
				TargetNamespaceExists: true,
				NamespaceNames:        []string{"monitoring"},
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
			Expect(container.Args).To(Equal([]string{"--resources=" + strings.Join(plan.EffectiveCollectors, ","), "--namespaces=monitoring"}))
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

	DescribeTable("UT-KSM-006 默认 collectors 固定为两个版本共同的 28 项 catalog", func(kubernetesVersion string) {
		By("执行最小声明的纯规划")
		plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{
			KubernetesVersion:     kubernetesVersion,
			TargetNamespaceExists: true,
			NamespaceNames:        []string{"monitoring"},
		})

		By("验证固定 catalog、实际参数与省略资源的行为")
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveCollectors).To(Equal([]string{
			"certificatesigningrequests", "configmaps", "cronjobs", "daemonsets", "deployments", "endpoints",
			"horizontalpodautoscalers", "ingresses", "jobs", "leases", "limitranges", "mutatingwebhookconfigurations",
			"namespaces", "networkpolicies", "nodes", "persistentvolumeclaims", "persistentvolumes", "poddisruptionbudgets",
			"pods", "replicasets", "replicationcontrollers", "resourcequotas", "secrets", "services", "statefulsets",
			"storageclasses", "validatingwebhookconfigurations", "volumeattachments",
		}))
		deployment := deploymentFrom(plan)
		Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(Equal([]string{
			"--resources=" + strings.Join(plan.EffectiveCollectors, ","),
			"--namespaces=monitoring",
		}))
		Expect(deployment.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{}))
	},
		Entry("Kubernetes 1.19", "v1.19.16"),
		Entry("Kubernetes 1.28", "v1.28.15"),
	)

	DescribeTable("UT-KSM-008 Include collectors 成为实际参数和 effective collectors", func(kubernetesVersion string) {
		By("准备显式 Include collector 声明")
		instance := newInstance()
		instance.Spec.Collectors = monitoringv1alpha1.KubeStateMetricsCollectors{
			Mode:  monitoringv1alpha1.CollectorModeInclude,
			Names: []string{"pods", "deployments"},
		}

		By("执行纯规划")
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
			KubernetesVersion:     kubernetesVersion,
			TargetNamespaceExists: true,
			NamespaceNames:        []string{"monitoring"},
		})

		By("验证实际参数和 effective collectors 来自同一解析结果")
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveCollectors).To(Equal([]string{"pods", "deployments"}))
		Expect(deploymentFrom(plan).Spec.Template.Spec.Containers[0].Args).To(Equal([]string{"--resources=pods,deployments", "--namespaces=monitoring"}))
	},
		Entry("Kubernetes 1.19", "v1.19.16"),
		Entry("Kubernetes 1.28", "v1.28.15"),
	)

	DescribeTable("UT-KSM-009 非法 collector 配置在规划资源前返回 InvalidSpec",
		func(collectors monitoringv1alpha1.KubeStateMetricsCollectors, wantMessage string) {
			By("准备非法 collector 声明")
			instance := newInstance()
			instance.Spec.Collectors = collectors
			expectInvalidSpecAcrossProfiles(instance, wantMessage)
		},
		Entry("Default 不接受 names", monitoringv1alpha1.KubeStateMetricsCollectors{
			Mode: monitoringv1alpha1.CollectorModeDefault, Names: []string{"pods"},
		}, "Default"),
		Entry("Include 要求 names 非空", monitoringv1alpha1.KubeStateMetricsCollectors{
			Mode: monitoringv1alpha1.CollectorModeInclude,
		}, "非空"),
		Entry("Include 不接受重复项", monitoringv1alpha1.KubeStateMetricsCollectors{
			Mode: monitoringv1alpha1.CollectorModeInclude, Names: []string{"pods", "pods"},
		}, "重复"),
		Entry("Include 不接受共同 catalog 外的名称", monitoringv1alpha1.KubeStateMetricsCollectors{
			Mode: monitoringv1alpha1.CollectorModeInclude, Names: []string{"serviceaccounts"},
		}, "serviceaccounts"),
	)

	DescribeTable("UT-KSM-010 metric filter 映射为对应的 kube-state-metrics 参数",
		func(kubernetesVersion string, mode monitoringv1alpha1.MetricFilterMode, patterns []string, wantFilterArg string) {
			By("准备显式 collector 和 metric filter")
			instance := newInstance()
			instance.Spec.Collectors = monitoringv1alpha1.KubeStateMetricsCollectors{
				Mode: monitoringv1alpha1.CollectorModeInclude, Names: []string{"pods"},
			}
			instance.Spec.MetricFilter = monitoringv1alpha1.KubeStateMetricsMetricFilter{Mode: mode, Patterns: patterns}

			By("执行纯规划")
			plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
				KubernetesVersion:     kubernetesVersion,
				TargetNamespaceExists: true,
				NamespaceNames:        []string{"monitoring"},
			})

			By("验证 filter 被原样映射到对应参数")
			Expect(problem).To(BeNil())
			Expect(deploymentFrom(plan).Spec.Template.Spec.Containers[0].Args).To(Equal([]string{
				"--resources=pods",
				wantFilterArg,
				"--namespaces=monitoring",
			}))
		},
		Entry("Kubernetes 1.19 Allow 保留未锚定和合法零命中 pattern", "v1.19.16", monitoringv1alpha1.MetricFilterModeAllow,
			[]string{"pod_info", "^definitely_no_metric$"}, "--metric-allowlist=pod_info,^definitely_no_metric$"),
		Entry("Kubernetes 1.28 Allow 保留未锚定和合法零命中 pattern", "v1.28.15", monitoringv1alpha1.MetricFilterModeAllow,
			[]string{"pod_info", "^definitely_no_metric$"}, "--metric-allowlist=pod_info,^definitely_no_metric$"),
		Entry("Kubernetes 1.19 Allow 保留首尾空白 regexp 的零命中语义", "v1.19.16", monitoringv1alpha1.MetricFilterModeAllow,
			[]string{" pod_info ", "   "}, "--metric-allowlist=( pod_info ),(   )"),
		Entry("Kubernetes 1.28 Allow 保留首尾空白 regexp 的零命中语义", "v1.28.15", monitoringv1alpha1.MetricFilterModeAllow,
			[]string{" pod_info ", "   "}, "--metric-allowlist=( pod_info ),(   )"),
		Entry("Kubernetes 1.19 Deny", "v1.19.16", monitoringv1alpha1.MetricFilterModeDeny,
			[]string{"kube_pod_info", "_labels$"}, "--metric-denylist=kube_pod_info,_labels$"),
		Entry("Kubernetes 1.28 Deny", "v1.28.15", monitoringv1alpha1.MetricFilterModeDeny,
			[]string{"kube_pod_info", "_labels$"}, "--metric-denylist=kube_pod_info,_labels$"),
	)

	DescribeTable("UT-KSM-011 非法 metric filter 在规划资源前返回 InvalidSpec",
		func(filter monitoringv1alpha1.KubeStateMetricsMetricFilter, wantMessage string) {
			By("准备非法 metric filter 声明")
			instance := newInstance()
			instance.Spec.MetricFilter = filter
			expectInvalidSpecAcrossProfiles(instance, wantMessage)
		},
		Entry("All 不接受 patterns", monitoringv1alpha1.KubeStateMetricsMetricFilter{
			Mode: monitoringv1alpha1.MetricFilterModeAll, Patterns: []string{"kube_pod_info"},
		}, "All"),
		Entry("Allow 要求 patterns 非空", monitoringv1alpha1.KubeStateMetricsMetricFilter{
			Mode: monitoringv1alpha1.MetricFilterModeAllow,
		}, "非空"),
		Entry("Deny 要求 patterns 非空", monitoringv1alpha1.KubeStateMetricsMetricFilter{
			Mode: monitoringv1alpha1.MetricFilterModeDeny,
		}, "非空"),
		Entry("拒绝空 pattern", monitoringv1alpha1.KubeStateMetricsMetricFilter{
			Mode: monitoringv1alpha1.MetricFilterModeAllow, Patterns: []string{"kube_pod_info", ""},
		}, "空项"),
		Entry("拒绝非法正则表达式", monitoringv1alpha1.KubeStateMetricsMetricFilter{
			Mode: monitoringv1alpha1.MetricFilterModeAllow, Patterns: []string{"["},
		}, "非法"),
		Entry("拒绝包含逗号的 pattern", monitoringv1alpha1.KubeStateMetricsMetricFilter{
			Mode: monitoringv1alpha1.MetricFilterModeDeny, Patterns: []string{"kube_(pod|node){1,2}"},
		}, "逗号"),
	)

	DescribeTable("UT-KSM-012 显式 podResources 完整下发且不伪造默认值", func(kubernetesVersion string) {
		By("准备显式 requests 和 limits")
		instance := newInstance()
		instance.Spec.PodResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("250m"),
				corev1.ResourceMemory:           resource.MustParse("256Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
			},
		}

		By("执行纯规划")
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
			KubernetesVersion:     kubernetesVersion,
			TargetNamespaceExists: true,
			NamespaceNames:        []string{"monitoring"},
		})

		By("验证 ResourceRequirements 未经改写地下发")
		Expect(problem).To(BeNil())
		Expect(deploymentFrom(plan).Spec.Template.Spec.Containers[0].Resources).To(Equal(*instance.Spec.PodResources))
	},
		Entry("Kubernetes 1.19", "v1.19.16"),
		Entry("Kubernetes 1.28", "v1.28.15"),
	)

	It("UT-KSM-013 在规划资源前拒绝缺少 Pod resourceClaims 声明的 claim", func() {
		By("准备引用未开放 Pod resourceClaims 的容器资源声明")
		instance := newInstance()
		instance.Spec.PodResources = &corev1.ResourceRequirements{
			Claims: []corev1.ResourceClaim{{Name: "accelerator"}},
		}

		expectInvalidSpecAcrossProfiles(instance, "podResources.claims")
	})

	It("UT-KSM-002 对未知 minor 返回 UnsupportedKubernetesVersion 且零资源", func() {
		plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{
			KubernetesVersion:     "v1.27.9",
			TargetNamespaceExists: true,
			NamespaceNames:        []string{"monitoring"},
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
			NamespaceNames:        []string{"monitoring"},
		})

		Expect(plan).To(BeNil())
		Expect(problem).To(SatisfyAll(
			HaveField("Reason", "InvalidSpec"),
			HaveField("Message", ContainSubstring("previous")),
		))
	})

	It("UT-KSM-005 Operator RBAC 覆盖两个 profile 下放的全部权限", func() {
		manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
		Expect(err).NotTo(HaveOccurred())
		managerRole := &rbacv1.ClusterRole{}
		Expect(yaml.Unmarshal(manifest, managerRole)).To(Succeed())

		for _, version := range []string{"v1.19.16", "v1.28.15"} {
			plan, problem := ksm.BuildPlan(newInstance(), ksm.ClusterFacts{
				KubernetesVersion:     version,
				TargetNamespaceExists: true,
				NamespaceNames:        []string{"monitoring"},
			})
			Expect(problem).To(BeNil())
			for _, delegated := range plan.Profile.Rules {
				for _, apiGroup := range delegated.APIGroups {
					for _, resource := range delegated.Resources {
						for _, verb := range delegated.Verbs {
							Expect(policyRulesAllow(managerRole.Rules, apiGroup, resource, verb)).To(
								BeTrue(), "Operator RBAC 未覆盖 Kubernetes %s 下放权限 %s %s/%s", version, verb, apiGroup, resource)
						}
					}
				}
			}
		}
	})
})

var _ = Describe("StatefulSetSharded 判别 union", func() {
	DescribeTable("UT-KSM-GH56-001 拒绝分支与 mode 不匹配或分片数不足的声明",
		func(deployment monitoringv1alpha1.KubeStateMetricsDeploymentSpec, wantMessage string) {
			By("准备非法的部署模式判别 union")
			instance := &monitoringv1alpha1.KubeStateMetrics{
				ObjectMeta: metav1.ObjectMeta{Name: "invalid-sharding", UID: types.UID("uid-56-invalid")},
				Spec: monitoringv1alpha1.KubeStateMetricsSpec{
					TargetNamespace: "monitoring",
					Deployment:      deployment,
				},
			}

			expectInvalidSpecAcrossProfiles(instance, wantMessage)
		},
		Entry("Deployment 不接受 StatefulSetSharded 分支", monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
			Mode: monitoringv1alpha1.DeploymentModeDeployment,
			StatefulSetSharded: &monitoringv1alpha1.KubeStateMetricsStatefulSetShardedSpec{
				Shards: 2,
			},
		}, "statefulSetSharded"),
		Entry("StatefulSetSharded 要求对应分支", monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
			Mode: monitoringv1alpha1.DeploymentModeStatefulSetSharded,
		}, "statefulSetSharded"),
		Entry("StatefulSetSharded 至少两个分片", monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
			Mode: monitoringv1alpha1.DeploymentModeStatefulSetSharded,
			StatefulSetSharded: &monitoringv1alpha1.KubeStateMetricsStatefulSetShardedSpec{
				Shards: 1,
			},
		}, "至少为 2"),
	)

	DescribeTable("UT-KSM-GH56-002 为两个版本规划统一的自动分片资源",
		func(kubernetesVersion string) {
			By("准备显式 collector、metric filter、namespace 和 resources 的分片声明")
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			}
			instance := &monitoringv1alpha1.KubeStateMetrics{
				ObjectMeta: metav1.ObjectMeta{Name: "sharded", UID: types.UID("uid-56-sharded")},
				Spec: monitoringv1alpha1.KubeStateMetricsSpec{
					TargetNamespace: "monitoring",
					Deployment: monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
						Mode: monitoringv1alpha1.DeploymentModeStatefulSetSharded,
						StatefulSetSharded: &monitoringv1alpha1.KubeStateMetricsStatefulSetShardedSpec{
							Shards: 3,
						},
					},
					Collectors: monitoringv1alpha1.KubeStateMetricsCollectors{
						Mode: monitoringv1alpha1.CollectorModeInclude, Names: []string{"pods", "deployments"},
					},
					MetricFilter: monitoringv1alpha1.KubeStateMetricsMetricFilter{
						Mode: monitoringv1alpha1.MetricFilterModeAllow, Patterns: []string{"kube_pod_info"},
					},
					NamespaceFilter: monitoringv1alpha1.NamespaceFilter{
						Mode: monitoringv1alpha1.NamespaceFilterModeInclude, Names: []string{"team-a", "team-b"},
					},
					PodResources: resources,
				},
			}

			By("执行纯规划并验证七类目标资源")
			plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
				KubernetesVersion:     kubernetesVersion,
				TargetNamespaceExists: true,
				NamespaceNames:        []string{"monitoring", "team-a", "team-b"},
			})
			Expect(problem).To(BeNil())
			Expect(plan.Mode).To(Equal(monitoringv1alpha1.DeploymentModeStatefulSetSharded))
			Expect(plan.DesiredPods).To(Equal(int32(3)))
			Expect(plan.Resources).To(HaveLen(7))

			statefulSet := statefulSetFrom(plan)
			Expect(*statefulSet.Spec.Replicas).To(Equal(int32(3)))
			Expect(statefulSet.Spec.ServiceName).To(Equal("ksm-sharded"))
			container := statefulSet.Spec.Template.Spec.Containers[0]
			Expect(container.Args).To(Equal([]string{
				"--resources=pods,deployments",
				"--metric-allowlist=kube_pod_info",
				"--namespaces=team-a,team-b",
				"--pod=$(POD_NAME)",
				"--pod-namespace=$(POD_NAMESPACE)",
			}))
			Expect(container.Env).To(ConsistOf(
				corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
				corev1.EnvVar{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			))
			Expect(container.Resources).To(Equal(*resources))

			service := resourceOfType[*corev1.Service](plan)
			Expect(service.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(service.Spec.Selector).To(SatisfyAll(
				HaveKeyWithValue("app.kubernetes.io/instance", "sharded"),
				HaveKeyWithValue("monitor-in-hand/endpoint-group", "sharded"),
				Not(HaveKey("app.kubernetes.io/version")),
			))
			role := resourceOfType[*rbacv1.Role](plan)
			Expect(role.Namespace).To(Equal("monitoring"))
			Expect(role.Rules).To(ConsistOf(
				rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
				rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"statefulsets"}, ResourceNames: []string{"ksm-sharded"}, Verbs: []string{"get"}},
			))
			roleBinding := resourceOfType[*rbacv1.RoleBinding](plan)
			Expect(roleBinding.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "ksm-sharded"}))
			Expect(roleBinding.Subjects).To(Equal([]rbacv1.Subject{{Kind: "ServiceAccount", Name: "ksm-sharded", Namespace: "monitoring"}}))
		},
		Entry("Kubernetes 1.19", "v1.19.16"),
		Entry("Kubernetes 1.28", "v1.28.15"),
	)
})

var _ = Describe("受管实例 phase 推导", func() {
	disabled := false
	condition := func(conditionType string, status metav1.ConditionStatus, reasons ...string) metav1.Condition {
		reason := "Test"
		if len(reasons) > 0 {
			reason = reasons[0]
		}
		return metav1.Condition{Type: conditionType, Status: status, Reason: reason, LastTransitionTime: metav1.Now()}
	}

	DescribeTable("UT-KSM-007 按生命周期优先级从删除状态与 conditions 推导 phase",
		func(instance *monitoringv1alpha1.KubeStateMetrics, want monitoringv1alpha1.KubeStateMetricsPhase) {
			Expect(ksm.DerivePhase(instance)).To(Equal(want))
		},
		Entry("删除不可被其他状态覆盖", &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time}},
			Status: monitoringv1alpha1.KubeStateMetricsStatus{Conditions: []metav1.Condition{
				condition("Degraded", metav1.ConditionTrue),
			}},
		}, monitoringv1alpha1.KubeStateMetricsPhaseDeleting),
		Entry("停用中的失败仍为 Disabling", &monitoringv1alpha1.KubeStateMetrics{
			Status: monitoringv1alpha1.KubeStateMetricsStatus{Conditions: []metav1.Condition{
				condition("Progressing", metav1.ConditionTrue, ksm.ReasonDisabling),
				condition("Degraded", metav1.ConditionTrue),
			}},
		}, monitoringv1alpha1.KubeStateMetricsPhaseDisabling),
		Entry("停用完成", &monitoringv1alpha1.KubeStateMetrics{
			Status: monitoringv1alpha1.KubeStateMetricsStatus{Conditions: []metav1.Condition{
				condition("Ready", metav1.ConditionFalse, ksm.ReasonDisabled),
			}},
		}, monitoringv1alpha1.KubeStateMetricsPhaseDisabled),
		Entry("启用实例优先报告故障", &monitoringv1alpha1.KubeStateMetrics{
			Status: monitoringv1alpha1.KubeStateMetricsStatus{Conditions: []metav1.Condition{
				condition("Degraded", metav1.ConditionTrue),
				condition("Progressing", metav1.ConditionTrue),
				condition("Ready", metav1.ConditionTrue),
			}},
		}, monitoringv1alpha1.KubeStateMetricsPhaseDegraded),
		Entry("无故障时优先报告调谐中", &monitoringv1alpha1.KubeStateMetrics{
			Status: monitoringv1alpha1.KubeStateMetricsStatus{Conditions: []metav1.Condition{
				condition("Progressing", metav1.ConditionTrue),
				condition("Ready", metav1.ConditionTrue),
			}},
		}, monitoringv1alpha1.KubeStateMetricsPhaseReconciling),
		Entry("已就绪", &monitoringv1alpha1.KubeStateMetrics{
			Status: monitoringv1alpha1.KubeStateMetricsStatus{Conditions: []metav1.Condition{
				condition("Ready", metav1.ConditionTrue),
			}},
		}, monitoringv1alpha1.KubeStateMetricsPhaseReady),
		Entry("spec 停用但尚无已观察 conditions", &monitoringv1alpha1.KubeStateMetrics{
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{Enabled: &disabled},
		}, monitoringv1alpha1.KubeStateMetricsPhasePending),
	)
})

var _ = Describe("Namespace 采集范围规划", func() {
	It("UT-KSM-019 随 Namespace 集合增删在零匹配与恢复之间重新规划", func() {
		By("准备只匹配 team Namespace 的受管实例")
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", UID: types.UID("uid-55-dynamic")},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: "monitoring",
				NamespaceFilter: monitoringv1alpha1.NamespaceFilter{
					Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"team-[ab]"},
				},
			},
		}
		facts := ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"monitoring"}}

		By("验证零匹配、扩展、归零和恢复都来自同一份 Namespace 集合")
		plan, problem := ksm.BuildPlan(instance, facts)
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveNamespaces).To(BeEmpty())
		Expect(plan.Resources).To(HaveLen(4))

		facts.NamespaceNames = append(facts.NamespaceNames, "team-a", "team-b")
		plan, problem = ksm.BuildPlan(instance, facts)
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveNamespaces).To(Equal([]string{"team-a", "team-b"}))
		Expect(deploymentFrom(plan).Spec.Template.Spec.Containers[0].Args).To(ContainElement("--namespaces=team-a,team-b"))

		facts.NamespaceNames = []string{"monitoring"}
		plan, problem = ksm.BuildPlan(instance, facts)
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveNamespaces).To(BeEmpty())
		Expect(plan.Resources).To(HaveLen(4))

		facts.NamespaceNames = append(facts.NamespaceNames, "team-b")
		plan, problem = ksm.BuildPlan(instance, facts)
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveNamespaces).To(Equal([]string{"team-b"}))
	})

	It("UT-KSM-018 即使目标 Namespace 缺失也优先把非法 regex 报告为 InvalidSpec", func() {
		By("准备同时包含静态错误和缺失依赖的声明")
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid-before-dependency", UID: types.UID("uid-55-order")},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: "missing",
				NamespaceFilter: monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"["}},
			},
		}

		By("验证静态 InvalidSpec 优先且零资源")
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{KubernetesVersion: "v1.28.15"})
		Expect(plan).To(BeNil())
		Expect(problem).To(SatisfyAll(HaveField("Reason", ksm.ReasonInvalidSpec), HaveField("Message", ContainSubstring("["))))
	})

	It("UT-KSM-017 零匹配只规划非 workload 资源并保留 workload 删除身份", func() {
		By("准备当前没有 Namespace 匹配的 Include 声明")
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "empty", UID: types.UID("uid-55-empty")},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: "monitoring",
				NamespaceFilter: monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"future-.*"}},
			},
		}

		By("验证只保留非 workload 期望资源和可删除 workload 身份")
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"monitoring"}})
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveNamespaces).To(BeEmpty())
		Expect(plan.Resources).To(HaveLen(4))
		Expect(plan.Workloads).To(ConsistOf(BeAssignableToTypeOf(&appsv1.Deployment{})))
	})

	DescribeTable("UT-KSM-016 对非法 namespaceFilter 返回 InvalidSpec 且零资源",
		func(filter monitoringv1alpha1.NamespaceFilter, message string) {
			By("执行包含非法 namespaceFilter 的纯规划")
			instance := &monitoringv1alpha1.KubeStateMetrics{
				ObjectMeta: metav1.ObjectMeta{Name: "invalid-filter", UID: types.UID("uid-55-invalid")},
				Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: "monitoring", NamespaceFilter: filter},
			}
			plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"monitoring"}})
			Expect(plan).To(BeNil())
			Expect(problem).To(SatisfyAll(HaveField("Reason", ksm.ReasonInvalidSpec), HaveField("Message", ContainSubstring(message))))
		},
		Entry("All 带字面名称", monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeAll, Names: []string{"team-a"}}, "All"),
		Entry("All 带表达式", monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeAll, NamePatterns: []string{"team-.*"}}, "All"),
		Entry("Include 为空", monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude}, "Include"),
		Entry("未知 mode", monitoringv1alpha1.NamespaceFilter{Mode: "Unknown", Names: []string{"team-a"}}, "Unknown"),
		Entry("非法字面 Namespace 名", monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, Names: []string{"team-a,team-b"}}, "team-a,team-b"),
		Entry("非法 RE2", monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"["}}, "["),
	)

	It("UT-KSM-015 将省略的 All 模式展开为全部当前 Namespace", func() {
		By("执行省略 namespaceFilter 的兼容声明")
		instance := &monitoringv1alpha1.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: "all", UID: types.UID("uid-55-all")}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: "monitoring"}}
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"team-b", "monitoring", "team-a"}})
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveNamespaces).To(Equal([]string{"monitoring", "team-a", "team-b"}))
		Expect(deploymentFrom(plan).Spec.Template.Spec.Containers[0].Args).To(ContainElement("--namespaces=monitoring,team-a,team-b"))
	})

	It("UT-KSM-014 将字面名称与完整 RE2 匹配结果作为排序去重并集下发", func() {
		By("执行包含字面名称、重叠 regex 和 substring 干扰项的纯规划")
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "filtered", UID: types.UID("uid-55")},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: "monitoring",
				NamespaceFilter: monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, Names: []string{"literal", "team-a"}, NamePatterns: []string{"team-[ab]"}},
			},
		}
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"monitoring", "team-b", "prefix-team-a", "team-a"}})
		Expect(problem).To(BeNil())
		Expect(plan.EffectiveNamespaces).To(Equal([]string{"literal", "team-a", "team-b"}))
		Expect(deploymentFrom(plan).Spec.Template.Spec.Containers[0].Args).To(ContainElement("--namespaces=literal,team-a,team-b"))
	})
})

func policyRulesAllow(rules []rbacv1.PolicyRule, apiGroup, resource, verb string) bool {
	for _, rule := range rules {
		if (slices.Contains(rule.APIGroups, apiGroup) || slices.Contains(rule.APIGroups, "*")) &&
			(slices.Contains(rule.Resources, resource) || slices.Contains(rule.Resources, "*")) &&
			(slices.Contains(rule.Verbs, verb) || slices.Contains(rule.Verbs, "*")) {
			return true
		}
	}
	return false
}

func deploymentFrom(plan *ksm.ResourcePlan) *appsv1.Deployment {
	for _, resource := range plan.Resources {
		if deployment, ok := resource.(*appsv1.Deployment); ok {
			return deployment
		}
	}
	Fail("deployment not found in resource plan")
	return nil
}

func serviceFrom(plan *ksm.ResourcePlan) *corev1.Service {
	for _, resource := range plan.Resources {
		if service, ok := resource.(*corev1.Service); ok {
			return service
		}
	}
	Fail("service not found in resource plan")
	return nil
}

func statefulSetFrom(plan *ksm.ResourcePlan) *appsv1.StatefulSet {
	return resourceOfType[*appsv1.StatefulSet](plan)
}

func resourceOfType[T client.Object](plan *ksm.ResourcePlan) T {
	for _, planned := range plan.Resources {
		if resource, ok := planned.(T); ok {
			return resource
		}
	}
	Fail(fmt.Sprintf("resource %T not found in resource plan", *new(T)))
	var zero T
	return zero
}

func expectInvalidSpecAcrossProfiles(instance *monitoringv1alpha1.KubeStateMetrics, wantMessage string) {
	for _, kubernetesVersion := range []string{"v1.19.16", "v1.28.15"} {
		By(fmt.Sprintf("在 %s profile 下执行纯规划", kubernetesVersion))
		plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
			KubernetesVersion:     kubernetesVersion,
			TargetNamespaceExists: true,
			NamespaceNames:        []string{"monitoring"},
		})

		By("验证 InvalidSpec 且零资源计划")
		Expect(plan).To(BeNil())
		Expect(problem).To(SatisfyAll(
			HaveField("Reason", ksm.ReasonInvalidSpec),
			HaveField("Message", ContainSubstring(wantMessage)),
		))
	}
}
