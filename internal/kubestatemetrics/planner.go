package kubestatemetrics

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
)

const (
	ReasonInvalidSpec                  = "InvalidSpec"
	ReasonDependencyMissing            = "DependencyMissing"
	ReasonResourceConflict             = "ResourceConflict"
	ReasonApplyFailed                  = "ApplyFailed"
	ReasonUnsupportedKubernetesVersion = "UnsupportedKubernetesVersion"
	ReasonNoNamespaceMatched           = "NoNamespaceMatched"
	ReasonSwitchingDeploymentMode      = "SwitchingDeploymentMode"
	ReasonObservationFailed            = "ObservationFailed"
	ReasonUnsupportedMode              = "UnsupportedMode"

	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelVersion   = "app.kubernetes.io/version"

	// LabelRole separates workload responsibility without narrowing Service endpoint selection.
	LabelRole = "monitor-in-hand/role"
	// LabelEndpointGroup groups distinct workload roles behind one metrics Service.
	LabelEndpointGroup = "monitor-in-hand/endpoint-group"

	// EndpointGroupDefault identifies the single Deployment endpoints.
	EndpointGroupDefault = "default"
	// EndpointGroupSharded identifies the automated StatefulSet shard endpoints.
	EndpointGroupSharded = "sharded"
	// EndpointGroupNode identifies per-node Pod metric endpoints.
	EndpointGroupNode = "node"
	// EndpointGroupControl identifies both DaemonSetSharded companion endpoints.
	EndpointGroupControl = "control"
	// WorkloadRoleNode owns scheduled Pod metrics for one node.
	WorkloadRoleNode = "node"
	// WorkloadRoleCluster owns all selected non-Pod collectors.
	WorkloadRoleCluster = "cluster"
	// WorkloadRoleUnscheduled owns metrics for Pods without a node assignment.
	WorkloadRoleUnscheduled = "unscheduled"
)

// ClusterFacts are read-only observations needed by pure planning.
type ClusterFacts struct {
	KubernetesVersion     string
	TargetNamespaceExists bool
	NamespaceNames        []string
}

// Problem is a stable, administrator-visible planning failure.
type Problem struct {
	Reason  string
	Message string
}

// ArgumentCapabilities 记录固定 kube-state-metrics profile 已核实的 CLI 能力。
type ArgumentCapabilities struct {
	LogToStderr    bool
	Verbosity      bool
	GZIPEncoding   bool
	APIServerCache bool
}

// Profile fixes every Kubernetes-version-dependent kube-state-metrics setting.
type Profile struct {
	Version                      string
	Image                        string
	ArgumentCapabilities         ArgumentCapabilities
	Rules                        []rbacv1.PolicyRule
	ServiceAccountAutomountToken *bool
	PodAutomountToken            *bool
	ContainerSecurityContext     *corev1.SecurityContext
	DaemonSetSharded             bool
}

// ResourcePlan contains the complete desired state for one reconcile.
type ResourcePlan struct {
	ServiceMonitor      *unstructured.Unstructured
	Profile             Profile
	InstanceID          string
	Name                string
	Mode                monitoringv1alpha1.DeploymentMode
	DesiredPods         int32
	EffectiveCollectors []string
	EffectiveNamespaces []string
	Resources           []client.Object
	Workloads           []client.Object
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
	monitor, monitorProblem := PlanServiceMonitor(instance)
	if monitorProblem != nil {
		return nil, monitorProblem
	}

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
	switch mode {
	case monitoringv1alpha1.DeploymentModeDeployment:
		if instance.Spec.Deployment.StatefulSetSharded != nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.deployment.statefulSetSharded 仅能在 mode 为 StatefulSetSharded 时声明"}
		}
		if instance.Spec.Deployment.DaemonSetSharded != nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.deployment.daemonSetSharded 仅能在 mode 为 DaemonSetSharded 时声明"}
		}
	case monitoringv1alpha1.DeploymentModeStatefulSetSharded:
		if instance.Spec.Deployment.DaemonSetSharded != nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.deployment.daemonSetSharded 仅能在 mode 为 DaemonSetSharded 时声明"}
		}
		if instance.Spec.Deployment.StatefulSetSharded == nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.deployment.mode 为 StatefulSetSharded 时必须声明 statefulSetSharded 分支"}
		}
		if instance.Spec.Deployment.StatefulSetSharded.Shards < 2 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.deployment.statefulSetSharded.shards 必须至少为 2"}
		}
	case monitoringv1alpha1.DeploymentModeDaemonSetSharded:
		if instance.Spec.Deployment.StatefulSetSharded != nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.deployment.statefulSetSharded 仅能在 mode 为 StatefulSetSharded 时声明"}
		}
		if instance.Spec.Deployment.DaemonSetSharded == nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.deployment.mode 为 DaemonSetSharded 时必须声明 daemonSetSharded 分支"}
		}
	default:
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.deployment.mode %q 不受支持", mode)}
	}
	if instance.Spec.PodResources != nil && len(instance.Spec.PodResources.Claims) != 0 {
		return nil, &Problem{
			Reason:  ReasonInvalidSpec,
			Message: "spec.podResources.claims 不受支持；受管 Pod 不开放 resourceClaims 声明",
		}
	}
	if instance.Spec.Deployment.DaemonSetSharded != nil {
		roleResources := []struct {
			field     string
			resources *corev1.ResourceRequirements
		}{
			{field: "nodePodResources", resources: instance.Spec.Deployment.DaemonSetSharded.NodePodResources},
			{field: "clusterPodResources", resources: instance.Spec.Deployment.DaemonSetSharded.ClusterPodResources},
			{field: "unscheduledPodResources", resources: instance.Spec.Deployment.DaemonSetSharded.UnscheduledPodResources},
		}
		for _, role := range roleResources {
			if role.resources != nil && len(role.resources.Claims) != 0 {
				return nil, &Problem{
					Reason:  ReasonInvalidSpec,
					Message: fmt.Sprintf("spec.deployment.daemonSetSharded.%s.claims 不受支持；受管 Pod 不开放 resourceClaims 声明", role.field),
				}
			}
		}
	}

	effectiveCollectors, problem := resolveCollectors(instance.Spec.Collectors)
	if problem != nil {
		return nil, problem
	}
	if mode == monitoringv1alpha1.DeploymentModeDaemonSetSharded {
		if !containsString(effectiveCollectors, "pods") || len(effectiveCollectors) < 2 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "DaemonSetSharded collectors 必须包含 pods 且至少包含一个非 Pod collector"}
		}
	}
	metricFilterArgs, problem := resolveMetricFilter(instance.Spec.MetricFilter)
	if problem != nil {
		return nil, problem
	}
	effectiveNamespaces, problem := resolveNamespaces(instance.Spec.NamespaceFilter, facts.NamespaceNames)
	if problem != nil {
		return nil, problem
	}

	// spec 有效后再选择版本 profile 并验证外部 Namespace 依赖。
	profile, problem := profileFor(facts.KubernetesVersion)
	if problem != nil {
		return nil, problem
	}
	if mode == monitoringv1alpha1.DeploymentModeDaemonSetSharded && !profile.DaemonSetSharded {
		return nil, &Problem{Reason: ReasonUnsupportedMode, Message: fmt.Sprintf("Kubernetes %s profile 不支持 experimental DaemonSetSharded", facts.KubernetesVersion)}
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
	sharedFilterArgs := append([]string(nil), metricFilterArgs...)
	sharedFilterArgs = append(sharedFilterArgs, "--namespaces="+strings.Join(effectiveNamespaces, ","))
	// Default 依赖上游默认集合；只有 Include 才把声明的资源边界显式下发给非 DaemonSet 拓扑。
	containerArgs := append([]string(nil), sharedFilterArgs...)
	if instance.Spec.Collectors.Mode == monitoringv1alpha1.CollectorModeInclude {
		containerArgs = append([]string{"--resources=" + strings.Join(effectiveCollectors, ",")}, containerArgs...)
	}
	var nonWorkloads []client.Object
	var workloads []client.Object
	desiredPods := int32(1)
	if mode == monitoringv1alpha1.DeploymentModeDaemonSetSharded {
		desiredPods = 2
		nonWorkloads, workloads = daemonSetShardedResources(instance, profile, instanceID, name, effectiveCollectors, sharedFilterArgs)
	} else if mode == monitoringv1alpha1.DeploymentModeStatefulSetSharded {
		desiredPods = instance.Spec.Deployment.StatefulSetSharded.Shards
		var workload client.Object
		nonWorkloads, workload = statefulSetShardedResources(instance, profile, instanceID, name, containerArgs, desiredPods)
		workloads = []client.Object{workload}
	} else {
		var workload client.Object
		nonWorkloads, workload = deploymentResources(instance, profile, instanceID, name, containerArgs)
		workloads = []client.Object{workload}
	}
	// 零匹配时从期望资源中分离全部 workload，但保留其身份供 controller 安全删除。
	resources := append([]client.Object(nil), nonWorkloads...)
	if len(effectiveNamespaces) > 0 {
		resources = append(resources, workloads...)
	} else {
		desiredPods = 0
	}
	return &ResourcePlan{
		ServiceMonitor:      monitor,
		Profile:             profile,
		InstanceID:          instanceID,
		Name:                name,
		Mode:                mode,
		DesiredPods:         desiredPods,
		EffectiveCollectors: effectiveCollectors,
		EffectiveNamespaces: effectiveNamespaces,
		Resources:           resources,
		Workloads:           workloads,
	}, nil
}

func resolveNamespaces(filter monitoringv1alpha1.NamespaceFilter, namespaceNames []string) ([]string, *Problem) {
	mode := filter.Mode
	if mode == "" {
		mode = monitoringv1alpha1.NamespaceFilterModeAll
	}
	switch mode {
	case monitoringv1alpha1.NamespaceFilterModeAll:
		if len(filter.Names) > 0 || len(filter.NamePatterns) > 0 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.namespaceFilter.mode 为 All 时 names 与 namePatterns 必须为空"}
		}
		selected := make(map[string]struct{}, len(namespaceNames))
		for _, name := range namespaceNames {
			selected[name] = struct{}{}
		}
		return sortedNames(selected), nil
	case monitoringv1alpha1.NamespaceFilterModeInclude:
		if len(filter.Names) == 0 && len(filter.NamePatterns) == 0 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: "spec.namespaceFilter.mode 为 Include 时 names 与 namePatterns 至少一项非空"}
		}
	default:
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.namespaceFilter.mode %q 不受支持", mode)}
	}

	selected := make(map[string]struct{}, len(filter.Names))
	for _, name := range filter.Names {
		if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.namespaceFilter.names 包含非法 Namespace 名 %q: %s", name, strings.Join(problems, "; "))}
		}
		selected[name] = struct{}{}
	}
	// 先编译每个表达式，再以显式锚点执行完整 Namespace 名匹配并合并字面项。
	for _, expression := range filter.NamePatterns {
		pattern, err := regexp.Compile("^(?:" + expression + ")$")
		if err != nil {
			return nil, &Problem{Reason: ReasonInvalidSpec, Message: fmt.Sprintf("spec.namespaceFilter.namePatterns 包含非法 RE2 表达式 %q: %v", expression, err)}
		}
		for _, namespace := range namespaceNames {
			if pattern.MatchString(namespace) {
				selected[namespace] = struct{}{}
			}
		}
	}

	return sortedNames(selected), nil
}

func sortedNames(selected map[string]struct{}) []string {
	result := make([]string, 0, len(selected))
	for name := range selected {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
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
			ArgumentCapabilities:     supportedArgumentCapabilities(),
			Rules:                    rulesForKubernetes119(),
			ContainerSecurityContext: &corev1.SecurityContext{RunAsUser: int64Pointer(65534)},
		}, nil
	case 28:
		return Profile{
			Version:                      "2.11.0",
			Image:                        "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.11.0",
			ArgumentCapabilities:         supportedArgumentCapabilities(),
			Rules:                        rulesForKubernetes128(),
			ServiceAccountAutomountToken: boolPointer(false),
			PodAutomountToken:            boolPointer(true),
			DaemonSetSharded:             true,
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

func deploymentResources(instance *monitoringv1alpha1.KubeStateMetrics, profile Profile, instanceID, name string, containerArgs []string) ([]client.Object, *appsv1.Deployment) {
	containerArgs = appendCapabilityArgs(containerArgs, profile.ArgumentCapabilities)
	nonWorkloads, deployment, _ := deploymentResourcesForEndpointGroup(instance, profile, instanceID, name, containerArgs, EndpointGroupDefault)
	return nonWorkloads, deployment
}

func deploymentResourcesForEndpointGroup(instance *monitoringv1alpha1.KubeStateMetrics, profile Profile, instanceID, name string, containerArgs []string, endpointGroup string) ([]client.Object, *appsv1.Deployment, *corev1.Service) {
	labels := managedLabels(instanceID, profile.Version, endpointGroup)
	podResources := corev1.ResourceRequirements{}
	if instance.Spec.PodResources != nil {
		podResources = *instance.Spec.PodResources.DeepCopy()
	}
	selector := map[string]string{
		labelName:          "kube-state-metrics",
		labelInstance:      instanceID,
		LabelEndpointGroup: endpointGroup,
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
			Selector: &metav1.LabelSelector{MatchLabels: copyLabels(selector)},
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
			Selector:  copyLabels(selector),
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
	return []client.Object{serviceAccount, clusterRole, clusterRoleBinding, service}, deployment, service
}

func statefulSetShardedResources(instance *monitoringv1alpha1.KubeStateMetrics, profile Profile, instanceID, name string, containerArgs []string, shards int32) ([]client.Object, client.Object) {
	// 先复用与 Deployment 一致的 Pod、ServiceAccount、Cluster RBAC 和 headless Service 契约，仅切换 endpoint-group。
	shardingArgs := append(append([]string(nil), containerArgs...), "--pod=$(POD_NAME)", "--pod-namespace=$(POD_NAMESPACE)")
	shardingArgs = appendShardedCapabilityArgs(shardingArgs, profile.ArgumentCapabilities)
	nonWorkloads, deployment, _ := deploymentResourcesForEndpointGroup(instance, profile, instanceID, name, shardingArgs, EndpointGroupSharded)
	// 通过 Downward API 把每个 Pod 的唯一身份交给上游 automated sharding。
	container := &deployment.Spec.Template.Spec.Containers[0]
	container.Env = []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
	}
	// 将已统一解析的 Pod template 放入指定副本数的 StatefulSet，以稳定 ordinal 支持自动分片。
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: *deployment.ObjectMeta.DeepCopy(),
		Spec: appsv1.StatefulSetSpec{
			Replicas:    int32Pointer(shards),
			Selector:    deployment.Spec.Selector.DeepCopy(),
			ServiceName: name,
			Template:    *deployment.Spec.Template.DeepCopy(),
		},
	}
	labels := managedLabels(instanceID, profile.Version, EndpointGroupSharded)
	owner := managingOwner(instance)
	// 最后添加仅分片模式需要的 namespaced RBAC，限定只读自身 Pod 和对应 StatefulSet。
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Spec.TargetNamespace, Labels: copyLabels(labels), OwnerReferences: []metav1.OwnerReference{owner}},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
			{APIGroups: []string{"apps"}, Resources: []string{"statefulsets"}, ResourceNames: []string{name}, Verbs: []string{"get"}},
		},
	}
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Spec.TargetNamespace, Labels: copyLabels(labels), OwnerReferences: []metav1.OwnerReference{owner}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: instance.Spec.TargetNamespace}},
	}
	return append(nonWorkloads, role, roleBinding), statefulSet
}

func daemonSetShardedResources(instance *monitoringv1alpha1.KubeStateMetrics, profile Profile, instanceID, name string, collectors, sharedFilterArgs []string) ([]client.Object, []client.Object) {
	daemonSpec := instance.Spec.Deployment.DaemonSetSharded
	// node 角色只负责已调度 Pod；Downward API 将所在节点交给上游 field selector 分片。
	nodeArgs := append([]string{"--resources=pods"}, sharedFilterArgs...)
	nodeArgs = append(nodeArgs, "--node=$(NODE_NAME)")
	nodeArgs = appendShardedCapabilityArgs(nodeArgs, profile.ArgumentCapabilities)
	nonWorkloads, nodeTemplate, nodeService := deploymentResourcesForEndpointGroup(instance, profile, instanceID, name, nodeArgs, EndpointGroupNode)
	setDeploymentRole(nodeTemplate, WorkloadRoleNode)
	setContainerResources(&nodeTemplate.Spec.Template.Spec.Containers[0], daemonSpec.NodePodResources, instance.Spec.PodResources)
	nodeTemplate.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
	}}
	node := &appsv1.DaemonSet{
		ObjectMeta: *nodeTemplate.ObjectMeta.DeepCopy(),
		Spec: appsv1.DaemonSetSpec{
			Selector: nodeTemplate.Spec.Selector.DeepCopy(),
			Template: *nodeTemplate.Spec.Template.DeepCopy(),
		},
	}

	// cluster 角色继承同一 collector 解析结果，但移除 pods，避免与两个 Pod 角色重复暴露指标。
	clusterCollectors := make([]string, 0, len(collectors)-1)
	for _, collector := range collectors {
		if collector != "pods" {
			clusterCollectors = append(clusterCollectors, collector)
		}
	}
	clusterArgs := append([]string{"--resources=" + strings.Join(clusterCollectors, ",")}, sharedFilterArgs...)
	clusterArgs = appendShardedCapabilityArgs(clusterArgs, profile.ArgumentCapabilities)
	_, cluster, _ := deploymentResourcesForEndpointGroup(instance, profile, instanceID, name+"-cluster", clusterArgs, EndpointGroupControl)
	cluster.Spec.Template.Spec.ServiceAccountName = name
	setDeploymentRole(cluster, WorkloadRoleCluster)
	setContainerResources(&cluster.Spec.Template.Spec.Containers[0], daemonSpec.ClusterPodResources, instance.Spec.PodResources)

	// unscheduled 角色使用 v2.11 官方的空 node 参数，只读取尚未绑定节点的 Pod。
	unscheduledArgs := append([]string{"--resources=pods"}, sharedFilterArgs...)
	unscheduledArgs = append(unscheduledArgs, `--node=""`)
	unscheduledArgs = appendShardedCapabilityArgs(unscheduledArgs, profile.ArgumentCapabilities)
	_, unscheduled, _ := deploymentResourcesForEndpointGroup(instance, profile, instanceID, name+"-unscheduled", unscheduledArgs, EndpointGroupControl)
	unscheduled.Spec.Template.Spec.ServiceAccountName = name
	setDeploymentRole(unscheduled, WorkloadRoleUnscheduled)
	setContainerResources(&unscheduled.Spec.Template.Spec.Containers[0], daemonSpec.UnscheduledPodResources, instance.Spec.PodResources)

	// 两个 Service 只按 endpoint-group 选择：node 单独暴露 DaemonSet，control 合并两个不同指标族的 companion。
	nodeService.Labels[LabelRole] = WorkloadRoleNode
	controlLabels := managedLabels(instanceID, profile.Version, EndpointGroupControl)
	controlLabels[LabelRole] = EndpointGroupControl
	controlService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-control", Namespace: instance.Spec.TargetNamespace, Labels: controlLabels,
			OwnerReferences: []metav1.OwnerReference{managingOwner(instance)},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector: map[string]string{
				labelName: "kube-state-metrics", labelInstance: instanceID, LabelEndpointGroup: EndpointGroupControl,
			},
			Ports: append([]corev1.ServicePort(nil), nodeService.Spec.Ports...),
		},
	}
	return append(nonWorkloads, controlService), []client.Object{node, cluster, unscheduled}
}

func setDeploymentRole(deployment *appsv1.Deployment, role string) {
	deployment.Labels[LabelRole] = role
	deployment.Spec.Selector.MatchLabels[LabelRole] = role
	deployment.Spec.Template.Labels[LabelRole] = role
}

func setContainerResources(container *corev1.Container, override, inherited *corev1.ResourceRequirements) {
	selected := inherited
	if override != nil {
		selected = override
	}
	container.Resources = corev1.ResourceRequirements{}
	if selected != nil {
		container.Resources = *selected.DeepCopy()
	}
}

func supportedArgumentCapabilities() ArgumentCapabilities {
	return ArgumentCapabilities{
		LogToStderr: true, Verbosity: true, GZIPEncoding: true, APIServerCache: true,
	}
}

func appendCapabilityArgs(args []string, capabilities ArgumentCapabilities) []string {
	// 先保留 collector、filter 与角色参数，再按 profile 能力追加统一参数。
	result := append([]string(nil), args...)
	if capabilities.LogToStderr {
		result = append(result, "--logtostderr=true")
	}
	if capabilities.Verbosity {
		result = append(result, "--v=0")
	}
	if capabilities.GZIPEncoding {
		result = append(result, "--enable-gzip-encoding=true")
	}
	return result
}

func appendShardedCapabilityArgs(args []string, capabilities ArgumentCapabilities) []string {
	result := appendCapabilityArgs(args, capabilities)
	if capabilities.APIServerCache {
		result = append(result, "--use-apiserver-cache=true")
	}
	return result
}

func managedLabels(instanceID, version, endpointGroup string) map[string]string {
	return map[string]string{
		labelName:          "kube-state-metrics",
		labelInstance:      instanceID,
		labelManagedBy:     "monitor-in-hand",
		labelVersion:       version,
		LabelRole:          "collector",
		LabelEndpointGroup: endpointGroup,
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
var _ runtime.Object = &appsv1.StatefulSet{}
var _ runtime.Object = &appsv1.DaemonSet{}

func boolPointer(value bool) *bool    { return &value }
func int32Pointer(value int32) *int32 { return &value }
func int64Pointer(value int64) *int64 { return &value }
