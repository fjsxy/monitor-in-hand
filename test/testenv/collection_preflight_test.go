package testenv_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("采集配置部署预检", func() {
	It("[UT-EXAMPLE-GH59-001] 示例固定四类非空 selector、六种 discovery 与无 nodes runtime RBAC", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		By("读取管理员实际应用的公开示例")
		agentObjects := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "agent.yaml"))
		Expect(agentObjects).To(HaveLen(1))
		agent := agentObjects[0]
		for _, field := range []string{"serviceMonitorSelector", "podMonitorSelector", "probeSelector", "scrapeConfigSelector"} {
			selector, found, err := unstructured.NestedMap(agent, "spec", field)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), field)
			Expect(selector).To(HaveKey("matchLabels"), field)
			Expect(selector).NotTo(BeEmpty(), field)
		}
		for _, field := range []string{"serviceMonitorNamespaceSelector", "podMonitorNamespaceSelector", "probeNamespaceSelector", "scrapeConfigNamespaceSelector"} {
			selector, found, err := unstructured.NestedMap(agent, "spec", field)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), field)
			Expect(selector).To(HaveKey("matchLabels"), field)
		}

		By("确认 ScrapeConfig 示例覆盖 static 与五种一期 Kubernetes discovery")
		configObjects := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "collection-configs.yaml"))
		kinds := map[string]int{}
		discoveries := map[string]bool{}
		for _, object := range configObjects {
			kind, found, err := unstructured.NestedString(object, "kind")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			kinds[kind]++
			if kind != "ScrapeConfig" {
				continue
			}
			staticConfigs, found, err := unstructured.NestedSlice(object, "spec", "staticConfigs")
			Expect(err).NotTo(HaveOccurred())
			if found && len(staticConfigs) > 0 {
				discoveries["Static"] = true
			}
			kubernetesConfigs, found, err := unstructured.NestedSlice(object, "spec", "kubernetesSDConfigs")
			Expect(err).NotTo(HaveOccurred())
			if !found {
				continue
			}
			for _, item := range kubernetesConfigs {
				config, ok := item.(map[string]any)
				Expect(ok).To(BeTrue())
				role, found, err := unstructured.NestedString(config, "role")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				discoveries[role] = true
			}
		}
		Expect(kinds).To(HaveKeyWithValue("ServiceMonitor", 1))
		Expect(kinds).To(HaveKeyWithValue("PodMonitor", 1))
		Expect(kinds).To(HaveKeyWithValue("Probe", 1))
		Expect(discoveries).To(Equal(map[string]bool{
			"Static": true, "Pod": true, "Service": true, "Endpoints": true, "EndpointSlice": true, "Ingress": true,
		}))

		By("确认统一 runtime ClusterRole 覆盖一期资源且没有 nodes 权限")
		rbacObjects := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "runtime-rbac.yaml"))
		var role map[string]any
		for _, object := range rbacObjects {
			if object["kind"] == "ClusterRole" {
				role = object
			}
		}
		Expect(role).NotTo(BeNil())
		rules, found, err := unstructured.NestedSlice(role, "rules")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		resources := []string{}
		hasMetrics := false
		for _, item := range rules {
			rule := item.(map[string]any)
			if ruleResources, ok := rule["resources"].([]any); ok {
				for _, resource := range ruleResources {
					resources = append(resources, resource.(string))
				}
			}
			if urls, ok := rule["nonResourceURLs"].([]any); ok {
				for _, url := range urls {
					hasMetrics = hasMetrics || url == "/metrics"
				}
			}
		}
		sort.Strings(resources)
		Expect(resources).To(ConsistOf("services", "endpoints", "pods", "configmaps", "ingresses", "endpointslices"))
		Expect(resources).NotTo(ContainElement("nodes"))
		Expect(hasMetrics).To(BeTrue())
	})

	It("[UT-PREFLIGHT-GH59-001] 拒绝一期范围外的空 object selector", func() {
		By("准备带空 ServiceMonitor object selector 的 Agent manifest")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == *"create --dry-run=client --filename "*" --output json"* ]]; then
  printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{}}}'
  exit 0
fi
printf 'unexpected kubectl call: %s\n' "$*" >&2
exit 1
`)

		By("通过公开预检入口执行计划检查")
		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证预检以支持资格失败而不是输入错误拒绝空 selector")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"collection-configuration"`),
			ContainSubstring(`"reason":"EmptyObjectSelectorUnsupported"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-002] 拒绝 Node、FileSD 与其他一期外 ScrapeConfig discovery", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		for _, unsupported := range []struct {
			name string
			spec string
		}{
			{name: "Node", spec: `{"kubernetesSDConfigs":[{"role":"Node"}]}`},
			{name: "Node metadata", spec: `{"kubernetesSDConfigs":[{"role":"Pod","attachMetadata":{"node":true}}]}`},
			{name: "FileSD", spec: `{"fileSDConfigs":[{"files":["/etc/prometheus/targets.json"]}]}`},
			{name: "DNS", spec: `{"dnsSDConfigs":[{"names":["metrics.example"]}]}`},
			{name: "LightSail", spec: `{"lightSailSDConfigs":[{}]}`},
		} {
			By("准备一期外 " + unsupported.name + " discovery 计划")
			fakeBin := GinkgoT().TempDir()
			agentManifest := filepath.Join(fakeBin, "agent.yaml")
			configManifest := filepath.Join(fakeBin, "scrape-config.yaml")
			Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
			Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())

			writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
case "$*" in
  *"agent.yaml"*)
    printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"monitoring.fjsxy.github.io/agent":"example"}}}}'
    ;;
  *"scrape-config.yaml"*)
    printf '%s\n' "{\"apiVersion\":\"monitoring.coreos.com/v1alpha1\",\"kind\":\"ScrapeConfig\",\"metadata\":{\"name\":\"unsupported\",\"namespace\":\"monitoring\",\"labels\":{\"monitoring.fjsxy.github.io/agent\":\"example\"}},\"spec\":${PREFLIGHT_UNSUPPORTED_SPEC}}"
    ;;
  *)
    printf 'unexpected kubectl call: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)

			By("通过公开预检入口执行 discovery 支持子集检查")
			command := exec.Command(
				filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring",
				"--agent-service-account", "agent",
				"--config-namespaces", "monitoring",
				"--discovery-namespaces", "monitoring",
				"--agent-manifest", agentManifest,
				"--collection-config-manifest", configManifest,
			)
			command.Env = append(os.Environ(),
				"PATH="+fakeBin+":"+os.Getenv("PATH"),
				"PREFLIGHT_UNSUPPORTED_SPEC="+unsupported.spec,
			)
			output, err := command.CombinedOutput()

			By("验证一期外 discovery 以可定位原因非零退出")
			Expect(err).To(HaveOccurred(), string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(ContainSubstring(`"reason":"UnsupportedScrapeConfigDiscovery"`))
		}
	})

	It("[UT-PREFLIGHT-GH59-003] 空 namespace selector 要求覆盖 Operator watch scope 内全部 namespace", func() {
		By("准备跨 namespace 的 PodMonitor 与空 namespace selector")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","podMonitorSelector":{"matchLabels":{"mih":"selected"}},"podMonitorNamespaceSelector":{}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"PodMonitor","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{}}`,
			false,
		)

		By("以不足的 config namespace 范围执行预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "shared",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证空 selector 的全部 watch scope 语义导致覆盖不足失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionNamespaceScopeIncomplete"`),
			ContainSubstring(`"requiredNamespaces"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-004] 在采集配置自身 namespace 检查 Secret 引用", func() {
		By("准备引用缺失 Secret 的同 namespace ServiceMonitor")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"endpoints":[{"basicAuth":{"password":{"name":"missing-credentials","key":"password"}}}]}}`,
			true,
		)

		By("通过公开预检入口执行引用检查")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证诊断保留声明对象 namespace 与缺失 Secret 名称")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionConfigsSelected"`),
			ContainSubstring(`"reason":"ReferencedAssetUnavailable"`),
			ContainSubstring(`"namespace":"monitoring"`),
			ContainSubstring(`"name":"missing-credentials"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-007] 跨 namespace 同名 ConfigMap 不满足采集配置引用", func() {
		By("准备在 monitoring namespace 引用缺失 ConfigMap 的 ServiceMonitor")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"endpoints":[{"tlsConfig":{"ca":{"configMap":{"name":"missing-ca","key":"ca.crt"}}}}]}}`,
			false,
		)

		By("通过公开预检入口执行 ConfigMap 引用检查")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证只查询声明对象 namespace，并以 ConfigMap 缺失原因非零退出")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"ReferencedAssetUnavailable"`),
			ContainSubstring(`"namespace":"monitoring"`),
			ContainSubstring(`"kind":"configmap"`),
			ContainSubstring(`"name":"missing-ca"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-008] Secret/ConfigMap 存在但缺少引用 key 时预检失败", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		for _, fixture := range []struct {
			name, reference, expectedName, expectedKey string
			configMapExists                            bool
		}{
			{
				name:         "Secret",
				reference:    `"basicAuth":{"password":{"name":"missing-credentials","key":"password"}}`,
				expectedName: "missing-credentials",
				expectedKey:  "password",
			},
			{
				name:            "ConfigMap",
				reference:       `"tlsConfig":{"ca":{"configMap":{"name":"missing-ca","key":"ca.crt"}}}`,
				expectedName:    "missing-ca",
				expectedKey:     "ca.crt",
				configMapExists: true,
			},
		} {
			By("准备引用同 namespace " + fixture.name + " 缺失 key 的 ServiceMonitor")
			fakeBin := GinkgoT().TempDir()
			agentManifest := filepath.Join(fakeBin, "agent.yaml")
			configManifest := filepath.Join(fakeBin, "config.yaml")
			Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
			Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
			writeCollectionValidationKubectl(fakeBin,
				`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{"matchLabels":{"mih":"selected"}}}}`,
				`{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"endpoints":[{`+fixture.reference+`}]}}`,
				false,
				fixture.configMapExists,
			)

			By("通过公开预检入口执行 " + fixture.name + " 引用 key 覆盖检查")
			command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring", "--agent-service-account", "agent",
				"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
				"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
			)
			command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
			output, err := command.CombinedOutput()

			By("验证对象存在仍因缺少引用 key 非零退出")
			Expect(err).To(HaveOccurred(), string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(SatisfyAll(
				ContainSubstring(`"reason":"ReferencedAssetKeyUnavailable"`),
				ContainSubstring(`"namespace":"monitoring"`),
				ContainSubstring(`"name":"`+fixture.expectedName+`"`),
				ContainSubstring(`"key":"`+fixture.expectedKey+`"`),
			))
		}
	})

	It("[UT-PREFLIGHT-GH59-005] Namespace LabelSelector 的 NotIn 匹配缺少该 label 的 namespace", func() {
		By("准备位于无 excluded label namespace 的 PodMonitor")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","podMonitorSelector":{"matchLabels":{"mih":"selected"}},"podMonitorNamespaceSelector":{"matchExpressions":[{"key":"excluded","operator":"NotIn","values":["true"]}]}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"PodMonitor","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{}}`,
			false,
		)

		By("执行包含 NotIn namespace selector 的计划预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "*", "--discovery-namespaces", "shared",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证 selector 阶段成功，并在 fixture 未实现的后续 API discovery 边界明确失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionConfigsSelected"`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"CollectionConfigCoverageIncomplete"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-006] 空 namespace selector 只要求覆盖 Operator 实际 watch scope", func() {
		By("准备受限 Operator watch scope 与 scope 外 namespace")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","podMonitorSelector":{"matchLabels":{"mih":"selected"}},"podMonitorNamespaceSelector":{}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"PodMonitor","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{}}`,
			false,
		)
		GinkgoT().Setenv("PREFLIGHT_OPERATOR_ARGS", `--namespaces=monitoring,shared`)

		By("执行 Operator common watch scope 受限的计划预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring,shared", "--discovery-namespaces", "shared",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证 selector 阶段成功，并在 fixture 未实现的后续 API discovery 边界明确失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionNamespaceScopeCovered"`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"CollectionNamespaceScopeIncomplete"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-009] 不把 static labels 中的 name/key 当作 Secret 引用", func() {
		By("准备 labels 恰好包含 name/key 的合法静态 ScrapeConfig")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"staticConfigs":[{"targets":["example:9090"],"labels":{"name":"workload","key":"value"}}]}}`,
			false,
		)

		By("执行引用识别阶段并保留 fixture 后续阶段的预期失败")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证静态 label 未触发伪引用，并在后续 API discovery 边界明确失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"DiscoveryNamespaceScopeCovered"`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"ReferencedAssetUnavailable"`)),
			Not(ContainSubstring(`"reason":"ReferencedAssetKeyUnavailable"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-010] ScrapeConfig 省略 namespaces 时要求全 namespace discovery 覆盖", func() {
		By("准备使用 Kubernetes discovery 原生全 namespace 默认值的 ScrapeConfig")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"kubernetesSDConfigs":[{"role":"Pod"}]}}`,
			false,
		)

		By("以单 namespace discovery 声明执行严格预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证全 namespace 原生语义因 runtime RBAC 声明覆盖不足而非零退出")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"DiscoveryNamespaceScopeIncomplete"`),
			ContainSubstring(`"requiredDiscoveryNamespaces":["*"]`),
		))
	})

	It("[UT-PREFLIGHT-GH59-011] ScrapeConfig ownNamespace 指向 Agent namespace", func() {
		By("准备位于其他配置 namespace、但 discovery 使用 Agent ownNamespace 的 ScrapeConfig")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"mih":"selected"}},"scrapeConfigNamespaceSelector":{}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"ownNamespace":true}}]}}`,
			false,
		)

		By("以 Agent namespace 的 runtime discovery 覆盖执行计划检查")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "*", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证 ownNamespace 不被误解为 ScrapeConfig 对象所在 namespace")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"DiscoveryNamespaceScopeCovered"`),
			ContainSubstring(`"requiredDiscoveryNamespaces":["monitoring"]`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"DiscoveryNamespaceScopeIncomplete"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-012] ServiceMonitor、PodMonitor 与 Probe target namespace 必须被 runtime discovery 范围覆盖", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		for _, fixture := range []struct {
			name, objectField, kind, spec string
		}{
			{name: "ServiceMonitor", objectField: "serviceMonitorSelector", kind: "ServiceMonitor", spec: `{"namespaceSelector":{"matchNames":["shared"]},"selector":{},"endpoints":[{"port":"metrics"}]}`},
			{name: "PodMonitor", objectField: "podMonitorSelector", kind: "PodMonitor", spec: `{"namespaceSelector":{"matchNames":["shared"]},"selector":{},"podMetricsEndpoints":[{"port":"metrics"}]}`},
			{name: "Probe ingress", objectField: "probeSelector", kind: "Probe", spec: `{"prober":{"url":"blackbox:9115"},"targets":{"ingress":{"namespaceSelector":{"matchNames":["shared"]},"selector":{}}}}`},
		} {
			By("准备发现 shared namespace target 的 " + fixture.name)
			fakeBin := GinkgoT().TempDir()
			agentManifest := filepath.Join(fakeBin, "agent.yaml")
			configManifest := filepath.Join(fakeBin, "config.yaml")
			Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
			Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
			writeCollectionValidationKubectl(fakeBin,
				`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","`+fixture.objectField+`":{"matchLabels":{"mih":"selected"}}}}`,
				`{"apiVersion":"monitoring.coreos.com/v1","kind":"`+fixture.kind+`","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":`+fixture.spec+`}`,
				false,
			)

			By("以未覆盖 shared 的 runtime discovery 范围执行严格预检")
			command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring", "--agent-service-account", "agent",
				"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
				"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
			)
			command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
			output, err := command.CombinedOutput()

			By("验证 " + fixture.name + " 的真实 target namespace 覆盖不足导致非零退出")
			Expect(err).To(HaveOccurred(), string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(SatisfyAll(
				ContainSubstring(`"reason":"DiscoveryNamespaceScopeIncomplete"`),
				ContainSubstring(`"requiredDiscoveryNamespaces":["shared"]`),
			))
		}
	})
})

func writeCollectionValidationKubectl(fakeBin, agentJSON, configJSON string, rejectSecret bool, configMapExists ...bool) {
	GinkgoHelper()
	rejectSecretValue := "0"
	if rejectSecret {
		rejectSecretValue = "1"
	}
	configMapExistsValue := "0"
	if len(configMapExists) > 0 && configMapExists[0] {
		configMapExistsValue = "1"
	}
	script := `#!/usr/bin/env bash
set -eu
case "$*" in
  *"agent.yaml"*) printf '%s\n' "${PREFLIGHT_AGENT_JSON}" ;;
  *"config.yaml"*) printf '%s\n' "${PREFLIGHT_CONFIG_JSON}" ;;
  "version --output json") printf '%s\n' '{"serverVersion":{"major":"1","minor":"28","gitVersion":"v1.28.15"}}' ;;
  "get namespace "*" --output name") printf 'namespace/%s\n' "$3" ;;
  "--namespace monitoring get serviceaccount agent --output name") printf '%s\n' 'serviceaccount/agent' ;;
  "get namespaces --output json")
    printf '%s\n' '{"items":[{"metadata":{"name":"monitoring","labels":{"scope":"agent"}}},{"metadata":{"name":"shared","labels":{"scope":"shared"}}},{"metadata":{"name":"prometheus-operator","labels":{}}}]}'
    ;;
  "--namespace prometheus-operator get deployment prometheus-operator --output json")
    jq --null-input --arg args "${PREFLIGHT_OPERATOR_ARGS:-}" '{spec:{template:{spec:{serviceAccountName:"prometheus-operator",containers:[{name:"prometheus-operator",args:(if $args == "" then [] else [$args] end)}]}}}}'
    ;;
  "--namespace monitoring get secret missing-credentials --output json")
    if [[ "${PREFLIGHT_REJECT_SECRET}" == 1 ]]; then
      printf '%s\n' 'secret not found' >&2
      exit 1
    fi
    printf '%s\n' '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"missing-credentials","namespace":"monitoring"},"data":{"username":"dXNlcg=="}}'
    ;;
  "--namespace monitoring get configmap missing-ca --output json")
    if [[ "${PREFLIGHT_CONFIGMAP_EXISTS}" == 1 ]]; then
      printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"missing-ca","namespace":"monitoring"},"data":{"other-key":"value"}}'
      exit 0
    fi
    printf '%s\n' 'configmap exists only in another namespace' >&2
    exit 1
    ;;
  *) printf 'unexpected kubectl call: %s\n' "$*" >&2; exit 1 ;;
esac
`
	writeExecutable(filepath.Join(fakeBin, "kubectl"), script)
	GinkgoT().Setenv("PREFLIGHT_AGENT_JSON", agentJSON)
	GinkgoT().Setenv("PREFLIGHT_CONFIG_JSON", configJSON)
	GinkgoT().Setenv("PREFLIGHT_REJECT_SECRET", rejectSecretValue)
	GinkgoT().Setenv("PREFLIGHT_CONFIGMAP_EXISTS", configMapExistsValue)
}

func readManifestObjects(path string) []map[string]any {
	GinkgoHelper()
	contents, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)
	objects := []map[string]any{}
	for {
		object := map[string]any{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if len(object) > 0 {
			objects = append(objects, object)
		}
	}
	return objects
}
