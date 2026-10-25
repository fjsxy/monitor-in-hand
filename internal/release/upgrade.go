package release

import (
	"context"
	"fmt"
	"path/filepath"
)

// Runner 是 helm/kubectl 进程边界，参数始终独立传递，不经过 shell 展开。
type Runner func(context.Context, string, ...string) error

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
	// 关键：CRD 写入失败就停止，不删除既有 CRD，不用 force 覆盖其他管理者的冲突。
	if err = run(ctx, "kubectl", "--context", kubeContext, "apply", "--server-side", "--field-manager=monitor-in-hand-release", "--filename", filepath.Join(dir, m.CRD)); err != nil {
		return fmt.Errorf("升级 CRD: %w", err)
	}
	return run(ctx, "helm", "upgrade", "--install", name, filepath.Join(dir, m.Chart), "--kube-context", kubeContext, "--namespace", namespace, "--create-namespace", "--skip-crds", "--wait", "--timeout", "180s")
}
