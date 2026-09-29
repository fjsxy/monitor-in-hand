package testenv_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	stableAcceptanceID = regexp.MustCompile(`(?:UT|IT)-[A-Z0-9]+-[0-9]{3}|HELM-[0-9]{3}|E2E-(?:119|128)-[A-Z0-9-]+-[0-9]{3}`)
	goTestFile         = regexp.MustCompile(`_test\.go$`)
)

var _ = Describe("一期验收矩阵可追溯性", func() {
	It("测试源码与矩阵中的稳定场景 ID 双向一致且各自唯一", func() {
		By("从所有 Go 测试源码收集会进入测试报告的稳定场景 ID")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		sourceIDs := collectStableIDs(root)

		By("从版本化一期验收矩阵收集已承诺的稳定场景 ID")
		matrixPath := filepath.Join(root, "docs", "testing", "phase1-acceptance-matrix.md")
		matrix, err := os.ReadFile(matrixPath)
		Expect(err).NotTo(HaveOccurred())
		matrixIDs := occurrences(stableAcceptanceID.FindAllString(string(matrix), -1), matrixPath)

		By("验证任一侧都没有重复 ID，且测试与矩阵不存在漏登或悬空条目")
		Expect(duplicates(sourceIDs)).To(BeEmpty(), "测试源码中的稳定场景 ID 必须唯一")
		Expect(duplicates(matrixIDs)).To(BeEmpty(), "验收矩阵中的稳定场景 ID 必须唯一")
		Expect(missingIDs(sourceIDs, matrixIDs)).To(BeEmpty(), "以下测试场景尚未登记到一期验收矩阵")
		Expect(missingIDs(matrixIDs, sourceIDs)).To(BeEmpty(), "以下矩阵场景没有对应的 Go 测试")
	})
})

func collectStableIDs(root string) map[string][]string {
	result := map[string][]string{}
	for _, directory := range []string{"internal", "test"} {
		base := filepath.Join(root, directory)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !goTestFile.MatchString(entry.Name()) {
				return nil
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, id := range stableAcceptanceID.FindAllString(string(contents), -1) {
				result[id] = append(result[id], path)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	return result
}

func occurrences(ids []string, source string) map[string][]string {
	result := map[string][]string{}
	for _, id := range ids {
		result[id] = append(result[id], source)
	}
	return result
}

func duplicates(ids map[string][]string) []string {
	result := make([]string, 0)
	for id, sources := range ids {
		if len(sources) > 1 {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

func missingIDs(want, got map[string][]string) []string {
	result := make([]string, 0)
	for id := range want {
		if len(got[id]) == 0 {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}
