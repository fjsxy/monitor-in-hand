package kubestatemetricssupport

import (
	"encoding/json"
	"fmt"
)

// ReplaceObjectManifest 保留对象身份与声明字段，省略不应由 replace 回写的 status。
func ReplaceObjectManifest(object any) ([]byte, error) {
	manifest, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("编码 replace 对象: %w", err)
	}

	// replace 只提交调用方修改的声明字段，避免把新客户端识别的 status 字段回写给旧版本 API。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &fields); err != nil {
		return nil, fmt.Errorf("解析 replace 对象: %w", err)
	}
	delete(fields, "status")
	replaceManifest, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("编码 replace manifest: %w", err)
	}
	return replaceManifest, nil
}
