package kubestatemetrics

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
)

const (
	ReasonInvalidSpec                  = "InvalidSpec"
	ReasonDependencyMissing            = "DependencyMissing"
	ReasonResourceConflict             = "ResourceConflict"
	ReasonApplyFailed                  = "ApplyFailed"
	ReasonUnsupportedKubernetesVersion = "UnsupportedKubernetesVersion"

	labelName          = "app.kubernetes.io/name"
	labelInstance      = "app.kubernetes.io/instance"
	labelManagedBy     = "app.kubernetes.io/managed-by"
	labelVersion       = "app.kubernetes.io/version"
	labelRole          = "monitor-in-hand/role"
	labelEndpointGroup = "monitor-in-hand/endpoint-group"
)

// ClusterFacts are read-only observations needed by pure planning.
type ClusterFacts struct {
	KubernetesVersion     string
	TargetNamespaceExists bool
}

// Problem is a stable, administrator-visible planning failure.
type Problem struct {
	Reason  string
	Message string
}

// Profile fixes every Kubernetes-version-dependent kube-state-metrics setting.
type Profile struct {
	Version                      string
	Image                        string
	Rules                        []rbacv1.PolicyRule
	ServiceAccountAutomountToken *bool
	PodAutomountToken            *bool
	ContainerSecurityContext     *corev1.SecurityContext
}

// ResourcePlan contains the complete desired state for one reconcile.
type ResourcePlan struct {
	Profile             Profile
	InstanceID          string
	Name                string
	EffectiveCollectors []string
	Resources           []client.Object
}

var collectorCatalog = []string{
	"certificatesigningrequests", "configmaps", "cronjobs", "daemonsets", "deployments", "endpoints",
	"horizontalpodautoscalers", "ingresses", "jobs", "leases", "limitranges", "mutatingwebhookconfigurations",
	"namespaces", "networkpolicies", "nodes", "persistentvolumeclaims", "persistentvolumes", "poddisruptionbudgets",
	"pods", "replicasets", "replicationcontrollers", "resourcequotas", "secrets", "services", "statefulsets",
	"storageclasses", "validatingwebhookconfigurations", "volumeattachments",
}

// BuildPlan validates all known inputs before returning any writable resources.
func BuildPlan(instance *monitoringv1alpha1.KubeStateMetrics, facts ClusterFacts) (*ResourcePlan, *Problem) {
	// 先完成全部纯 spec 校验，使配置错误稳定返回 InvalidSpec，而不被集群依赖状态掩盖。
	if instance.Spec.TargetNamespace == "" {
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.targetNamespace 必填"}
	}
	if instance.Status.AppliedTargetNamespace != "" && instance.Status.AppliedTargetNamespace != instance.Spec.TargetNamespace {
		return nil, &Problem{
			Reason:  ReasonInvalidSpec,
			Message: fmt.Sprintf("spec.targetNamespace 首次应用后不可修改：已应用 %q，当前 %q", instance.Status.AppliedTargetNamespace, instance.Spec.TargetNamespace),
		}
	}
	mode := instance.Spec.Deployment.Mode
	if mode == "" {
		mode = monitoringv1alpha1.DeploymentModeDeployment
	}
	if mode != monitoringv1alpha1.DeploymentModeDeployment {
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.deployment.mode %q 不受支持", mode)}
	}
	if instance.Spec.PodResources != nil && len(instance.Spec.PodResources.Claims) != 0 {
		return nil, &Problem{
			Reason:  ReasonInvalidSpec,
			Message: "spec.podResources.claims 不受支持；受管 Pod 不开放 resourceClaims 声明",
		}
	}

	effectiveCollectors, problem := resolveCollectors(instance.Spec.Collectors)
	if problem != nil {
		return nil, problem
	}
	metricFilterArgs, problem := resolveMetricFilter(instance.Spec.MetricFilter)
	if problem != nil {
		return nil, problem
	}

	// spec 有效后再选择版本 profile 并验证外部 Namespace 依赖。
	profile, problem := profileFor(facts.KubernetesVersion)
	if problem != nil {
		return nil, problem
	}
	if !facts.TargetNamespaceExists {
		return nil, &Problem{
			Reason:  ReasonDependencyMissing,
			Message: fmt.Sprintf("目标 Namespace %q 不存在；Operator 不会创建 Namespace", instance.Spec.TargetNamespace),
		}
	}

	// 最后把同一份解析结果同时用于 workload 参数和 status 所需的资源计划。
	instanceID := stableInstanceID(instance.Name)
	name := ManagedResourceName(instance.Name)
	containerArgs := []string{"--resources=" + strings.Join(effectiveCollectors, ",")}
	containerArgs = append(containerArgs, metricFilterArgs...)
	resources := deploymentResources(instance, profile, instanceID, name, containerArgs)
	return &ResourcePlan{
		Profile:             profile,
		InstanceID:          instanceID,
		Name:                name,
		EffectiveCollectors: effectiveCollectors,
		Resources:           resources,
	}, nil
}

func resolveMetricFilter(config monitoringv1alpha1.KubeStateMetricsMetricFilter) ([]string, *Problem) {
	switch config.Mode {
	case "", monitoringv1alpha1.MetricFilterModeAll:
		if len(config.Patterns) != 0 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.metricFilter.mode 为 All 时 patterns 必须为空"}
		}
		return nil, nil
	case monitoringv1alpha1.MetricFilterModeAllow:
		patterns, problem := metricPatternsForArgument(config.Patterns, monitoringv1alpha1.MetricFilterModeAllow)
		if problem != nil {
			return nil, problem
		}
		return []string{"--metric-allowlist=" + strings.Join(patterns, ",")}, nil
	case monitoringv1alpha1.MetricFilterModeDeny:
		patterns, problem := metricPatternsForArgument(config.Patterns, monitoringv1alpha1.MetricFilterModeDeny)
		if problem != nil {
			return nil, problem
		}
		return []string{"--metric-denylist=" + strings.Join(patterns, ",")}, nil
	default:
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.metricFilter.mode %q 不受支持", config.Mode)}
	}
}

func metricPatternsForArgument(patterns []string, mode monitoringv1alpha1.MetricFilterMode) ([]string, *Problem) {
	if len(patterns) == 0 {
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.metricFilter.mode 为 %s 时 patterns 必须非空", mode)}
	}
	result := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern == "" {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.metricFilter.patterns 不得包含空项"}
		}
		if strings.Contains(pattern, ",") {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.metricFilter.patterns 中 %q 包含逗号", pattern)}
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.metricFilter.patterns 中 %q 是非法 Go 正则表达式: %v", pattern, err)}
		}
		// 上游会 trim 每个 flag 项；用捕获组保护首尾空白，同时保持 Go MatchString 语义不变。
		if strings.TrimSpace(pattern) != pattern {
			pattern = "(" + pattern + ")"
		}
		result = append(result, pattern)
	}
	return result, nil
}

func resolveCollectors(config monitoringv1alpha1.KubeStateMetricsCollectors) ([]string, *Problem) {
	switch config.Mode {
	case "", monitoringv1alpha1.CollectorModeDefault:
		if len(config.Names) != 0 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.collectors.mode 为 Default 时 names 必须为空"}
		}
		return append([]string(nil), collectorCatalog...), nil
	case monitoringv1alpha1.CollectorModeInclude:
		if len(config.Names) == 0 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.collectors.mode 为 Include 时 names 必须非空"}
		}
		seen := make(map[string]struct{}, len(config.Names))
		for _, name := range config.Names {
			if !isKnownCollector(name) {
				return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.collectors.names 包含共同 catalog 外的 collector %q", name)}
			}
			if _, exists := seen[name]; exists {
				return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.collectors.names 包含重复项 %q", name)}
			}
			seen[name] = struct{}{}
		}
		return append([]string(nil), config.Names...), nil
	default:
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.collectors.mode %q 不受支持", config.Mode)}
	}
}

func isKnownCollector(name string) bool {
	for _, candidate := range collectorCatalog {
		if name == candidate {
			return true
		}
	}
	return false
}

// ManagedResourceName returns the stable shared name of an instance's directly managed resources.
func ManagedResourceName(instanceName string) string {
	return "ksm-" + stableInstanceID(instanceName)
}

func profileFor(gitVersion string) (Profile, *Problem) {
	minor, ok := kubernetesMinor(gitVersion)
	if !ok {
		return Profile{}, &Problem{Reason: ReasonUnsupportedKubernetesVersion, Message: fmt.Sprintf("无法识别 Kubernetes 版本 %q", gitVersion)}
	}
	switch minor {
	case 19:
		return Profile{
			Version:                  "2.3.0",
			Image:                    "k8s.gcr.io/kube-state-metrics/kube-state-metrics:v2.3.0",
			Rules:                    rulesForKubernetes119(),
			ContainerSecurityContext: &corev1.SecurityContext{RunAsUser: int64Pointer(65534)},
		}, nil
	case 28:
		return Profile{
			Version:                      "2.11.0",
			Image:                        "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.11.0",
			Rules:                        rulesForKubernetes128(),
			ServiceAccountAutomountToken: boolPointer(false),
			PodAutomountToken:            boolPointer(true),
			ContainerSecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPointer(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				ReadOnlyRootFilesystem:   boolPointer(true),
				RunAsNonRoot:             boolPointer(true),
				RunAsUser:                int64Pointer(65534),
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		}, nil
	default:
		return Profile{}, &Problem{
			Reason:  ReasonUnsupportedKubernetesVersion,
			Message: fmt.Sprintf("Kubernetes 1.%d 不受支持；仅支持 1.19.x 与 1.28.x", minor),
		}
	}
}

func kubernetesMinor(gitVersion string) (int, bool) {
	version := strings.TrimPrefix(gitVersion, "v")
	parts := strings.Split(version, ".")
	if len(parts) < 2 || parts[0] != "1" {
		return 0, false
	}
	minorText := strings.TrimRight(parts[1], "+")
	minor, err := strconv.Atoi(minorText)
	return minor, err == nil
}

func stableInstanceID(name string) string {
	normalized := strings.ReplaceAll(name, ".", "-")
	if normalized == name && len(normalized) <= 43 {
		return normalized
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	prefix := strings.Trim(normalized, "-")
	if len(prefix) > 34 {
		prefix = strings.TrimRight(prefix[:34], "-")
	}
	return prefix + "-" + digest
}

func deploymentResources(instance *monitoringv1alpha1.KubeStateMetrics, profile Profile, instanceID, name string, containerArgs []string) []client.Object {
	labels := managedLabels(instanceID, profile.Version)
	podResources := corev1.ResourceRequirements{}
	if instance.Spec.PodResources != nil {
		podResources = *instance.Spec.PodResources.DeepCopy()
	}
	selector := map[string]string{
		labelName:          "kube-state-metrics",
		labelInstance:      instanceID,
		labelEndpointGroup: "default",
	}
	owner := managingOwner(instance)
	namespace := instance.Spec.TargetNamespace
	ports := []corev1.ContainerPort{
		{Name: "http-metrics", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		{Name: "telemetry", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: copyLabels(labels), OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Pointer(1),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(labels)},
				Spec: corev1.PodSpec{
					ServiceAccountName:           name,
					AutomountServiceAccountToken: profile.PodAutomountToken,
					NodeSelector:                 map[string]string{"kubernetes.io/os": "linux"},
					Containers: []corev1.Container{{
						Name:                     "kube-state-metrics",
						Image:                    profile.Image,
						ImagePullPolicy:          corev1.PullIfNotPresent,
						Args:                     append([]string(nil), containerArgs...),
						Ports:                    ports,
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http-metrics"), Scheme: corev1.URISchemeHTTP}},
							InitialDelaySeconds: 5,
							TimeoutSeconds:      5,
							PeriodSeconds:       10,
							SuccessThreshold:    1,
							FailureThreshold:    3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromString("telemetry"), Scheme: corev1.URISchemeHTTP}},
							InitialDelaySeconds: 5,
							TimeoutSeconds:      5,
							PeriodSeconds:       10,
							SuccessThreshold:    1,
							FailureThreshold:    3,
						},
						SecurityContext: profile.ContainerSecurityContext.DeepCopy(),
						Resources:       podResources,
					}},
				},
			},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: copyLabels(labels), OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  selector,
			Ports: []corev1.ServicePort{
				{Name: "http-metrics", Port: 8080, TargetPort: intstr.FromString("http-metrics"), Protocol: corev1.ProtocolTCP},
				{Name: "telemetry", Port: 8081, TargetPort: intstr.FromString("telemetry"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: copyLabels(labels), OwnerReferences: []metav1.OwnerReference{owner}},
		AutomountServiceAccountToken: profile.ServiceAccountAutomountToken,
	}
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: copyLabels(labels), OwnerReferences: []metav1.OwnerReference{owner}},
		Rules:      append([]rbacv1.PolicyRule(nil), profile.Rules...),
	}
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: copyLabels(labels), OwnerReferences: []metav1.OwnerReference{owner}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: namespace}},
	}
	return []client.Object{serviceAccount, clusterRole, clusterRoleBinding, service, deployment}
}

func managedLabels(instanceID, version string) map[string]string {
	return map[string]string{
		labelName:          "kube-state-metrics",
		labelInstance:      instanceID,
		labelManagedBy:     "monitor-in-hand",
		labelVersion:       version,
		labelRole:          "collector",
		labelEndpointGroup: "default",
	}
}

func copyLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}

func managingOwner(instance *monitoringv1alpha1.KubeStateMetrics) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: monitoringv1alpha1.GroupVersion.String(),
		Kind:       "KubeStateMetrics",
		Name:       instance.Name,
		UID:        instance.UID,
		Controller: boolPointer(true),
	}
}

func rulesForKubernetes119() []rbacv1.PolicyRule {
	return commonRules(false)
}

func rulesForKubernetes128() []rbacv1.PolicyRule {
	rules := commonRules(true)
	return append(rules,
		rbacv1.PolicyRule{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list", "watch"}},
		rbacv1.PolicyRule{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterrolebindings", "clusterroles", "rolebindings", "roles"}, Verbs: []string{"list", "watch"}},
	)
}

func commonRules(includeServiceAccountsAndIngressClasses bool) []rbacv1.PolicyRule {
	coreResources := []string{"configmaps", "secrets", "nodes", "pods", "services", "resourcequotas", "replicationcontrollers", "limitranges", "persistentvolumeclaims", "persistentvolumes", "namespaces", "endpoints"}
	networkingResources := []string{"networkpolicies", "ingresses"}
	if includeServiceAccountsAndIngressClasses {
		coreResources = append(coreResources, "serviceaccounts")
		networkingResources = append(networkingResources, "ingressclasses")
	}
	listWatch := []string{"list", "watch"}
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: coreResources, Verbs: listWatch},
		{APIGroups: []string{"apps"}, Resources: []string{"statefulsets", "daemonsets", "deployments", "replicasets"}, Verbs: listWatch},
		{APIGroups: []string{"batch"}, Resources: []string{"cronjobs", "jobs"}, Verbs: listWatch},
		{APIGroups: []string{"autoscaling"}, Resources: []string{"horizontalpodautoscalers"}, Verbs: listWatch},
		{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}, Verbs: listWatch},
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests"}, Verbs: listWatch},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses", "volumeattachments"}, Verbs: listWatch},
		{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"}, Verbs: listWatch},
		{APIGroups: []string{"networking.k8s.io"}, Resources: networkingResources, Verbs: listWatch},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: listWatch},
	}
}

// Ensure every desired resource can be handled through the controller-runtime client.
var _ runtime.Object = &appsv1.Deployment{}

func boolPointer(value bool) *bool    { return &value }
func int32Pointer(value int32) *int32 { return &value }
func int64Pointer(value int64) *int64 { return &value }
