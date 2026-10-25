package kubestatemetricssupport_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("E2E orchestration contracts", func() {
	It("[UT-KSM-GH57-004] 镜像准备不占用 Ginkgo setup 时间预算", func() {
		By("从公开 Make 入口读取实际 E2E 编排")
		repoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
		Expect(err).NotTo(HaveOccurred())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "make", "--dry-run", "e2e-kubestatemetrics",
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
		Expect(err).NotTo(HaveOccurred(), string(output))

		By("验证宿主构建、runtime 镜像、kind 导入与 Ginkgo 的严格顺序")
		chartData, err := os.ReadFile(filepath.Join(repoRoot, "charts/monitor-in-hand/Chart.yaml"))
		Expect(err).NotTo(HaveOccurred())
		chart := map[string]any{}
		Expect(yaml.Unmarshal(chartData, &chart)).To(Succeed())
		image := "docker.io/fjsxy/monitor-in-hand:" + chart["appVersion"].(string)
		plan := string(output)
		hostBuildAt := strings.Index(plan, "CGO_ENABLED=0 GOOS=linux go build")
		buildAt := strings.Index(plan, fmt.Sprintf(`docker build --file test/e2e/kubestatemetrics/Dockerfile --tag "%s" bin/e2e-kubestatemetrics`, image))
		loadAt := strings.Index(plan, fmt.Sprintf(`kind load docker-image --name "mih-e2e-128-sc-57-contract" "%s"`, image))
		ginkgoAt := strings.Index(plan, "ginkgo -v")
		Expect(hostBuildAt).To(BeNumerically(">=", 0), plan)
		Expect(buildAt).To(BeNumerically(">=", 0), plan)
		Expect(loadAt).To(BeNumerically(">=", 0), plan)
		Expect(ginkgoAt).To(BeNumerically(">=", 0), plan)
		Expect(hostBuildAt < buildAt && buildAt < loadAt && loadAt < ginkgoAt).To(BeTrue(), plan)

		By("验证 E2E 镜像定义只有不依赖 Go toolchain 的 runtime stage")
		dockerfile, err := os.ReadFile(filepath.Join(repoRoot, "test", "e2e", "kubestatemetrics", "Dockerfile"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(dockerfile)).NotTo(ContainSubstring("FROM golang:"))
		Expect(strings.Count(string(dockerfile), "FROM ")).To(Equal(1), string(dockerfile))
	})
})
