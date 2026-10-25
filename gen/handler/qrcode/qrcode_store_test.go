package qrcode

import (
	"testing"
)

// handler 层行为断言（2026-10-25 审计 A5：gen/handler/qrcode 此前**零测试**）。
//
// 核心契约：**容量超限时淘汰「最早写入」的条目**。
// 修前实现是 `for k := range QRCodeStore { delete(k); break }` ⇒ Go map 迭代序
// 随机 ⇒ 删的是任意一条 ⇒ 注释承诺的「近似 LRU」不成立（后写入的可能先被淘汰）。
//
// 断言必须能变红（负对照）：把 qrcodePut 的淘汰逻辑改回「随机删一条」⇒ 本用例红。

// TestQrcodePut_EvictsOldestNotRandom 容量超限 ⇒ 淘汰**最早写入**的条目。
//
// 多轮重复：随机实现下「最早那条恰好没被删」的概率约 2/3 ⇒ 单轮检出不稳；
// 20 轮全中的概率 ≈ 0.03% ⇒ 检出近乎确定（正确实现下恒绿，无 flaky）。
func TestQrcodePut_EvictsOldestNotRandom(t *testing.T) {
	// 把容量调到 3（测试内改包级变量，测完复原）
	prevCap := qrcodeMaxCap
	qrcodeMaxCap = 3
	defer func() { qrcodeMaxCap = prevCap }()

	for round := 0; round < 20; round++ {
		// 清空存量
		qrcodeMu.Lock()
		QRCodeStore = make(map[string]*StoredQRCode)
		qrcodeMu.Unlock()

		// 写入 a,b,c,d（d 触发淘汰）
		for _, id := range []string{"a", "b", "c", "d"} {
			qrcodePut(id, &StoredQRCode{ID: id, DataStr: id})
		}

		qrcodeMu.RLock()
		_, hasA := QRCodeStore["a"]
		hasB, hasC, hasD := QRCodeStore["b"] != nil, QRCodeStore["c"] != nil, QRCodeStore["d"] != nil
		size := len(QRCodeStore)
		qrcodeMu.RUnlock()

		if hasA {
			t.Fatalf("第 %d 轮：最早的条目 a 必须被淘汰（修前是随机删 ⇒ 可能留着 a 而删掉 b/c/d）", round)
		}
		if !hasB || !hasC || !hasD {
			t.Fatalf("第 %d 轮：较新的条目应保留：b=%v c=%v d=%v", round, hasB, hasC, hasD)
		}
		if size != 3 {
			t.Fatalf("第 %d 轮：存量应维持在容量上限 3，实得 %d", round, size)
		}
	}
}

// TestQrcodePut_EvictsOldestAcrossManyInserts 连续插入 ⇒ 始终淘汰最老的一条
// （随机实现下大概率留不住新条目 ⇒ 断言会红）。
func TestQrcodePut_EvictsOldestAcrossManyInserts(t *testing.T) {
	prevCap := qrcodeMaxCap
	qrcodeMaxCap = 5
	defer func() { qrcodeMaxCap = prevCap }()

	qrcodeMu.Lock()
	QRCodeStore = make(map[string]*StoredQRCode)
	qrcodeMu.Unlock()

	for i := 0; i < 50; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		qrcodePut(id, &StoredQRCode{ID: id, DataStr: id})
	}

	qrcodeMu.RLock()
	size := len(QRCodeStore)
	newestPresent := QRCodeStore[string(rune('a'+49%26))+string(rune('0'+49/26))] != nil
	qrcodeMu.RUnlock()

	if size != 5 {
		t.Errorf("存量应维持在容量上限 5，实得 %d", size)
	}
	if !newestPresent {
		t.Error("最新写入的条目必须仍在（随机淘汰下它可能刚被删掉）")
	}
}

// TestQrcodeGet 读取路径。
func TestQrcodeGet(t *testing.T) {
	qrcodeMu.Lock()
	QRCodeStore = make(map[string]*StoredQRCode)
	qrcodeMu.Unlock()

	if _, ok := qrcodeGet("nope"); ok {
		t.Error("不存在的 ID 不应命中")
	}
	qrcodePut("x", &StoredQRCode{ID: "x", DataStr: "hello"})
	got, ok := qrcodeGet("x")
	if !ok || got == nil {
		t.Fatal("写入后应能读到")
	}
	if got.DataStr != "hello" {
		t.Errorf("内容不对: %q", got.DataStr)
	}
}
