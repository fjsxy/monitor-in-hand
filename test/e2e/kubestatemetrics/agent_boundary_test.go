package kubestatemetrics_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
)

type gh60BoundarySnapshot struct {
	Namespace, Resource, Name, Fingerprint string
}

var gh60PreInstallSnapshots []gh60BoundarySnapshot

var _ = Describe("PrometheusAgent 反向所有权边界", Serial, func() {
	It(fmt.Sprintf("[%s] 本 Operator 启动前后不改变外部 Agent 资源图", gh60BoundaryScenarioID()), func(ctx SpecContext) {
		scenarioID := gh60BoundaryScenarioID()
		By(scenarioID + " 确认当前工作树构建的本 Operator 已经可用")
		operator, err := getObject[appsv1.Deployment](ctx, operatorNamespace, "deployment", "monitor-in-hand-monitor-in-hand")
		Expect(err).NotTo(HaveOccurred())
		Expect(operator.Status.ObservedGeneration).To(Equal(operator.Generation))
		Expect(operator.Status.AvailableReplicas).To(Equal(int32(1)))

		By(scenarioID + " 将当前对象与 Operator 启动前的身份、spec/data 和 owner graph 基线逐一比较")
		Consistently(func(g Gomega) {
			for _, snapshot := range gh60PreInstallSnapshots {
				fingerprint, fingerprintErr := gh60BoundaryFingerprint(ctx, snapshot.Namespace, snapshot.Resource, snapshot.Name)
				g.Expect(fingerprintErr).NotTo(HaveOccurred())
				g.Expect(fingerprint).To(Equal(snapshot.Fingerprint), snapshot.Resource+"/"+snapshot.Name)
			}
		}, 10*time.Second, 2*time.Second).Should(Succeed())
	}, SpecTimeout(2*time.Minute))
})

func gh60BoundaryScenarioID() string {
	ids := map[string]string{
		"1.19": "E2E-119-BOUNDARY-GH60-001",
		"1.28": "E2E-128-BOUNDARY-GH60-001",
	}
	version := active.kubernetes
	if version == "" {
		version = os.Getenv("KUBERNETES_VERSION")
	}
	return ids[version]
}

func captureGH60PreInstallSnapshots(ctx context.Context) []gh60BoundarySnapshot {
	GinkgoHelper()
	resources := []gh60BoundarySnapshot{
		{Namespace: targetNamespace, Resource: "prometheusagent", Name: "fixture"},
		{Namespace: targetNamespace, Resource: "serviceaccount", Name: "prometheus-agent"},
		{Resource: "clusterrolebinding", Name: "monitor-in-hand-test-prometheus-agent"},
		{Resource: "clusterrole", Name: "monitor-in-hand-test-prometheus-agent"},
		{Namespace: targetNamespace, Resource: "servicemonitor", Name: "fixture"},
		{Namespace: targetNamespace, Resource: "podmonitor", Name: "fixture"},
		{Namespace: targetNamespace, Resource: "probe", Name: "fixture"},
		{Namespace: targetNamespace, Resource: "scrapeconfig", Name: "fixture"},
		{Namespace: targetNamespace, Resource: "statefulset", Name: "prom-agent-fixture"},
		{Namespace: targetNamespace, Resource: "pod", Name: "prom-agent-fixture-0"},
		{Namespace: targetNamespace, Resource: "secret", Name: "prom-agent-fixture"},
		{Namespace: targetNamespace, Resource: "secret", Name: "prom-agent-fixture-tls-assets-0"},
		{Namespace: targetNamespace, Resource: "secret", Name: "prom-agent-fixture-web-config"},
		{Namespace: targetNamespace, Resource: "persistentvolumeclaim", Name: "prom-agent-fixture-db-prom-agent-fixture-0"},
		{Namespace: targetNamespace, Resource: "service", Name: "prometheus-agent-operated"},
	}
	for index := range resources {
		fingerprint, err := gh60BoundaryFingerprint(ctx, resources[index].Namespace, resources[index].Resource, resources[index].Name)
		Expect(err).NotTo(HaveOccurred())
		resources[index].Fingerprint = fingerprint
	}
	return resources
}

func gh60BoundaryFingerprint(ctx context.Context, namespace, resource, name string) (string, error) {
	args := []string{"--context", kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", resource, name, "--output", "json")
	output, err := exec.CommandContext(ctx, kubectlBin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("读取 %s/%s: %w: %s", resource, name, err, output)
	}
	var object map[string]any
	if err := json.Unmarshal(output, &object); err != nil {
		return "", fmt.Errorf("解析 %s/%s: %w", resource, name, err)
	}
	metadata, _ := object["metadata"].(map[string]any)
	fingerprint := map[string]any{
		"uid":             metadata["uid"],
		"labels":          metadata["labels"],
		"annotations":     metadata["annotations"],
		"ownerReferences": metadata["ownerReferences"],
		"finalizers":      metadata["finalizers"],
	}
	for _, field := range []string{"spec", "data", "roleRef", "subjects", "rules", "secrets"} {
		if value, found := object[field]; found {
			fingerprint[field] = value
		}
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return "", fmt.Errorf("编码 %s/%s 快照: %w", resource, name, err)
	}
	return string(encoded), nil
}
