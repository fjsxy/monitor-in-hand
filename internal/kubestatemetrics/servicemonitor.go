package kubestatemetrics

import (
	"fmt"
	api "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"strings"
)

// PlanServiceMonitor 与 workload 共用实例身份，避免平台复制内部 Service 契约。
func PlanServiceMonitor(instance *api.KubeStateMetrics) (*unstructured.Unstructured, *Problem) {
	c := instance.Spec.ServiceMonitor
	if c == nil {
		return nil, nil
	}
	interval, timeout := c.Interval, c.ScrapeTimeout
	if interval == "" {
		interval = "30s"
	}
	if timeout == "" {
		timeout = "10s"
	}
	i, e1 := model.ParseDuration(interval)
	t, e2 := model.ParseDuration(timeout)
	invalid := func(message string) (*unstructured.Unstructured, *Problem) {
		return nil, &Problem{Reason: ReasonInvalidSpec, Message: message}
	}
	if e1 != nil || e2 != nil || i <= 0 || t <= 0 || t > i {
		return invalid("serviceMonitor 时长须为正数，且 scrapeTimeout 不超过 interval")
	}
	for k, v := range c.Labels {
		if strings.HasPrefix(k, "app.kubernetes.io/") || strings.HasPrefix(k, "monitor-in-hand/") || len(validation.IsQualifiedName(k)) > 0 || len(validation.IsValidLabelValue(v)) > 0 {
			return invalid(fmt.Sprintf("serviceMonitor.labels %q 非法或覆盖 Operator 保留标签", k))
		}
	}
	if !c.Enabled {
		return nil, nil
	}
	labels := map[string]interface{}{"app.kubernetes.io/name": "kube-state-metrics", "app.kubernetes.io/instance": stableInstanceID(instance.Name), "app.kubernetes.io/managed-by": "monitor-in-hand"}
	extras := runtime.DeepCopyJSONValue(labels).(map[string]interface{})
	for k, v := range c.Labels {
		extras[k] = v
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "monitoring.coreos.com/v1", "kind": "ServiceMonitor",
		"metadata": map[string]interface{}{"name": ManagedResourceName(instance.Name), "namespace": instance.Spec.TargetNamespace, "labels": extras},
		"spec": map[string]interface{}{
			"selector":          map[string]interface{}{"matchLabels": labels},
			"namespaceSelector": map[string]interface{}{"matchNames": []interface{}{instance.Spec.TargetNamespace}},
			"endpoints":         []interface{}{map[string]interface{}{"port": "http-metrics", "path": "/metrics", "interval": interval, "scrapeTimeout": timeout}},
		}}}
	obj.SetOwnerReferences([]metav1.OwnerReference{managingOwner(instance)})
	return obj, nil
}
