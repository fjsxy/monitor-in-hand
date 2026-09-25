# Domain docs

本仓库采用 single-context domain 文档布局。

## 探索代码前

按需读取：

- 根目录的 `CONTEXT.md`
- `docs/adr/` 中与当前工作相关的 ADR

如果文件尚不存在，静默继续，不要仅因缺失而提前创建。

`/domain-modeling` 会在术语或决策真正明确时，按需创建这些文件。

## 文件结构

```text
/
├── CONTEXT.md
├── docs/
│   └── adr/
└── src/
```

## 使用统一领域语言

Issue title、设计建议、假设和测试名称，应使用 `CONTEXT.md` 中定义的领域术语。

如果所需概念尚未出现在 glossary 中：

1. 先判断是否正在发明项目并未使用的概念。
2. 如果确实存在领域缺口，交给 `/domain-modeling` 澄清并记录。

## ADR 冲突

如果新的输出与已有 ADR 冲突，必须明确指出冲突，不得静默覆盖：

> 与 ADR-0007 冲突，但建议重新讨论，因为……
