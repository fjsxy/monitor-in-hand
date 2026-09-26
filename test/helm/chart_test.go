package helm_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

var _ = Describe("Operator Helm chart", func() {
	It("HELM-001 渲染固定的单副本控制面契约", func() {
		objects := renderChart()

		Expect(resourceKinds(objects)).To(ConsistOf(
			"ServiceAccount",
			"ClusterRole",
			"RoleBinding",
			"Deployment",
		))

		deployment := objectByKind(objects, "Deployment")
		Expect(nested(deployment, "spec", "replicas")).To(BeEquivalentTo(1))
		Expect(nested(deployment, "spec", "strategy", "type")).To(Equal("RollingUpdate"))
		Expect(nested(deployment, "spec", "strategy", "rollingUpdate", "maxSurge")).To(BeEquivalentTo(1))
		Expect(nested(deployment, "spec", "strategy", "rollingUpdate", "maxUnavailable")).To(BeEquivalentTo(0))
		Expect(nested(deployment, "spec", "template", "spec", "terminationGracePeriodSeconds")).To(BeEquivalentTo(30))
		Expect(nested(deployment, "spec", "template", "spec", "nodeSelector", "kubernetes.io/os")).To(Equal("linux"))

		podSecurity := nestedMap(deployment, "spec", "template", "spec", "securityContext")
		Expect(podSecurity).To(SatisfyAll(
			HaveKeyWithValue("runAsNonRoot", true),
			HaveKeyWithValue("seccompProfile", HaveKeyWithValue("type", "RuntimeDefault")),
		))

		container := firstContainer(deployment)
		Expect(nested(deployment, "spec", "template", "spec", "serviceAccountName")).To(Equal("test-release-monitor-in-hand"))
		Expect(container["image"]).To(Equal("docker.io/fjsxy/monitor-in-hand:0.1.0"))
		Expect(container["imagePullPolicy"]).To(Equal("IfNotPresent"))
		Expect(container["args"]).To(ConsistOf(
			"--leader-elect=true",
			"--health-probe-bind-address=:8081",
			"--log-level=info",
		))
		Expect(container["ports"]).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "health"),
			HaveKeyWithValue("containerPort", BeEquivalentTo(8081)),
		)))
		Expect(container["securityContext"]).To(SatisfyAll(
			HaveKeyWithValue("allowPrivilegeEscalation", false),
			HaveKeyWithValue("readOnlyRootFilesystem", true),
			HaveKeyWithValue("capabilities", HaveKeyWithValue("drop", ConsistOf("ALL"))),
		))
		Expect(nestedMap(container, "livenessProbe", "httpGet")).To(SatisfyAll(
			HaveKeyWithValue("path", "/healthz"),
			HaveKeyWithValue("port", "health"),
		))
		Expect(nestedMap(container, "readinessProbe", "httpGet")).To(SatisfyAll(
			HaveKeyWithValue("path", "/readyz"),
			HaveKeyWithValue("port", "health"),
		))
	})

	It("HELM-002 保持生成 RBAC 最小且同步", func() {
		objects := renderChart()
		role := objectByKind(objects, "ClusterRole")
		rules := nested(role, "rules")

		Expect(rules).To(Equal(generatedRBACRules()))
		Expect(string(mustRenderChart())).NotTo(ContainSubstring("monitoring.coreos.com"))
	})

	It("HELM-003 健康监听、container port 与 probes 共用一个端口", func() {
		objects := renderChart("--set", "healthProbe.port=9095", "--set", "logLevel=debug")
		container := firstContainer(objectByKind(objects, "Deployment"))

		Expect(container["args"]).To(ConsistOf(
			"--leader-elect=true",
			"--health-probe-bind-address=:9095",
			"--log-level=debug",
		))
		Expect(container["ports"]).To(ContainElement(HaveKeyWithValue("containerPort", BeEquivalentTo(9095))))
		Expect(nested(container, "livenessProbe", "httpGet", "port")).To(Equal("health"))
		Expect(nested(container, "readinessProbe", "httpGet", "port")).To(Equal("health"))
	})

	DescribeTable("HELM-004 在 render 阶段拒绝不支持的 values",
		func(arguments ...string) {
			output, err := renderChartError(arguments...)
			Expect(err).To(HaveOccurred(), output)
		},
		Entry("特权端口", "--set", "healthProbe.port=80"),
		Entry("超过 65535 的端口", "--set", "healthProbe.port=65536"),
		Entry("不支持的日志级别", "--set", "logLevel=trace"),
		Entry("Linux selector 冲突", "--set", "nodeSelector.kubernetes\\.io/os=windows"),
		Entry("未声明的设置", "--set", "replicaCount=2"),
	)

	It("HELM-005 只开放约定的 scheduling 与 runtime values", func() {
		root := repositoryRoot()
		values := decodeYAMLFile(filepath.Join(root, "charts", "monitor-in-hand", "values.yaml"))
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		Expect(keys).To(Equal([]string{
			"affinity",
			"healthProbe",
			"logLevel",
			"nodeSelector",
			"resources",
			"tolerations",
			"topologySpreadConstraints",
		}))

		deployment := objectByKind(renderChart(
			"--values",
			filepath.Join(root, "test", "helm", "testdata", "custom-values.yaml"),
		), "Deployment")
		podSpec := nestedMap(deployment, "spec", "template", "spec")
		Expect(nested(podSpec, "nodeSelector", "kubernetes.io/os")).To(Equal("linux"))
		Expect(nested(podSpec, "nodeSelector", "topology.kubernetes.io/zone")).To(Equal("test-zone"))
		Expect(nested(podSpec, "affinity", "nodeAffinity")).NotTo(BeNil())
		Expect(nested(podSpec, "tolerations")).To(HaveLen(1))
		Expect(nested(podSpec, "topologySpreadConstraints")).To(HaveLen(1))
		Expect(nested(firstContainer(deployment), "resources", "requests", "cpu")).To(Equal("50m"))
	})

	It("HELM-006 携带与生成源同步的 KubeStateMetrics CRD", func() {
		root := repositoryRoot()
		generatedPath := filepath.Join(root, "config", "crd", "bases", "monitoring.fjsxy.github.io_kubestatemetrics.yaml")
		chartPath := filepath.Join(root, "charts", "monitor-in-hand", "crds", "monitoring.fjsxy.github.io_kubestatemetrics.yaml")
		generated, err := os.ReadFile(generatedPath)
		Expect(err).NotTo(HaveOccurred())
		chartCRD, err := os.ReadFile(chartPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(chartCRD).To(Equal(generated))

		crd := decodeYAMLFile(chartPath)
		Expect(nested(crd, "spec", "scope")).To(Equal("Cluster"))
		Expect(nested(crd, "spec", "group")).To(Equal("monitoring.fjsxy.github.io"))
		Expect(nested(crd, "spec", "versions", "0", "name")).To(Equal("v1alpha1"))
		Expect(nested(crd, "spec", "versions", "0", "schema", "openAPIV3Schema", "required")).To(ContainElement("spec"))
		Expect(nested(crd, "spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec", "properties", "enabled", "default")).To(BeTrue())
		Expect(nested(crd, "spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec", "properties", "suspended", "default")).To(BeFalse())
		Expect(nested(crd, "spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec", "properties", "deployment", "default", "mode")).To(Equal("Deployment"))
	})

	It("HELM-007 只授予默认 Deployment 调谐所需业务权限", func() {
		role := objectByKind(renderChart(), "ClusterRole")
		rules := nested(role, "rules").([]any)

		Expect(rules).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("apiGroups", ConsistOf("monitoring.fjsxy.github.io")),
			HaveKeyWithValue("resources", ConsistOf("kubestatemetrics/status")),
			HaveKeyWithValue("verbs", ConsistOf("get", "patch", "update")),
		)))
		Expect(rules).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("apiGroups", ConsistOf("apps")),
			HaveKeyWithValue("resources", ConsistOf("deployments")),
			HaveKeyWithValue("verbs", ConsistOf("create", "get", "list", "patch", "update", "watch")),
		)))
		Expect(string(mustRenderChart())).NotTo(ContainSubstring("monitoring.coreos.com"))
	})
})

func renderChart(extraArgs ...string) []*unstructured.Unstructured {
	output := mustRenderChart(extraArgs...)

	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var objects []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		err := decoder.Decode(object)
		if err == io.EOF {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if object.GetKind() != "" {
			objects = append(objects, object)
		}
	}
	return objects
}

func mustRenderChart(extraArgs ...string) []byte {
	output, err := renderChartError(extraArgs...)
	Expect(err).NotTo(HaveOccurred(), output)
	return []byte(output)
}

func renderChartError(extraArgs ...string) (string, error) {
	root := repositoryRoot()
	helm := os.Getenv("HELM_BIN")
	if helm == "" {
		helm = filepath.Join(root, ".tools", "bin", "helm")
	} else if !filepath.IsAbs(helm) && filepath.Dir(helm) != "." {
		helm = filepath.Join(root, helm)
	}
	args := []string{"template", "test-release", filepath.Join(root, "charts", "monitor-in-hand"), "--namespace", "monitor-system"}
	args = append(args, extraArgs...)
	command := exec.Command(helm, args...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func repositoryRoot() string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	Expect(err).NotTo(HaveOccurred())
	return root
}

func generatedRBACRules() any {
	manifest := decodeYAMLFile(filepath.Join(repositoryRoot(), "config", "rbac", "role.yaml"))
	return nested(manifest, "rules")
}

func decodeYAMLFile(path string) map[string]any {
	contents, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	object := map[string]any{}
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)
	Expect(decoder.Decode(&object)).To(Succeed())
	return object
}

func resourceKinds(objects []*unstructured.Unstructured) []string {
	kinds := make([]string, 0, len(objects))
	for _, object := range objects {
		kinds = append(kinds, object.GetKind())
	}
	return kinds
}

func objectByKind(objects []*unstructured.Unstructured, kind string) map[string]any {
	for _, object := range objects {
		if object.GetKind() == kind {
			return object.Object
		}
	}
	Fail("rendered chart did not contain " + kind)
	return nil
}

func nested(object map[string]any, fields ...string) any {
	var value any = object
	for _, field := range fields {
		switch typed := value.(type) {
		case map[string]any:
			var found bool
			value, found = typed[field]
			Expect(found).To(BeTrue(), "missing field %v", fields)
		case []any:
			index := 0
			_, err := fmt.Sscanf(field, "%d", &index)
			Expect(err).NotTo(HaveOccurred(), "field %q is not a list index", field)
			Expect(index).To(BeNumerically("<", len(typed)), "missing field %v", fields)
			value = typed[index]
		default:
			Fail("cannot descend into field " + field)
		}
	}
	return value
}

func nestedMap(object map[string]any, fields ...string) map[string]any {
	value, found, err := unstructured.NestedMap(object, fields...)
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue(), "missing map %v", fields)
	return value
}

func firstContainer(deployment map[string]any) map[string]any {
	containers, found, err := unstructured.NestedSlice(deployment, "spec", "template", "spec", "containers")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	Expect(containers).To(HaveLen(1))
	container, ok := containers[0].(map[string]any)
	Expect(ok).To(BeTrue())
	return container
}
