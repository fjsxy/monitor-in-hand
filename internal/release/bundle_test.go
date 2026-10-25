package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	product "github.com/fjsxy/monitor-in-hand/internal/release"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("可消费的发布包", func() {
	It("UT-RELEASE-GH87-002 逐文件核对来源和双架构身份，拒绝篡改与不完整产物", func() {
		dir, manifest := bundleFixture()
		Expect(product.VerifyBundle(dir, fixtureCommit)).To(Succeed())
		By("独立改写已锁定文件，不能靠名字相同绕过校验")
		Expect(os.WriteFile(filepath.Join(dir, manifest.CRD), []byte("tampered"), 0600)).To(Succeed())
		Expect(product.VerifyBundle(dir, fixtureCommit)).NotTo(Succeed())
		for _, change := range []func(*product.Manifest){
			func(m *product.Manifest) { m.SourceCommit = strings.Repeat("c", 40) },
			func(m *product.Manifest) { delete(m.Image.Platforms, "linux/amd64") },
			func(m *product.Manifest) { m.Image.IndexDigest = "latest" },
			func(m *product.Manifest) { m.SchemaVersion = 9 },
			func(m *product.Manifest) { m.Files["../outside"] = strings.Repeat("a", 64) },
			func(m *product.Manifest) { m.Image.SourceCommit = "unknown" },
		} {
			dir, m := bundleFixture()
			change(m)
			writeManifest(dir, m)
			Expect(product.VerifyBundle(dir, fixtureCommit)).NotTo(Succeed())
		}
	})

	It("UT-RELEASE-GH87-003 校验和基线拒绝零写入，带 UID/RV test 更新后才升级 chart", func() {
		dir, m := bundleFixture()
		oldData, err := os.ReadFile(filepath.Join(dir, m.Legacy.CRD))
		Expect(err).NotTo(HaveOccurred())
		old := &extv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(oldData, old)).To(Succeed())
		old.UID = "original-uid"
		old.ResourceVersion = "41"
		old.Spec.Conversion = &extv1.CustomResourceConversion{Strategy: extv1.NoneConverter}
		current, _ := json.MarshalIndent(old, "", "  ")
		commands := [][]string{}
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			commands = append(commands, append([]string{name}, args...))
			if containsArg(args, "get") {
				return current, nil
			}
			if containsArg(args, "--patch-file") {
				data, e := os.ReadFile(args[len(args)-1])
				Expect(e).NotTo(HaveOccurred())
				var patch []map[string]any
				Expect(json.Unmarshal(data, &patch)).To(Succeed())
				Expect(patch).To(HaveLen(3))
				Expect(patch[0]).To(HaveKeyWithValue("value", "original-uid"))
				Expect(patch[1]).To(HaveKeyWithValue("value", "41"))
				Expect(patch[2]).To(HaveKeyWithValue("path", "/spec"))
			}
			return nil, nil
		}
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "kind-owned", "monitoring", "operator", runner)).To(Succeed())
		Expect(commands).To(HaveLen(3))
		Expect(commands[1]).To(ContainElements("patch", "--type=json", "--patch-file"))
		Expect(commands[2]).To(ContainElements("helm", "upgrade", "--skip-crds", filepath.Join(dir, m.Chart)))
		By("当前 schema 已是新基线时只读后升级 Helm，新装则先 create")
		data, e := os.ReadFile(filepath.Join(dir, m.CRD))
		Expect(e).NotTo(HaveOccurred())
		desired := &extv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(data, desired)).To(Succeed())
		desired.UID = old.UID
		desired.ResourceVersion = "42"
		current, _ = json.Marshal(desired)
		commands = nil
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "owned", "ns", "op", runner)).To(Succeed())
		Expect(commands).To(HaveLen(2))
		current = nil
		commands = nil
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "owned", "ns", "op", runner)).To(Succeed())
		Expect(commands[1]).To(ContainElement("create"))
		By("未知漂移只读即拒绝，更新冲突不运行 Helm，坏包不调用 Kubernetes")
		old.Spec.Group = "foreign.example"
		current, _ = json.Marshal(old)
		commands = nil
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "owned", "ns", "op", runner)).NotTo(Succeed())
		Expect(commands).To(HaveLen(1))
		old.Spec.Group = "monitoring.fjsxy.github.io"
		current, _ = json.Marshal(old)
		commands = nil
		failing := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if containsArg(args, "patch") {
				commands = append(commands, []string{"conflict"})
				return nil, fmt.Errorf("409 conflict")
			}
			return runner(ctx, name, args...)
		}
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "owned", "ns", "op", failing)).NotTo(Succeed())
		Expect(commands).To(HaveLen(2))
		commands = nil
		Expect(product.Upgrade(context.Background(), dir, "wrong", "owned", "ns", "op", runner)).NotTo(Succeed())
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "", "ns", "op", runner)).NotTo(Succeed())
		Expect(commands).To(BeEmpty())
	})

})

const fixtureCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// bundleFixture 独立构造最小真实 tgz 和文件，不调用发布器为自己生成期望。
func bundleFixture() (string, *product.Manifest) {
	dir := GinkgoT().TempDir()
	crd, err := os.ReadFile("../../config/crd/bases/monitoring.fjsxy.github.io_kubestatemetrics.yaml")
	Expect(err).NotTo(HaveOccurred())
	image := product.Image{Repository: "docker.io/fjsxy/monitor-in-hand", Tag: "0.2.0-rc.1", SourceCommit: fixtureCommit, IndexDigest: "sha256:" + strings.Repeat("1", 64), Platforms: map[string]string{"linux/amd64": "sha256:" + strings.Repeat("2", 64), "linux/arm64": "sha256:" + strings.Repeat("3", 64)}}
	m := &product.Manifest{SchemaVersion: 1, Version: "0.2.0-rc.1", SourceCommit: fixtureCommit, TestedCommit: fixtureCommit, Image: image, Chart: "monitor-in-hand-0.2.0-rc.1.tgz", CRD: "kubestatemetrics.yaml", RBAC: "manager-role.yaml", Contract: "servicemonitor-contract.json", Files: map[string]string{}, Legacy: product.Baseline{Version: "0.1.0", Chart: "legacy.tgz", CRD: "legacy.yaml", Image: image}, Previous: product.Baseline{Version: "0.1.0-pre-sm.test", SourceCommit: fixtureCommit, Chart: "previous.tgz", CRD: "legacy.yaml", Image: image}}
	m.Legacy.Image.Tag = m.Legacy.Version
	m.Previous.Image.Tag = m.Previous.Version
	chart := map[string][]byte{"monitor-in-hand/Chart.yaml": []byte("name: monitor-in-hand\nversion: 0.2.0-rc.1\nappVersion: 0.2.0-rc.1\n"), "monitor-in-hand/values.yaml": []byte("image:\n  digest: " + image.IndexDigest + "\n"), "monitor-in-hand/crds/monitoring.fjsxy.github.io_kubestatemetrics.yaml": crd, "monitor-in-hand/generated/manager-role.yaml": []byte("kind: ClusterRole\n")}
	var archive bytes.Buffer
	zip := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(zip)
	for name, data := range chart {
		Expect(tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))})).To(Succeed())
		_, err = tarWriter.Write(data)
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(tarWriter.Close()).To(Succeed())
	Expect(zip.Close()).To(Succeed())
	legacy := &extv1.CustomResourceDefinition{}
	Expect(yaml.Unmarshal(crd, legacy)).To(Succeed())
	delete(legacy.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "serviceMonitor")
	delete(legacy.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties, "serviceMonitor")
	legacyData, err := yaml.Marshal(legacy)
	Expect(err).NotTo(HaveOccurred())
	files := map[string][]byte{m.Chart: archive.Bytes(), m.CRD: crd, m.RBAC: []byte("kind: ClusterRole\n"), m.Contract: []byte(`{"contractVersion":1,"capability":"ksm-companion-servicemonitor/v1"}`), "legacy.tgz": []byte("legacy-chart"), "previous.tgz": []byte("previous-chart"), "legacy.yaml": legacyData}
	for name, data := range files {
		Expect(os.WriteFile(filepath.Join(dir, name), data, 0600)).To(Succeed())
		m.Files[name] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	writeManifest(dir, m)
	return dir, m
}

func writeManifest(dir string, m *product.Manifest) {
	data, err := json.Marshal(m)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, "release.json"), data, 0600)).To(Succeed())
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
