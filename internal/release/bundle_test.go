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

	It("UT-RELEASE-GH87-003 校验失败零写入，CRD apply 成功后才升级同一 chart", func() {
		dir, m := bundleFixture()
		commands := [][]string{}
		runner := func(ctx context.Context, name string, args ...string) error {
			commands = append(commands, append([]string{name}, args...))
			return nil
		}
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "kind-owned", "monitoring", "monitor-in-hand", runner)).To(Succeed())
		Expect(commands).To(HaveLen(2))
		Expect(commands[0]).To(Equal([]string{"kubectl", "--context", "kind-owned", "apply", "--server-side", "--field-manager=monitor-in-hand-release", "--filename", filepath.Join(dir, m.CRD)}))
		Expect(commands[1]).To(ContainElements("helm", "upgrade", "--skip-crds", filepath.Join(dir, m.Chart), "--kube-context", "kind-owned"))
		By("外部 API 写失败时不会继续改变 Operator，坏 bundle 在任何命令前拒绝")
		commands = nil
		failing := func(ctx context.Context, name string, args ...string) error {
			commands = append(commands, []string{name})
			return fmt.Errorf("conflict")
		}
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "kind-owned", "monitoring", "operator", failing)).NotTo(Succeed())
		Expect(commands).To(HaveLen(1))
		commands = nil
		Expect(product.Upgrade(context.Background(), dir, "wrong-sha", "kind-owned", "monitoring", "operator", runner)).NotTo(Succeed())
		Expect(product.Upgrade(context.Background(), dir, fixtureCommit, "", "monitoring", "operator", runner)).NotTo(Succeed())
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
	files := map[string][]byte{m.Chart: archive.Bytes(), m.CRD: crd, m.RBAC: []byte("kind: ClusterRole\n"), m.Contract: []byte(`{"contractVersion":1,"capability":"ksm-companion-servicemonitor/v1"}`), "legacy.tgz": []byte("legacy-chart"), "previous.tgz": []byte("previous-chart"), "legacy.yaml": []byte("kind: CustomResourceDefinition\n")}
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
