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

		// 镜像构建和导入必须先于 Ginkgo，冷构建才能脱离 suite/node timeout。
		plan := string(output)
		buildAt := strings.Index(plan, `docker build --tag "docker.io/fjsxy/monitor-in-hand:0.1.0"`)
		loadAt := strings.Index(plan, `kind load docker-image --name "mih-e2e-128-sc-57-contract" "docker.io/fjsxy/monitor-in-hand:0.1.0"`)
		ginkgoAt := strings.Index(plan, "ginkgo -v")
		if buildAt < 0 || loadAt < 0 || ginkgoAt < 0 {
			t.Fatalf("E2E 编排缺少镜像准备或 Ginkgo 阶段:\n%s", plan)
		}
		if !(buildAt < loadAt && loadAt < ginkgoAt) {
			t.Fatalf("E2E 编排顺序错误: build=%d load=%d ginkgo=%d\n%s", buildAt, loadAt, ginkgoAt, plan)
		}
	})
}
