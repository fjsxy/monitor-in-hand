package phase1gate

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var blockingID = regexp.MustCompile(`(?:UT|IT)-[A-Z0-9]+(?:-[A-Z0-9]+)*-(?:GH[1-9][0-9]*-)?[0-9]{3}|HELM-(?:GH[1-9][0-9]*-)?[0-9]{3}|E2E-(?:119|128)-[A-Z0-9]+(?:-[A-Z0-9]+)*-(?:GH[1-9][0-9]*-)?[0-9]{3}`)
var blockingE2EID = regexp.MustCompile(`E2E-(?:119|128)-[A-Z0-9]+(?:-[A-Z0-9]+)*-(?:GH[1-9][0-9]*-)?[0-9]{3}`)
var blockingE2EIDParts = regexp.MustCompile(`^E2E-(119|128)-([A-Z0-9]+)(?:-[A-Z0-9]+)*-(?:GH[1-9][0-9]*-)?[0-9]{3}$`)

type JUnitSummary struct {
	Passed      int
	ReportFiles int
}

type CoverageSummary struct {
	TotalPercent   float64
	PackagePercent map[string]float64
}

// BlockingIDs returns every stable identity committed by the acceptance matrix.
func BlockingIDs(matrix []byte) ([]string, error) {
	occurrences := map[string]int{}
	for _, id := range blockingID.FindAllString(string(matrix), -1) {
		occurrences[id]++
	}
	if len(occurrences) == 0 {
		return nil, errors.New("验收矩阵没有 blocking ID")
	}

	ids := make([]string, 0, len(occurrences))
	var problems []string
	for id, count := range occurrences {
		ids = append(ids, id)
		if count != 1 {
			problems = append(problems, fmt.Sprintf("%s 在验收矩阵中重复 %d 次", id, count))
		}
	}
	sort.Strings(ids)
	sort.Strings(problems)
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "；"))
	}
	return ids, nil
}

// BlockingE2EIDs returns the versioned E2E subset committed by the acceptance matrix.
func BlockingE2EIDs(matrix []byte) ([]string, error) {
	ids, err := BlockingIDs(matrix)
	if err != nil {
		return nil, err
	}
	var e2eIDs []string
	for _, id := range ids {
		if blockingE2EID.MatchString(id) && blockingE2EID.FindString(id) == id {
			e2eIDs = append(e2eIDs, id)
		}
	}
	if len(e2eIDs) == 0 {
		return nil, errors.New("验收矩阵没有 blocking E2E ID")
	}
	return e2eIDs, nil
}

type junitOutcome string

const (
	junitPassed  junitOutcome = "通过"
	junitFailed  junitOutcome = "失败"
	junitError   junitOutcome = "错误"
	junitSkipped junitOutcome = "被跳过"
)

type junitTestCase struct {
	Name      string       `xml:"name,attr"`
	Classname string       `xml:"classname,attr"`
	Failure   *struct{}    `xml:"failure"`
	Error     *struct{}    `xml:"error"`
	Skipped   *struct{}    `xml:"skipped"`
	Status    junitOutcome `xml:"-"`
}

type junitResult struct {
	Outcome junitOutcome
	Path    string
}

// VerifyJUnitResults proves every matrix identity ran successfully; versioned E2E results must also be unique.
func VerifyJUnitResults(expected []string, reportsRoot string) (JUnitSummary, error) {
	expectedSet := map[string]struct{}{}
	for _, id := range expected {
		if !blockingID.MatchString(id) || blockingID.FindString(id) != id {
			return JUnitSummary{}, fmt.Errorf("非法 blocking ID %q", id)
		}
		if _, duplicate := expectedSet[id]; duplicate {
			return JUnitSummary{}, fmt.Errorf("blocking ID %s 重复", id)
		}
		expectedSet[id] = struct{}{}
	}
	if len(expectedSet) == 0 {
		return JUnitSummary{}, errors.New("没有待验证的 blocking ID")
	}

	// 只读取 Ginkgo 的单 suite junit.xml 或多 suite *_junit.xml，避免把其他 XML 当成测试结果。
	results := map[string][]junitResult{}
	summary := JUnitSummary{}
	err := filepath.WalkDir(reportsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (entry.Name() != "junit.xml" && !strings.HasSuffix(entry.Name(), "_junit.xml")) {
			return nil
		}
		testCases, err := readJUnit(path)
		if err != nil {
			return err
		}
		summary.ReportFiles++
		for _, testCase := range testCases {
			seenInTestCase := map[string]struct{}{}
			for _, id := range blockingID.FindAllString(testCase.Name+" "+testCase.Classname, -1) {
				if _, duplicate := seenInTestCase[id]; duplicate {
					continue
				}
				seenInTestCase[id] = struct{}{}
				// 矩阵外 deferred/optional 诊断不参与一期通过率，也不能替代任何 blocking ID。
				if _, blocking := expectedSet[id]; blocking {
					results[id] = append(results[id], junitResult{Outcome: testCase.Status, Path: path})
				}
			}
		}
		return nil
	})
	if err != nil {
		return JUnitSummary{}, fmt.Errorf("读取 JUnit: %w", err)
	}

	// 缺失、非成功、E2E 重复与矩阵外结果全部汇总，确保一次失败给出完整诊断。
	var problems []string
	for _, id := range expected {
		outcomes := results[id]
		if len(outcomes) == 0 {
			problems = append(problems, id+" 缺失")
			continue
		}
		isE2E := blockingE2EID.MatchString(id) && blockingE2EID.FindString(id) == id
		if isE2E && len(outcomes) != 1 {
			problems = append(problems, fmt.Sprintf("%s 出现 %d 次", id, len(outcomes)))
			continue
		}
		valid := true
		for _, outcome := range outcomes {
			if provenanceProblem := validateJUnitProvenance(id, outcome.Path); provenanceProblem != "" {
				problems = append(problems, id+" "+provenanceProblem)
				valid = false
			}
			if outcome.Outcome != junitPassed {
				problems = append(problems, id+" "+string(outcome.Outcome))
				valid = false
			}
		}
		if valid {
			summary.Passed++
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return summary, errors.New(strings.Join(problems, "；"))
	}
	return summary, nil
}

func validateJUnitProvenance(id, path string) string {
	normalized := "/" + strings.TrimPrefix(filepath.ToSlash(path), "/")
	switch {
	case strings.HasPrefix(id, "UT-"):
		if !strings.Contains(normalized, "/quality/") {
			return fmt.Sprintf("JUnit 来源不匹配，期望 quality artifact，实际 %s", path)
		}
		return ""
	case strings.HasPrefix(id, "IT-"):
		if !strings.Contains(normalized, "/quality/integration/") {
			return fmt.Sprintf("JUnit 来源不匹配，期望 quality/integration，实际 %s", path)
		}
		return ""
	case strings.HasPrefix(id, "HELM-"):
		if !strings.Contains(normalized, "/quality/helm/") {
			return fmt.Sprintf("JUnit 来源不匹配，期望 quality/helm，实际 %s", path)
		}
		return ""
	}
	parts := blockingE2EIDParts.FindStringSubmatch(id)
	if len(parts) != 3 {
		return "无法解析版本/lane"
	}
	version := map[string]string{"119": "1.19", "128": "1.28"}[parts[1]]
	lane := map[string]string{
		"KSM":        "ksm",
		"BOUNDARY":   "ksm",
		"PREFLIGHT":  "agent",
		"COLLECTION": "agent",
	}[parts[2]]
	if lane == "" {
		return "没有声明所属 lane"
	}
	wantRunIdentity := "-" + parts[1] + "-" + lane + "-" + version
	if !strings.Contains(normalized, wantRunIdentity+"/") {
		return fmt.Sprintf("JUnit 来源不匹配，期望 %s lane 的 Kubernetes %s，实际 %s", lane, version, path)
	}
	return ""
}

func readJUnit(path string) ([]junitTestCase, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := xml.NewDecoder(file)
	var result []junitTestCase
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 %s: %w", path, err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "testcase" {
			continue
		}
		var testCase junitTestCase
		if err := decoder.DecodeElement(&testCase, &start); err != nil {
			return nil, fmt.Errorf("解析 %s testcase: %w", path, err)
		}
		switch {
		case testCase.Error != nil:
			testCase.Status = junitError
		case testCase.Failure != nil:
			testCase.Status = junitFailed
		case testCase.Skipped != nil:
			testCase.Status = junitSkipped
		default:
			testCase.Status = junitPassed
		}
		result = append(result, testCase)
	}
	return result, nil
}

type coverageBlock struct {
	Package    string
	Statements int
	Covered    bool
}

// VerifyCoverage applies the phase-one aggregate and per-core-package statement thresholds.
func VerifyCoverage(profile []byte, aggregateMinimum, packageMinimum float64) (CoverageSummary, error) {
	// 先解析并按 block 身份合并多个测试二进制的 profile，避免重复分母或丢失任一覆盖命中。
	blocks := map[string]coverageBlock{}
	lines := strings.Split(strings.TrimSpace(string(profile)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "mode: ") {
		return CoverageSummary{}, errors.New("coverage profile 缺少 mode header")
	}
	for lineNumber, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return CoverageSummary{}, fmt.Errorf("coverage profile 第 %d 行格式错误", lineNumber+2)
		}
		statements, err := strconv.Atoi(fields[1])
		if err != nil || statements < 0 {
			return CoverageSummary{}, fmt.Errorf("coverage profile 第 %d 行 statement 数无效", lineNumber+2)
		}
		count, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return CoverageSummary{}, fmt.Errorf("coverage profile 第 %d 行 count 无效", lineNumber+2)
		}
		file := fields[0]
		colon := strings.LastIndex(file, ":")
		if colon < 0 {
			return CoverageSummary{}, fmt.Errorf("coverage profile 第 %d 行路径无效", lineNumber+2)
		}
		slash := strings.LastIndex(file[:colon], "/")
		if slash < 0 {
			return CoverageSummary{}, fmt.Errorf("coverage profile 第 %d 行路径无效", lineNumber+2)
		}
		key := fields[0] + " " + fields[1]
		block := coverageBlock{
			Package:    file[:slash],
			Statements: statements,
			Covered:    count > 0,
		}
		if existing, found := blocks[key]; found {
			if existing.Package != block.Package || existing.Statements != block.Statements {
				return CoverageSummary{}, fmt.Errorf("coverage block %s 定义不一致", fields[0])
			}
			block.Covered = block.Covered || existing.Covered
		}
		blocks[key] = block
	}
	if len(blocks) == 0 {
		return CoverageSummary{}, errors.New("coverage profile 没有 statement")
	}

	type totals struct{ covered, statements int }
	// 再从唯一 block 聚合仓库总体与逐核心包的原始 statement 计数。
	byPackage := map[string]totals{}
	all := totals{}
	for _, block := range blocks {
		current := byPackage[block.Package]
		current.statements += block.Statements
		all.statements += block.Statements
		if block.Covered {
			current.covered += block.Statements
			all.covered += block.Statements
		}
		byPackage[block.Package] = current
	}

	summary := CoverageSummary{PackagePercent: map[string]float64{}}
	summary.TotalPercent = percentage(all.covered, all.statements)
	// 最后使用原始整数计数严格比较阈值；格式化百分比只用于诊断展示。
	var problems []string
	if belowCoverageMinimum(all.covered, all.statements, aggregateMinimum) {
		problems = append(problems, fmt.Sprintf("总体 statement coverage %.3f%% 低于 %.1f%%", summary.TotalPercent, aggregateMinimum))
	}
	for packageName, packageTotals := range byPackage {
		value := percentage(packageTotals.covered, packageTotals.statements)
		summary.PackagePercent[packageName] = value
		if belowCoverageMinimum(packageTotals.covered, packageTotals.statements, packageMinimum) {
			problems = append(problems, fmt.Sprintf("%s statement coverage %.3f%% 低于 %.1f%%", packageName, value, packageMinimum))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return summary, errors.New(strings.Join(problems, "；"))
	}
	return summary, nil
}

func belowCoverageMinimum(covered, total int, minimum float64) bool {
	return float64(covered)*100 < minimum*float64(total)
}

func percentage(covered, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(covered) * 100 / float64(total)
}
