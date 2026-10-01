package kubestatemetrics_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubeStateMetricsE2EOrchestration(t *testing.T) {
	t.Run("UT-KSM-GH57-004 镜像准备不占用 Ginkgo setup 时间预算", func(t *testing.T) {
		// 通过公开 Make 入口读取实际编排，避免在测试中复制 shell 行为或调用 Docker。
		repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
		if err != nil {
			t.Fatalf("resolve repository root: %v", err)
		}
		command := exec.Command("make", "--dry-run", "e2e-kubestatemetrics",
			"KUBERNETES_VERSION=1.28",
			"TEST_RUN_ID=sc-57-contract",
			"HELM=helm",
			"GINKGO=ginkgo",
			"CONTAINER_TOOL=docker",
			"KIND=kind",
		)
		command.Dir = repoRoot
		command.Env = os.Environ()
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("dry-run public E2E target: %v\n%s", err, output)
		}

		// 镜像准备必须先用宿主机工具链生成 Linux 二进制，避免冷 Docker 构建等待编译器镜像元数据。
		plan := string(output)
		hostBuildAt := strings.Index(plan, "CGO_ENABLED=0 GOOS=linux go build")
		buildAt := strings.Index(plan, `docker build --file test/e2e/kubestatemetrics/Dockerfile --tag "docker.io/fjsxy/monitor-in-hand:0.1.0" bin/e2e-kubestatemetrics`)
		loadAt := strings.Index(plan, `kind load docker-image --name "mih-e2e-128-sc-57-contract" "docker.io/fjsxy/monitor-in-hand:0.1.0"`)
		ginkgoAt := strings.Index(plan, "ginkgo -v")
		if hostBuildAt < 0 || buildAt < 0 || loadAt < 0 || ginkgoAt < 0 {
			t.Fatalf("E2E 编排缺少镜像准备或 Ginkgo 阶段:\n%s", plan)
		}
		if !(hostBuildAt < buildAt && buildAt < loadAt && loadAt < ginkgoAt) {
			t.Fatalf("E2E 编排顺序错误: host-build=%d image-build=%d load=%d ginkgo=%d\n%s", hostBuildAt, buildAt, loadAt, ginkgoAt, plan)
		}

		// 读取公开编排引用的镜像定义，锁定 Docker 阶段不再依赖远端 Go 编译器镜像。
		dockerfile, err := os.ReadFile(filepath.Join(repoRoot, "test", "e2e", "kubestatemetrics", "Dockerfile"))
		if err != nil {
			t.Fatalf("read E2E runtime Dockerfile: %v", err)
		}
		if strings.Contains(string(dockerfile), "FROM golang:") || strings.Count(string(dockerfile), "FROM ") != 1 {
			t.Fatalf("E2E 镜像必须仅包含 runtime stage:\n%s", dockerfile)
		}
	})
}
