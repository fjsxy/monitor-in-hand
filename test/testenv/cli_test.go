package testenv_test

import (
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("测试依赖基线公共入口", func() {
	It("拒绝一期范围外的 Kubernetes minor", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-up",
			"KUBERNETES_VERSION=1.27",
			"TEST_RUN_ID=cli-contract",
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("KUBERNETES_VERSION 必须是 1.19 或 1.28"))
	})

	It("拒绝缺失的 test-run 身份", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-up",
			"KUBERNETES_VERSION=1.28",
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("TEST_RUN_ID 必须是非空的 DNS label"))
	})

	It("身份标记不匹配时拒绝删除集群", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		deleted := filepath.Join(fakeBin, "deleted")

		writeExecutable(filepath.Join(fakeBin, "kind"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == "get clusters" ]]; then
  echo "mih-e2e-128-cli-contract"
  exit 0
fi
if [[ "${1:-}" == "delete" ]]; then
  touch "${TESTENV_DELETE_SENTINEL}"
  exit 0
fi
exit 1
`)
		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
printf 'another-run'
`)

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-down",
			"KUBERNETES_VERSION=1.28",
			"TEST_RUN_ID=cli-contract",
		)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"TESTENV_DELETE_SENTINEL="+deleted,
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("test-run 身份不匹配，拒绝清理"))
		Expect(deleted).NotTo(BeAnExistingFile())
	})

	It("拒绝复用同名 kind 集群", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		created := filepath.Join(fakeBin, "created")

		writeExecutable(filepath.Join(fakeBin, "kind"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == "get clusters" ]]; then
  echo "mih-e2e-119-cli-contract"
  exit 0
fi
if [[ "${1:-}" == "create" ]]; then
  touch "${TESTENV_CREATE_SENTINEL}"
  exit 0
fi
exit 1
`)

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-up",
			"KUBERNETES_VERSION=1.19",
			"TEST_RUN_ID=cli-contract",
		)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"TESTENV_CREATE_SENTINEL="+created,
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("拒绝复用已有 kind 集群"))
		Expect(created).NotTo(BeAnExistingFile())
	})
})

func writeExecutable(path, contents string) {
	Expect(os.WriteFile(path, []byte(contents), 0o755)).To(Succeed())
}
