// release 提供可重复的候选发布与显式 CRD 升级入口；凭据只交给现有 Docker/gh 客户端。
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	product "github.com/fjsxy/monitor-in-hand/internal/release"
	"sigs.k8s.io/yaml"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: release images|package|publish|verify|capability|upgrade")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	source := fs.String("source-commit", "", "锁定的完整源提交")
	version := fs.String("version", "", "候选版本")
	dir := fs.String("bundle", "", "已下载或待生成的发布包目录")
	file := fs.String("file", "", "部署 CRD 文件")
	kubeContext := fs.String("context", "", "显式 Kubernetes context")
	namespace := fs.String("namespace", "monitor-in-hand-system", "Operator namespace")
	name := fs.String("release", "monitor-in-hand", "Helm release")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "capability":
		data, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		return product.CheckCapability(data)
	case "verify":
		return product.VerifyBundle(*dir, *source)
	case "upgrade":
		return product.Upgrade(ctx, *dir, *source, *kubeContext, *namespace, *name, output)
	case "images", "package":
		if err := cleanSource(ctx, *source); err != nil {
			return err
		}
		data, err := os.ReadFile("charts/monitor-in-hand/Chart.yaml")
		if err != nil {
			return err
		}
		var chart map[string]any
		if err = yaml.Unmarshal(data, &chart); err != nil {
			return err
		}
		if chart["version"] != *version || chart["appVersion"] != *version {
			return fmt.Errorf("候选版本必须与源 chart/version 和 appVersion 相同")
		}
		if args[0] == "images" {
			return publishImages(ctx, *source, *version)
		}
		return packageBundle(ctx, *dir, *source, *version)
	case "publish":
		if err := product.VerifyBundle(*dir, *source); err != nil {
			return err
		}
		m, err := product.ReadManifest(*dir)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(*dir)
		if err != nil {
			return err
		}
		args := []string{"release", "create", "v" + m.Version, "--repo", "fjsxy/monitor-in-hand", "--target", m.SourceCommit, "--prerelease", "--title", "配套 ServiceMonitor 候选 " + m.Version, "--notes-file", "docs/releases/servicemonitor.md"}
		for _, entry := range entries {
			if !entry.IsDir() {
				args = append(args, filepath.Join(*dir, entry.Name()))
			}
		}
		return execute(ctx, "gh", args...)
	default:
		return fmt.Errorf("不支持的发布命令 %q", args[0])
	}
}

func execute(ctx context.Context, name string, args ...string) error {
	if name == "helm" && os.Getenv("HELM_BIN") != "" {
		name = os.Getenv("HELM_BIN")
	}
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "helm" && os.Getenv("HELM_BIN") != "" {
		name = os.Getenv("HELM_BIN")
	}
	c := exec.CommandContext(ctx, name, args...)
	data, err := c.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s 失败: %w: %s", name, err, data)
	}
	return data, nil
}

func cleanSource(ctx context.Context, source string) error {
	sha, err := output(ctx, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(sha)) != source {
		return fmt.Errorf("源提交与 HEAD 不匹配")
	}
	status, err := output(ctx, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(status)) != 0 {
		return fmt.Errorf("发布要求已提交的干净工作树（含未跟踪源文件）")
	}
	return nil
}

// inspectImage 同时读取 index 与每个平台的 OCI 配置，验证实际 revision 标签而非只抄入调用参数。
func inspectImage(ctx context.Context, tag, source string) (product.Image, error) {
	ref := product.Repository + ":" + tag
	data, err := output(ctx, "docker", "buildx", "imagetools", "inspect", ref, "--format", "{{json .Manifest}}")
	if err != nil {
		return product.Image{}, err
	}
	var index struct {
		Digest    string `json:"digest"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err = json.Unmarshal(data, &index); err != nil {
		return product.Image{}, err
	}
	image := product.Image{Repository: product.Repository, Tag: tag, SourceCommit: source, IndexDigest: index.Digest, Platforms: map[string]string{}}
	for _, manifest := range index.Manifests {
		p := manifest.Platform.OS + "/" + manifest.Platform.Architecture
		if p == "linux/amd64" || p == "linux/arm64" {
			image.Platforms[p] = manifest.Digest
		}
	}
	if len(image.Platforms) != 2 {
		return image, fmt.Errorf("发布镜像必须同时包含 amd64/arm64")
	}
	if source != "" {
		configs, err := output(ctx, "docker", "buildx", "imagetools", "inspect", product.Repository+"@"+index.Digest, "--format", "{{json .Image}}")
		if err != nil {
			return image, err
		}
		var entries map[string]struct {
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"config"`
		}
		if err = json.Unmarshal(configs, &entries); err != nil {
			return image, err
		}
		for platform := range image.Platforms {
			if entries[platform].Config.Labels["org.opencontainers.image.revision"] != source {
				return image, fmt.Errorf("%s 的实际 OCI revision 与源提交不匹配", platform)
			}
		}
	}
	return image, nil
}

func publishImages(ctx context.Context, source, version string) error {
	previous, cleanup, err := exportPrevious(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	// 分别发布当前源与冻结功能前源，逐个平台核对实际 OCI revision。
	for _, build := range []struct{ tag, sha, dir string }{{version, source, root}, {product.PreviousVersion, product.PreviousCommit, previous}} {
		ref := product.Repository + ":" + build.tag
		_, err := inspectImage(ctx, build.tag, build.sha)
		if err == nil {
			fmt.Println("复用同源不可变候选", ref)
			continue
		}
		if !strings.Contains(err.Error(), ref+": not found") && !strings.Contains(err.Error(), "manifest unknown") {
			return err
		}
		// 关键：已存在但来源不同的 tag 不覆盖；只有明确的 registry NotFound 才创建新候选。
		if err = execute(ctx, "docker", "buildx", "build", "--platform", "linux/amd64,linux/arm64", "--file", filepath.Join(root, "Dockerfile"), "--tag", ref, "--build-arg", "SOURCE_COMMIT="+build.sha, "--push", build.dir); err != nil {
			return err
		}
		if _, err = inspectImage(ctx, build.tag, build.sha); err != nil {
			return err
		}
	}
	return nil
}

// exportPrevious 固定导出 #86 前的 Git 对象，不包含主工作区的未提交文件或凭据。
func exportPrevious(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "mih-pre-sm-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	data, err := output(ctx, "git", "archive", product.PreviousCommit)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	t := tar.NewReader(bytes.NewReader(data))
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			cleanup()
			return "", func() {}, e
		}
		// Git archive 的全局 PAX 头仅携带 commit 元数据，不是待展开文件。
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		path := filepath.Join(dir, h.Name)
		if !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
			cleanup()
			return "", func() {}, fmt.Errorf("非法 Git archive 路径")
		}
		if h.Typeflag == tar.TypeDir {
			err = os.MkdirAll(path, 0755)
		} else if h.Typeflag == tar.TypeReg {
			err = os.MkdirAll(filepath.Dir(path), 0755)
			if err == nil {
				var b []byte
				b, err = io.ReadAll(t)
				if err == nil {
					err = os.WriteFile(path, b, os.FileMode(h.Mode))
				}
			}
		} else {
			err = fmt.Errorf("不支持 Git archive 链接")
		}
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	return dir, cleanup, nil
}

func packageBundle(ctx context.Context, dir, source, version string) error {
	if dir == "" {
		return fmt.Errorf("必须指定 bundle 输出目录")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// 准备并校验 registry 中三条真实镜像基线，历史 tag 漂移必须停止。
	image, err := inspectImage(ctx, version, source)
	if err != nil {
		return err
	}
	previousImage, err := inspectImage(ctx, product.PreviousVersion, product.PreviousCommit)
	if err != nil {
		return err
	}
	legacy, err := inspectImage(ctx, "0.1.0", "")
	if err != nil {
		return err
	}
	if legacy.IndexDigest != "sha256:c64fda8d39b6653f74ede950d8ce3e37a479e25c137fd78f956442d641f41d6a" {
		return fmt.Errorf("历史 0.1.0 tag 已变化，必须重新审查基线，不能静默替换")
	}
	previous, cleanup, err := exportPrevious(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	m := &product.Manifest{SchemaVersion: 1, Version: version, SourceCommit: source, TestedCommit: source, Image: image, Chart: "monitor-in-hand-" + version + ".tgz", CRD: "kubestatemetrics.yaml", RBAC: "manager-role.yaml", Contract: "servicemonitor-contract.json", Files: map[string]string{}, Legacy: product.Baseline{Version: "0.1.0", Chart: "monitor-in-hand-0.1.0.tgz", CRD: "legacy-kubestatemetrics.yaml", Image: legacy}, Previous: product.Baseline{Version: product.PreviousVersion, SourceCommit: product.PreviousCommit, Chart: "monitor-in-hand-" + product.PreviousVersion + ".tgz", CRD: "legacy-kubestatemetrics.yaml", Image: previousImage}}
	// 产物由同一受测源组装；只有 values 中的运行 digest 在 registry 返回真实值后写入临时 chart。
	if err = packageChart(ctx, "charts/monitor-in-hand", dir, version, image.IndexDigest); err != nil {
		return err
	}
	for _, v := range []string{"0.1.0", product.PreviousVersion} {
		if err = packageChart(ctx, filepath.Join(previous, "charts/monitor-in-hand"), dir, v, ""); err != nil {
			return err
		}
	}
	// 将独立 CRD、RBAC 和机器契约纳入同一来源记录，并逐文件计算摘要。
	for dest, src := range map[string]string{m.CRD: "config/crd/bases/monitoring.fjsxy.github.io_kubestatemetrics.yaml", m.RBAC: "config/rbac/role.yaml", m.Contract: "docs/contracts/servicemonitor-v1.json", m.Legacy.CRD: filepath.Join(previous, "config/crd/bases/monitoring.fjsxy.github.io_kubestatemetrics.yaml")} {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, dest), data, 0644); err != nil {
			return err
		}
	}
	for _, name := range []string{m.Chart, m.Previous.Chart, m.Legacy.Chart, m.CRD, m.RBAC, m.Contract, m.Legacy.CRD} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		m.Files[name] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err = os.WriteFile(filepath.Join(dir, "release.json"), data, 0644); err != nil {
		return err
	}
	if err = product.VerifyBundle(dir, source); err != nil {
		return err
	}
	// 根 manifest 也进入摘要清单；其不可变摘要通过下游交付评论单独锁定。
	names := []string{"release.json"}
	for name := range m.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(b), name)
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0644)
}

func packageChart(ctx context.Context, source, dir, version, digest string) error {
	tmp, err := os.MkdirTemp("", "mih-chart-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	chart := filepath.Join(tmp, "monitor-in-hand")
	// 只在临时目录组装 chart，registry 返回 digest 后再固定运行镜像。
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(chart, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0644)
	})
	if err != nil {
		return err
	}
	// 发布包默认选择 registry 已返回的不可变 digest，保留源 chart 的其他配置。
	if digest != "" {
		path := filepath.Join(chart, "values.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var values map[string]any
		if err = yaml.Unmarshal(data, &values); err != nil {
			return err
		}
		values["image"] = map[string]any{"digest": digest}
		data, err = yaml.Marshal(values)
		if err != nil {
			return err
		}
		if err = os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	// 由 Helm 实际生成包，版本与运行镜像版本均来自同一次发布输入。
	return execute(ctx, "helm", "package", chart, "--destination", dir, "--version", version, "--app-version", version)
}
