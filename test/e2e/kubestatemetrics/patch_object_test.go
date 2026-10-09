package kubestatemetrics_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

func TestPatchObjectReturnsPatchedServerRepresentation(t *testing.T) {
	t.Run("UT-KSM-GH81-002", func(t *testing.T) {
		// 准备一个只暴露 kubectl 公共命令契约的 fixture，返回 PATCH 当次的服务端对象。
		temporaryDirectory := t.TempDir()
		argumentsPath := filepath.Join(temporaryDirectory, "arguments")
		kubectlFixture := filepath.Join(temporaryDirectory, "kubectl")
		fixture := `#!/bin/sh
printf '%s\n' "$@" > "$KUBECTL_ARGUMENTS_PATH"
printf '%s' '{"metadata":{"generation":7},"spec":{"template":{"spec":{"containers":[{"args":["--resources=pods","--namespaces=monitor-in-hand-test"]}]}}}}'
`
		if err := os.WriteFile(kubectlFixture, []byte(fixture), 0o700); err != nil {
			t.Fatalf("写入 kubectl fixture: %v", err)
		}
		t.Setenv("KUBECTL_ARGUMENTS_PATH", argumentsPath)

		previousKubectlBin, previousKubeContext := kubectlBin, kubeContext
		kubectlBin, kubeContext = kubectlFixture, "fixture-context"
		t.Cleanup(func() {
			kubectlBin, kubeContext = previousKubectlBin, previousKubeContext
		})

		// 执行 PATCH 并直接读取同一次响应，模拟 owner watch 可能立即恢复对象的竞态窗口。
		patch := []byte(`[{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--resources=pods"]}]`)
		deployment, err := patchObject[appsv1.Deployment](context.Background(), targetNamespace, "deployment", convergenceWorkload, patch)
		if err != nil {
			t.Fatalf("patchObject 返回错误: %v", err)
		}
		if deployment.Generation != 7 {
			t.Fatalf("generation=%d, want 7", deployment.Generation)
		}
		actualArgs := deployment.Spec.Template.Spec.Containers[0].Args
		if len(actualArgs) != 2 || actualArgs[0] != "--resources=pods" {
			t.Fatalf("patched args=%v", actualArgs)
		}

		// 验证测试确实通过公开 kubectl PATCH 接口请求 JSON，而没有依赖后续 GET 观察瞬时漂移。
		arguments, err := os.ReadFile(argumentsPath)
		if err != nil {
			t.Fatalf("读取 kubectl 参数: %v", err)
		}
		invocation := strings.Split(strings.TrimSpace(string(arguments)), "\n")
		wantArguments := []string{
			"--context", "fixture-context", "--namespace", targetNamespace,
			"patch", "deployment/" + convergenceWorkload, "--type=json", "--patch", string(patch), "--output=json",
		}
		if strings.Join(invocation, "\x00") != strings.Join(wantArguments, "\x00") {
			t.Fatalf("kubectl args=%q, want %q", invocation, wantArguments)
		}
	})
}
