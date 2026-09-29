package collection_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCollectionHelpers(t *testing.T) {
	t.Run("[UT-COLLECTION-GH59-001] 将 kubectl 警告与授权结果分流", func(t *testing.T) {
		// 模拟 kubectl auth can-i 拒绝请求时 stdout 返回 no、stderr 返回 scope 警告。
		command := exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestCollectionCommandFixture")
		command.Env = append(os.Environ(), "GO_WANT_COLLECTION_COMMAND_FIXTURE=1")
		output, err := commandResult(command)

		// 授权拒绝仍须保留非零退出，同时机器可判定输出只能包含 stdout。
		if err == nil {
			t.Fatal("expected denied authorization to return an error")
		}
		if output != "no\n" {
			t.Fatalf("expected stdout-only authorization decision, got %q", output)
		}
		if !strings.Contains(err.Error(), "resource 'nodes' is not namespace scoped") {
			t.Fatalf("expected stderr warning in returned error, got %q", err)
		}
	})

	t.Run("[UT-COLLECTION-GH59-002] 从格式化 Event JSON 读取拒绝原因", func(t *testing.T) {
		// 重放两个失败 job 都捕获到的 pretty-printed Event 形态。
		events := `{
  "items": [
    {
      "reason": "InvalidConfiguration",
      "type": "Warning"
    }
  ]
}`
		found, err := eventHasReason(events, "InvalidConfiguration")
		if err != nil {
			t.Fatalf("parse events: %v", err)
		}
		if !found {
			t.Fatal("expected InvalidConfiguration event reason")
		}
		if _, err := eventHasReason(`{"items":`, "InvalidConfiguration"); err == nil {
			t.Fatal("expected malformed event JSON to return an error")
		}
	})
}

func TestCollectionCommandFixture(t *testing.T) {
	if os.Getenv("GO_WANT_COLLECTION_COMMAND_FIXTURE") != "1" {
		return
	}
	fmt.Fprintln(os.Stderr, "Warning: resource 'nodes' is not namespace scoped")
	fmt.Fprintln(os.Stdout, "no")
	os.Exit(1)
}
