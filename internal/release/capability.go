package release

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// CheckCapability 在任何 exporter 写入前识别 schema，避免 API server 静默裁剪新字段。
func CheckCapability(data []byte) error {
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		return fmt.Errorf("读取 CRD: %w", err)
	}
	for path, want := range map[string]string{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata.name": "kubestatemetrics.monitoring.fjsxy.github.io", "spec.group": "monitoring.fjsxy.github.io", "spec.scope": "Cluster"} {
		if lookup(crd, path) != want {
			return fmt.Errorf("不支持配套 ServiceMonitor: %s 不匹配", path)
		}
	}
	versions, _ := lookup(crd, "spec.versions").([]any)
	for _, item := range versions {
		v, _ := item.(map[string]any)
		if v["name"] != "v1alpha1" || v["served"] != true {
			continue
		}
		schema, _ := lookup(v, "schema.openAPIV3Schema").(map[string]any)
		for path, want := range map[string]string{
			"spec.serviceMonitor": "object", "spec.serviceMonitor.enabled": "boolean", "spec.serviceMonitor.labels": "object", "spec.serviceMonitor.interval": "string", "spec.serviceMonitor.scrapeTimeout": "string",
			"status.serviceMonitor": "object", "status.serviceMonitor.observedGeneration": "integer", "status.serviceMonitor.state": "string", "status.serviceMonitor.reason": "string", "status.serviceMonitor.message": "string",
			"status.serviceMonitor.resource": "object", "status.serviceMonitor.resource.apiVersion": "string", "status.serviceMonitor.resource.kind": "string", "status.serviceMonitor.resource.namespace": "string", "status.serviceMonitor.resource.name": "string", "status.serviceMonitor.resource.uid": "string",
		} {
			field := schemaField(schema, path)
			if field["type"] != want || field["x-kubernetes-preserve-unknown-fields"] == true {
				return fmt.Errorf("不支持配套 ServiceMonitor: %s 类型缺失或不匹配", path)
			}
		}
		if schemaField(schema, "spec.serviceMonitor.enabled")["default"] != false {
			return fmt.Errorf("不支持配套 ServiceMonitor: enabled 缺省必须关闭")
		}
		labels := schemaField(schema, "spec.serviceMonitor.labels")
		if lookup(labels, "additionalProperties.type") != "string" {
			return fmt.Errorf("不支持配套 ServiceMonitor: labels 必须是字符串映射")
		}
		states, _ := schemaField(schema, "status.serviceMonitor.state")["enum"].([]any)
		seen := map[string]bool{}
		for _, state := range states {
			if value, ok := state.(string); ok {
				seen[value] = true
			}
		}
		for _, state := range []string{"Unknown", "Disabled", "Ready", "Degraded", "Deleting"} {
			if !seen[state] {
				return fmt.Errorf("不支持配套 ServiceMonitor: 缺少状态 %s", state)
			}
		}
		return nil
	}
	return fmt.Errorf("不支持配套 ServiceMonitor: 没有 served v1alpha1 schema")
}

// schemaField 逐级跨越 OpenAPI properties，不把字段存在误当作类型兼容。
func schemaField(schema map[string]any, path string) map[string]any {
	for _, key := range strings.Split(path, ".") {
		schema, _ = lookup(schema, "properties."+key).(map[string]any)
	}
	return schema
}

func lookup(value map[string]any, path string) any {
	var current any = value
	for _, key := range strings.Split(path, ".") {
		m, _ := current.(map[string]any)
		current = m[key]
	}
	return current
}
