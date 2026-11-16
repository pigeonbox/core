package share

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
)

// ==================== 取件失败事件与取件时间线（frontend-core#93） ====================
//
// 寄件者视角的「谁在何时因何失败」：失败事件落 pickup_failures（只记录、
// 不扣次、7 天留存、不落明文 IP），与成功的 pickup_charges 合并成时间线。
// 三态：从未被访问 / 被尝试但失败 / 成功取件。

// 时间线三态（wire 取值；前端按此渲染状态标签）。
const (
	// PickupTimelineStateNever 从未被访问（无成功也无失败事件）。
	PickupTimelineStateNever = "never"
	// PickupTimelineStateFailed 被尝试但失败（只有失败事件）。
	PickupTimelineStateFailed = "failed"
	// PickupTimelineStatePicked 成功取件过（有过成功事件；失败事件仍在 Events 里可见）。
	PickupTimelineStatePicked = "picked"
)

const (
	// timelineResultSuccess / timelineResultFailed 事件结果枚举（wire）。
	timelineResultSuccess = "success"
	timelineResultFailed  = "failed"

	// pickupTimelineEventLimit 时间线事件条数上限（成功/失败各取最近 N 条再合并）。
	// 明细是增强信息：更早的事件按 7 天留存本就会被 janitor 清掉。
	pickupTimelineEventLimit = 50
)

// ErrTimelineNotFound 时间线不可达：分享不存在 / 不属于调用方 / 已删除。
// 三种情形统一一个哨兵 ⇒ handler 统一 404 同文案（不区分「没有」与「不是你的」，防枚举）。
var ErrTimelineNotFound = errors.New("分享不存在或无权访问")

// PickupTimelineEvent 时间线单条事件。
//
// 隐私边界（拍板）：**任何 IP/指纹都不下发**——失败面只给
// SameSource（与上一条已知来源的失败是否同一来源）与聚合层的来源计数。
type PickupTimelineEvent struct {
	// At 事件时刻（RFC3339Nano，UTC）。
	At string `json:"at"`
	// Result success / failed。
	Result string `json:"result"`
	// Reason 失败原因枚举（model.PickupFail*；仅 failed 事件有值）。
	Reason string `json:"reason,omitempty"`
	// Channel 交付面（select / download），观测用。
	Channel string `json:"channel,omitempty"`
	// SameSource 与**时间上一条已知来源**的失败是否同一来源（仅 failed 事件、
	// 且两侧指纹都已知时下发；nil = 不可比——首条或来源未知）。
	// 「是否同一来源」的布尔形态，拍板要求的粗粒度来源信息之一。
	SameSource *bool `json:"same_source,omitempty"`

	// t 内部排序键（不序列化：RFC3339Nano 掉尾零，字符串序 ≠ 时间序）。
	t   time.Time
	seq int
}

// PickupTimeline 取件时间线（owner 侧「分享详情」数据）。
type PickupTimeline struct {
	// State 三态：never / failed / picked。
	State string `json:"state"`
	// Events 成功+失败事件合并，按时间倒序（最新在前）。
	Events []*PickupTimelineEvent `json:"events"`
	// FailureCount 失败事件总数（不受 Events 截断影响）。
	FailureCount int64 `json:"failure_count"`
	// FailureSources 失败来源数（指纹去重；空指纹不计）——拍板的「来源计数」。
	FailureSources int64 `json:"failure_sources"`
}

// RecordPickupFailure 落一条取件失败事件。
//
// 约定：
//   - **只记录，不扣次**——计次不变量（失败不消耗次数）完全不动；
//   - 落库失败只告警不阻断取件主流程（观测面不得反噬交付面）；
//   - code 必须是**规范分享码**（能归因到分享才落表；码不存在的探测
//     挂不上分享行，走 lockout 枚举防护，不进本表——否则枚举流量会放大成
//     DB 写放大）。
func (s *Service) RecordPickupFailure(ctx context.Context, code, reason, channel, ip string) {
	if code == "" || reason == "" {
		return
	}
	row := &model.PickupFailure{
		Code:       code,
		Reason:     reason,
		Channel:    channel,
		SourceHash: failureSourceHash(code, ip),
		FailedAt:   time.Now(),
	}
	if err := s.fileCodeRepo.CreatePickupFailure(ctx, row); err != nil {
		logger.CtxWarn(ctx, "record pickup failure failed",
			"code", code, "reason", reason, "err", err)
	}
}

// failureSourceHash 粗粒度来源指纹：sha256(规范分享码 NUL IP) 前 32 hex。
//
// 为什么掺分享码：① 时间线只做**单分享内**的去重/判等，掺码后跨分享不可关联
// （隐私更优）；② 拿到表的人没有码字典就无法对 IPv4 空间做彩虹表。
// 空 IP（无可信代理解析）返回空指纹 ⇒ 聚合时按「来源未知」处理（不计数、不判等）。
func failureSourceHash(code, ip string) string {
	if ip == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(code + "\x00" + ip))
	return hex.EncodeToString(sum[:])[:32]
}

// PickupTimeline 取某分享的取件时间线（owner 侧；成功+失败按时间序）。
//
// 属主校验：分享必须存在且 UserID == 调用方；不可达一律 ErrTimelineNotFound
// （统一 404，不泄露「分享存在但不是你的」）。
func (s *Service) PickupTimeline(ctx context.Context, userID uint, code string) (*PickupTimeline, error) {
	if code == "" {
		return nil, ErrTimelineNotFound
	}
	fc, err := s.fileCodeRepo.GetByCode(ctx, code)
	if err != nil || fc == nil {
		return nil, ErrTimelineNotFound
	}
	if fc.UserID == nil || *fc.UserID != userID {
		return nil, ErrTimelineNotFound
	}

	pickups, err := s.fileCodeRepo.ListPickups(ctx, fc.Code, pickupTimelineEventLimit)
	if err != nil {
		return nil, err
	}
	fails, err := s.fileCodeRepo.ListPickupFailures(ctx, fc.Code, pickupTimelineEventLimit)
	if err != nil {
		return nil, err
	}
	failCount, err := s.fileCodeRepo.CountPickupFailures(ctx, fc.Code)
	if err != nil {
		return nil, err
	}
	failSources, err := s.fileCodeRepo.CountDistinctFailureSources(ctx, fc.Code)
	if err != nil {
		return nil, err
	}

	// 「是否同一来源」：按时间正序遍历失败事件，与**上一条已知来源**的失败判等。
	// fails 是倒序（新→旧）⇒ 从尾部往前走即时间正序。
	sameByFailID := make(map[uint]*bool, len(fails))
	prev := ""
	for i := len(fails) - 1; i >= 0; i-- {
		f := fails[i]
		if f.SourceHash == "" {
			continue // 来源未知：不判等、不推进基线
		}
		if prev != "" {
			same := prev == f.SourceHash
			sameByFailID[f.ID] = &same
		}
		prev = f.SourceHash
	}

	type timed struct {
		t   time.Time
		seq int
		ev  PickupTimelineEvent
	}
	items := make([]timed, 0, len(pickups)+len(fails))
	seq := 0
	for _, p := range pickups {
		items = append(items, timed{
			t: p.ChargedAt, seq: seq,
			ev: PickupTimelineEvent{
				Result:  timelineResultSuccess,
				Channel: p.Channel,
			},
		})
		seq++
	}
	for _, f := range fails {
		items = append(items, timed{
			t: f.FailedAt, seq: seq,
			ev: PickupTimelineEvent{
				Result:     timelineResultFailed,
				Reason:     f.Reason,
				Channel:    f.Channel,
				SameSource: sameByFailID[f.ID],
			},
		})
		seq++
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].t.Equal(items[j].t) {
			return items[i].t.After(items[j].t)
		}
		return items[i].seq > items[j].seq // 同刻：构建序倒放，确定性输出
	})

	events := make([]*PickupTimelineEvent, 0, len(items))
	for _, it := range items {
		ev := it.ev
		ev.At = it.t.UTC().Format(time.RFC3339Nano)
		events = append(events, &ev)
	}

	state := PickupTimelineStateNever
	if len(pickups) > 0 {
		state = PickupTimelineStatePicked
	} else if failCount > 0 {
		state = PickupTimelineStateFailed
	}

	return &PickupTimeline{
		State:          state,
		Events:         events,
		FailureCount:   failCount,
		FailureSources: failSources,
	}, nil
}
