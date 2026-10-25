package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

const Repository = "docker.io/fjsxy/monitor-in-hand"
const PreviousCommit = "2b2a8b79a52070fb89a08f5afae2b7698901c999"
const PreviousVersion = "0.1.0-pre-sm.2b2a8b7"

type Image struct {
	Repository   string            `json:"repository"`
	Tag          string            `json:"tag"`
	SourceCommit string            `json:"sourceCommit,omitempty"`
	IndexDigest  string            `json:"indexDigest"`
	Platforms    map[string]string `json:"platforms"`
}
type Baseline struct {
	Version      string `json:"version"`
	SourceCommit string `json:"sourceCommit,omitempty"`
	Chart        string `json:"chart"`
	CRD          string `json:"crd"`
	Image        Image  `json:"image"`
}
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	Version       string            `json:"version"`
	SourceCommit  string            `json:"sourceCommit"`
	TestedCommit  string            `json:"testedCommit"`
	Image         Image             `json:"image"`
	Chart         string            `json:"chart"`
	CRD           string            `json:"crd"`
	RBAC          string            `json:"rbac"`
	Contract      string            `json:"contract"`
	Files         map[string]string `json:"files"`
	Legacy        Baseline          `json:"legacy"`
	Previous      Baseline          `json:"previous"`
}

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?$`)

// VerifyBundle 核对完整 bundle 后才允许安装；调用者必须提供已锁定的源提交。
func VerifyBundle(dir, expectedCommit string) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	if m.SchemaVersion != 1 || !versionPattern.MatchString(m.Version) || !commitPattern.MatchString(expectedCommit) || m.SourceCommit != expectedCommit || m.TestedCommit != expectedCommit || m.Image.SourceCommit != expectedCommit || m.Image.Tag != m.Version {
		return fmt.Errorf("发布来源、版本或 manifest schema 不匹配")
	}
	for _, image := range []Image{m.Image, m.Legacy.Image, m.Previous.Image} {
		if err := validateImage(image); err != nil {
			return err
		}
	}
	if m.Legacy.Version != "0.1.0" || m.Legacy.Image.Tag != m.Legacy.Version || m.Previous.Image.Tag != m.Previous.Version || !commitPattern.MatchString(m.Previous.SourceCommit) || m.Previous.Image.SourceCommit != m.Previous.SourceCommit || !versionPattern.MatchString(m.Previous.Version) {
		return fmt.Errorf("缺少明确的历史或 pre-SM 对照基线")
	}
	for _, required := range []string{m.Chart, m.CRD, m.RBAC, m.Contract, m.Legacy.Chart, m.Legacy.CRD, m.Previous.Chart, m.Previous.CRD} {
		if _, ok := m.Files[required]; !ok || required == "" {
			return fmt.Errorf("发布包缺少必选产物 %s", required)
		}
	}
	// 关键：拒绝路径逃逸与 symlink，且先验证全部 hash，再解析或调用任何集群命令。
	for name, want := range m.Files {
		if name != filepath.Base(name) || name == "." || strings.ContainsAny(name, "/\\") || !hashPattern.MatchString(want) {
			return fmt.Errorf("非法产物名称或摘要 %q", name)
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("产物必须是普通文件: %s", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != want {
			return fmt.Errorf("产物 hash 不匹配: %s", name)
		}
	}
	crd, err := os.ReadFile(filepath.Join(dir, m.CRD))
	if err != nil {
		return err
	}
	if err = CheckCapability(crd); err != nil {
		return err
	}
	contract, err := os.ReadFile(filepath.Join(dir, m.Contract))
	if err != nil {
		return err
	}
	var c map[string]any
	if json.Unmarshal(contract, &c) != nil || c["contractVersion"] != float64(1) || c["capability"] != "ksm-companion-servicemonitor/v1" {
		return fmt.Errorf("配套采集机器契约不兼容")
	}
	chart, err := readChart(filepath.Join(dir, m.Chart))
	if err != nil {
		return err
	}
	var metadata, values map[string]any
	if yaml.Unmarshal(chart["monitor-in-hand/Chart.yaml"], &metadata) != nil || metadata["name"] != "monitor-in-hand" || metadata["version"] != m.Version || metadata["appVersion"] != m.Version {
		return fmt.Errorf("chart 版本与发布记录不符")
	}
	if yaml.Unmarshal(chart["monitor-in-hand/values.yaml"], &values) != nil || lookup(values, "image.digest") != m.Image.IndexDigest {
		return fmt.Errorf("chart 未锁定本候选 image digest")
	}
	rbac, err := os.ReadFile(filepath.Join(dir, m.RBAC))
	if err != nil {
		return err
	}
	if !bytes.Equal(chart["monitor-in-hand/crds/monitoring.fjsxy.github.io_kubestatemetrics.yaml"], crd) || !bytes.Equal(chart["monitor-in-hand/generated/manager-role.yaml"], rbac) {
		return fmt.Errorf("chart 与独立 CRD/RBAC 不一致")
	}
	return nil
}

func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err = json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func validateImage(image Image) error {
	if image.Repository != Repository || !validDigest(image.IndexDigest) {
		return fmt.Errorf("镜像必须锁定本项目 registry 的 OCI digest")
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if !validDigest(image.Platforms[platform]) {
			return fmt.Errorf("缺少平台 digest: %s", platform)
		}
	}
	return nil
}

func validDigest(digest string) bool {
	return strings.HasPrefix(digest, "sha256:") && hashPattern.MatchString(strings.TrimPrefix(digest, "sha256:"))
}

// readChart 只读取受限大小的普通 tar 项，不在文件系统展开候选包。
func readChart(path string) (map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	t := tar.NewReader(z)
	files := map[string][]byte{}
	total := int64(0)
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg || filepath.IsAbs(h.Name) || strings.Contains(h.Name, "..") || h.Size < 0 || h.Size > 8<<20 {
			return nil, fmt.Errorf("非法 chart 项 %q", h.Name)
		}
		total += h.Size
		if total > 32<<20 {
			return nil, fmt.Errorf("chart 超过大小上限")
		}
		if _, exists := files[h.Name]; exists {
			return nil, fmt.Errorf("重复 chart 项 %q", h.Name)
		}
		data, e := io.ReadAll(t)
		if e != nil {
			return nil, e
		}
		files[h.Name] = data
	}
	return files, nil
}
