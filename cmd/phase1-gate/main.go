package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fjsxy/monitor-in-hand/internal/phase1gate"
)

func main() {
	matrixPath := flag.String("matrix", "docs/testing/phase1-acceptance-matrix.md", "一期验收矩阵路径")
	reportsPath := flag.String("reports", "", "本次 blocking JUnit 根目录")
	expectedBlockingCount := flag.Int("expected-blocking-count", 0, "预期的一期 blocking ID 总数；0 表示只以矩阵为准")
	expectedE2ECount := flag.Int("expected-e2e-count", 0, "预期的一期 blocking E2E ID 数量；0 表示只以矩阵为准")
	coveragePath := flag.String("coverage-profile", "", "unit statement coverage profile 路径")
	aggregateMinimum := flag.Float64("aggregate-coverage", 80, "unit 聚合 statement coverage 下限")
	packageMinimum := flag.Float64("package-coverage", 70, "核心包 statement coverage 下限")
	flag.Parse()

	if *reportsPath == "" && *coveragePath == "" {
		fail("至少指定 --reports 或 --coverage-profile")
	}

	if *coveragePath != "" {
		profile, err := os.ReadFile(*coveragePath)
		if err != nil {
			fail("读取 coverage profile: %v", err)
		}
		summary, err := phase1gate.VerifyCoverage(profile, *aggregateMinimum, *packageMinimum)
		if err != nil {
			fail("unit coverage 门禁拒绝: %v", err)
		}
		fmt.Printf("unit coverage 门禁通过：总体 %.1f%%，%d 个核心包均不低于 %.1f%%\n", summary.TotalPercent, len(summary.PackagePercent), *packageMinimum)
	}

	if *reportsPath != "" {
		matrix, err := os.ReadFile(*matrixPath)
		if err != nil {
			fail("读取一期验收矩阵: %v", err)
		}
		ids, err := phase1gate.BlockingIDs(matrix)
		if err != nil {
			fail("读取 blocking ID: %v", err)
		}
		if *expectedBlockingCount > 0 && len(ids) != *expectedBlockingCount {
			fail("一期验收矩阵包含 %d 个 blocking ID，期望 %d 个", len(ids), *expectedBlockingCount)
		}
		e2eIDs, err := phase1gate.BlockingE2EIDs(matrix)
		if err != nil {
			fail("读取 blocking E2E ID: %v", err)
		}
		if *expectedE2ECount > 0 && len(e2eIDs) != *expectedE2ECount {
			fail("一期验收矩阵包含 %d 个 blocking E2E ID，期望 %d 个", len(e2eIDs), *expectedE2ECount)
		}
		summary, err := phase1gate.VerifyJUnitResults(ids, *reportsPath)
		if err != nil {
			fail("JUnit 门禁拒绝: %v", err)
		}
		fmt.Printf("JUnit 门禁通过：%d 个 blocking 场景，来自 %d 份报告\n", summary.Passed, summary.ReportFiles)
	}
}

func fail(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
