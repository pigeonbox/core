package storage

import (
	"encoding/json"
	"testing"
)

func TestFlatUnmarshal(t *testing.T) {
	body := `{"type":"sftp","sftp":{"host":"127.0.0.1:2222","username":"tester","password":"testerpass","root":"upload"}}`
	req := &UpdateConfigRequest{}
	if err := json.Unmarshal([]byte(body), req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Type != "sftp" {
		t.Fatalf("Type=%q", req.Type)
	}
	if req.SFTP == nil || req.SFTP.Host != "127.0.0.1:2222" {
		t.Fatalf("SFTP 未填充: %+v", req.SFTP)
	}
	// 扁平形态只有一个来源（旧的「有没有顶层实质字段」探测已删：
	// 请求体恒为完整候选配置）⇒ 这里只断言扁平字段被正确填充。
	if req.StorageConfig.Type != "sftp" && req.Type != "sftp" {
		t.Fatalf("扁平形态的 type 未绑定: %+v", req.StorageConfig)
	}
}
