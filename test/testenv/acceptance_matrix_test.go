package testenv_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	stableAcceptanceID    = regexp.MustCompile(`(?:UT|IT)-[A-Z0-9]+-(?:GH[1-9][0-9]*-)?[0-9]{3}|HELM-(?:GH[1-9][0-9]*-)?[0-9]{3}|E2E-(?:119|128)-[A-Z0-9]+-(?:GH[1-9][0-9]*-)?[0-9]{3}`)
	issueScopedAcceptance = regexp.MustCompile(`^(?:(?:UT|IT)-[A-Z0-9]+|HELM|E2E-(?:119|128)-[A-Z0-9-]+)-GH([1-9][0-9]*)-[0-9]{3}$`)
	issueReference        = regexp.MustCompile(`#([1-9][0-9]*)`)
	goTestFile            = regexp.MustCompile(`_test\.go$`)
)

var _ = Describe("一期验收矩阵可追溯性", func() {
	DescribeTable("识别由 GitHub Issue 独占分配的稳定场景 ID",
		func(id string, wantIssue int) {
			issue, ok := acceptanceIssue(id)
			Expect(ok).To(BeTrue())
			Expect(issue).To(Equal(wantIssue))
			Expect(stableAcceptanceID.FindString(id)).To(Equal(id))
		},
		Entry("unit", "UT-KSM-GH"+"53-001", 53),
		Entry("integration", "IT-KSM-GH"+"56-002", 56),
		Entry("helm", "HELM-GH"+"59-001", 59),
		Entry("e2e", "E2E-119-KSM-GH"+"53-003", 53),
	)

	It("拒绝 Issue-scoped ID 与验收矩阵来源不一致", func() {
		matrix := "| `UT-KSM-GH" + "53-001` | #56 | 纯规划 |"
		Expect(issueOwnershipMismatches(matrix)).To(Equal([]string{"UT-KSM-GH" + "53-001"}))
	})

	It("拒绝历史清单之外的全局递增 ID", func() {
		found := occurrences([]string{"UT-KSM-" + "999", "UT-KSM-GH" + "53-001"}, "fixture")
		allowedLegacy := map[string]struct{}{"UT-KSM-" + "001": {}}
		Expect(unscopedAcceptanceIDs(found, allowedLegacy)).To(Equal([]string{"UT-KSM-" + "999"}))
	})

	const e2eID = "E2E-119-KSM-GH" + "53-001"
	const pairedE2EID = "E2E-128-KSM-GH" + "53-001"
	const e2eHeader = "| 场景 ID | 来源 | 验收目的 | 执行步骤 | 执行预期 |\n| --- | --- | --- | --- | --- |\n"
	DescribeTable("检查 E2E 验收条目的三个必填部分",
		func(matrix string, want []string) {
			Expect(missingE2EAcceptanceDetails(matrix)).To(Equal(want))
		},
		Entry("完整条目通过", e2eHeader+"| `"+e2eID+"` | #53 | 验证隔离 | 创建两个实例 | 资源互不影响 |", []string(nil)),
		Entry("验收目的为空", e2eHeader+"| `"+e2eID+"` | #53 | | 创建两个实例 | 资源互不影响 |", []string{e2eID + " 缺少验收目的"}),
		Entry("执行步骤仅含空白", e2eHeader+"| `"+e2eID+"` | #53 | 验证隔离 | \t | 资源互不影响 |", []string{e2eID + " 缺少执行步骤"}),
		Entry("执行预期为空", e2eHeader+"| `"+e2eID+"` | #53 | 验证隔离 | 创建两个实例 | |", []string{e2eID + " 缺少执行预期"}),
		Entry("缺少必填列", "| 场景 ID | 来源 | 验收目的 | 执行预期 |\n| `"+e2eID+"` | #53 | 验证隔离 | 资源互不影响 |", []string{e2eID + " 缺少执行步骤"}),
		Entry("截断条目不能沿用表头内容", e2eHeader+"| `"+e2eID+"` | #53 | 验证隔离 | 创建两个实例 |", []string{e2eID + " 缺少执行预期"}),
		Entry("共用一行的两个版本均须完整", e2eHeader+"| `"+e2eID+"` / `"+pairedE2EID+"` | #53 | 验证隔离 | | 资源互不影响 |", []string{e2eID + " 缺少执行步骤", pairedE2EID + " 缺少执行步骤"}),
		Entry("按列名识别重排后的字段", "| 场景 ID | 执行预期 | 执行步骤 | 验收目的 | 来源 |\n| `"+e2eID+"` | 资源互不影响 | 创建两个实例 | 验证隔离 | #53 |", []string(nil)),
		Entry("不继承上一张表的表头", e2eHeader+"\n## E2E\n| `"+e2eID+"` | #53 | 验证隔离 | 创建两个实例 | 资源互不影响 |", []string{e2eID + " 缺少验收目的", e2eID + " 缺少执行步骤", e2eID + " 缺少执行预期"}),
		Entry("其他测试层级保留原有字段", "| 场景 ID | 来源 | 动作与精确断言 |\n| `UT-KSM-GH"+"53-001` | #53 | 返回计划 |", []string(nil)),
	)

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
		legacyIDs, err := loadAcceptanceIDs(filepath.Join(root, "test", "testenv", "testdata", "legacy-acceptance-ids.txt"))
		Expect(err).NotTo(HaveOccurred())

		By("验证任一侧都没有重复 ID，且测试与矩阵不存在漏登或悬空条目")
		Expect(issueOwnershipMismatches(string(matrix))).To(BeEmpty(), "Issue-scoped ID 必须归属验收矩阵来源中的同一 Issue")
		Expect(unscopedAcceptanceIDs(sourceIDs, legacyIDs)).To(BeEmpty(), "新增测试场景必须使用 Issue-scoped ID")
		Expect(unscopedAcceptanceIDs(matrixIDs, legacyIDs)).To(BeEmpty(), "新增矩阵场景必须使用 Issue-scoped ID")
		Expect(duplicates(sourceIDs)).To(BeEmpty(), "测试源码中的稳定场景 ID 必须唯一")
		Expect(duplicates(matrixIDs)).To(BeEmpty(), "验收矩阵中的稳定场景 ID 必须唯一")
		Expect(missingIDs(sourceIDs, matrixIDs)).To(BeEmpty(), "以下测试场景尚未登记到一期验收矩阵")
		Expect(missingIDs(matrixIDs, sourceIDs)).To(BeEmpty(), "以下矩阵场景没有对应的 Go 测试")

		By("验证每个 E2E case 都明确验收目的、执行步骤和执行预期")
		Expect(missingE2EAcceptanceDetails(string(matrix))).To(BeEmpty(), "E2E 验收条目的三个部分必须分别填写且不能为空")
	})
})

func missingE2EAcceptanceDetails(matrix string) []string {
	var result []string
	var columns map[string]int
	for _, line := range strings.Split(matrix, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			columns = nil
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if strings.TrimSpace(cells[0]) == "场景 ID" {
			// 按当前表的列名定位必填部分，避免重排字段后校验错列。
			columns = map[string]int{}
			for index, cell := range cells {
				columns[strings.TrimSpace(cell)] = index
			}
			continue
		}

		// 同一行可以登记两个版本；缺列、短行和空白内容都逐个归属到稳定 ID。
		for _, id := range stableAcceptanceID.FindAllString(cells[0], -1) {
			if !strings.HasPrefix(id, "E2E-") {
				continue
			}
			for _, field := range []string{"验收目的", "执行步骤", "执行预期"} {
				index, found := columns[field]
				if !found || index >= len(cells) || strings.TrimSpace(cells[index]) == "" {
					result = append(result, id+" 缺少"+field)
				}
			}
		}
	}
	return result
}

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

func acceptanceIssue(id string) (int, bool) {
	matches := issueScopedAcceptance.FindStringSubmatch(id)
	if len(matches) != 2 {
		return 0, false
	}
	issue, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	return issue, true
}

func loadAcceptanceIDs(path string) (map[string]struct{}, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	result := map[string]struct{}{}
	for _, id := range strings.Fields(string(contents)) {
		result[id] = struct{}{}
	}
	return result, nil
}

func unscopedAcceptanceIDs(ids map[string][]string, allowedLegacy map[string]struct{}) []string {
	var result []string
	for id := range ids {
		if _, scoped := acceptanceIssue(id); scoped {
			continue
		}
		if _, allowed := allowedLegacy[id]; !allowed {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

func issueOwnershipMismatches(matrix string) []string {
	var result []string
	for _, line := range strings.Split(matrix, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}

		sourceIssues := map[int]struct{}{}
		for _, match := range issueReference.FindAllStringSubmatch(cells[2], -1) {
			issue, err := strconv.Atoi(match[1])
			if err == nil {
				sourceIssues[issue] = struct{}{}
			}
		}
		for _, id := range stableAcceptanceID.FindAllString(cells[1], -1) {
			issue, scoped := acceptanceIssue(id)
			if _, found := sourceIssues[issue]; scoped && !found {
				result = append(result, id)
			}
		}
	}
	sort.Strings(result)
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
