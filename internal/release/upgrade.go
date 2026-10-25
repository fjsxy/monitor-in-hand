package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// Runner 是 helm/kubectl 进程边界，参数始终独立传递，不经过 shell 展开。
type Runner func(context.Context, string, ...string) ([]byte, error)

// Upgrade 保持 CRD 身份，先显式升级 schema，再升级同一发布包的 Operator。
func Upgrade(ctx context.Context, dir, source, kubeContext, namespace, name string, run Runner) error {
	if kubeContext == "" || namespace == "" || name == "" {
		return fmt.Errorf("必须显式指定 context、namespace 和 Helm release")
	}
	if err := VerifyBundle(dir, source); err != nil {
		return err
	}
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	// 关键：只有已知基线才更新，任何 CRD 冲突都在 Helm 前停止，不删除或 force 接管。
	if err = upgradeCRD(ctx, dir, m, kubeContext, run); err != nil {
		return fmt.Errorf("升级 CRD: %w", err)
	}
	_, err = run(ctx, "helm", "upgrade", "--install", name, filepath.Join(dir, m.Chart), "--kube-context", kubeContext, "--namespace", namespace, "--create-namespace", "--skip-crds", "--wait", "--timeout", "180s")
	return err
}

func upgradeCRD(ctx context.Context, dir string, m *Manifest, kubeContext string, run Runner) error {
	// 读取当前对象，完整核对 spec 基线，拒绝未纳入候选的 schema 或转换 webhook。
	data, err := os.ReadFile(filepath.Join(dir, m.CRD))
	if err != nil {
		return err
	}
	desired := &extv1.CustomResourceDefinition{}
	if err = yaml.Unmarshal(data, desired); err != nil {
		return err
	}
	prefix := []string{"--context", kubeContext}
	currentData, err := run(ctx, "kubectl", append(prefix, "get", "crd", desired.Name, "--ignore-not-found", "--output=json")...)
	if err != nil {
		return err
	}
	if len(currentData) == 0 {
		_, err = run(ctx, "kubectl", append(prefix, "create", "--filename", filepath.Join(dir, m.CRD))...)
		return err
	}
	current := &extv1.CustomResourceDefinition{}
	if err = json.Unmarshal(currentData, current); err != nil {
		return err
	}
	if current.UID == "" || current.ResourceVersion == "" || current.Name != desired.Name {
		return fmt.Errorf("CRD 身份不完整")
	}
	if sameSpec(current.Spec, desired.Spec) {
		return nil
	}
	oldData, err := os.ReadFile(filepath.Join(dir, m.Legacy.CRD))
	if err != nil {
		return err
	}
	old := &extv1.CustomResourceDefinition{}
	if err = yaml.Unmarshal(oldData, old); err != nil {
		return err
	}
	if !sameSpec(current.Spec, old.Spec) {
		return fmt.Errorf("现有 CRD 不匹配已知旧/新 spec，停止升级，请检查漂移")
	}
	// 原位替换 spec，保留 metadata/status；API 原子验证 UID/RV，避免覆盖读取后的并发变化。
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": current.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": current.ResourceVersion}, {"op": "replace", "path": "/spec", "value": desired.Spec}}
	payload, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "mih-crd-upgrade-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(payload); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	_, err = run(ctx, "kubectl", append(prefix, "patch", "crd", desired.Name, "--type=json", "--field-manager=monitor-in-hand-release", "--patch-file", f.Name())...)
	return err
}

// sameSpec 仅归一化 API 默认的 None conversion；其他 spec 变化必须匹配冻结基线。
func sameSpec(a, b extv1.CustomResourceDefinitionSpec) bool {
	normalize := func(s extv1.CustomResourceDefinitionSpec) extv1.CustomResourceDefinitionSpec {
		if s.Conversion != nil && s.Conversion.Strategy == extv1.NoneConverter && s.Conversion.Webhook == nil {
			s.Conversion = nil
		}
		return s
	}
	canonical := func(spec extv1.CustomResourceDefinitionSpec) any {
		data, err := json.Marshal(normalize(spec))
		if err != nil {
			return nil
		}
		var value any
		if json.Unmarshal(data, &value) != nil {
			return nil
		}
		return value
	}
	left, right := canonical(a), canonical(b)
	return left != nil && right != nil && reflect.DeepEqual(left, right)
}
