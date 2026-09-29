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
			kind, _, _ := unstructured.NestedString(object, "kind")
			kinds[kind]++
			if kind != "ScrapeConfig" {
				continue
			}
			staticConfigs, _, _ := unstructured.NestedSlice(object, "spec", "staticConfigs")
			if len(staticConfigs) > 0 {
				discoveries["Static"] = true
			}
			kubernetesConfigs, _, _ := unstructured.NestedSlice(object, "spec", "kubernetesSDConfigs")
			for _, item := range kubernetesConfigs {
				role, _, _ := unstructured.NestedString(item.(map[string]any), "role")
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
		} {
			By(unsupported.name)
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

			Expect(err).To(HaveOccurred(), string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(ContainSubstring(`"reason":"UnsupportedScrapeConfigDiscovery"`))
		}
	})

	It("[UT-PREFLIGHT-GH59-003] 空 namespace selector 要求覆盖 Operator watch scope 内全部 namespace", func() {
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

		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionNamespaceScopeIncomplete"`),
			ContainSubstring(`"requiredNamespaces"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-004] 在采集配置自身 namespace 检查 Secret 引用", func() {
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

		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

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
})

func writeCollectionValidationKubectl(fakeBin, agentJSON, configJSON string, rejectSecret bool) {
	GinkgoHelper()
	rejectSecretValue := "0"
	if rejectSecret {
		rejectSecretValue = "1"
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
  "--namespace monitoring get secret missing-credentials --output name")
    if [[ "${PREFLIGHT_REJECT_SECRET}" == 1 ]]; then
      printf '%s\n' 'secret not found' >&2
      exit 1
    fi
    printf '%s\n' 'secret/missing-credentials'
    ;;
  *) printf 'unexpected kubectl call: %s\n' "$*" >&2; exit 1 ;;
esac
`
	writeExecutable(filepath.Join(fakeBin, "kubectl"), script)
	GinkgoT().Setenv("PREFLIGHT_AGENT_JSON", agentJSON)
	GinkgoT().Setenv("PREFLIGHT_CONFIG_JSON", configJSON)
	GinkgoT().Setenv("PREFLIGHT_REJECT_SECRET", rejectSecretValue)
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
