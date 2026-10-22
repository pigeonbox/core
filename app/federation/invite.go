// invite.go — 联邦信封管理（跨实例推送邀请；2026-10-22 由直传包搬入）。
//
// 语义：邀请 = 口令 bearer（与手工交接同信任级——口令本身即授权，通道只是
// 递送）；收件箱内存态（cap 20 / TTL 10 分钟 = p2pc 配对窗口，进程重启丢弃
// 可接受，口令同样已失效）；出站推送目标 URL 走 SSRF 守卫（与 registry_url
// 同策略，私网目标需 security.ssrf.allow_private_networks）。
//
// 归属：信封是跨实例交接的凭证与状态机，属联邦域，不属直传域——本文件不
// 依赖 p2pc 运行时（无 spawn / 子进程 / 任务环），Wave B 删直传包时
// 本包原样留存。
//
// 命名（2026-10-22 L3）：本包三类服务按"构造条件"分家，读者一眼可辨——
//   - Service：联邦运行时（注册/心跳/公告），**仅联邦启用时**构造；
//   - InviteMailbox：联邦邀请收件箱，**无条件**构造（联邦关闭时仍可用，
//     它是跨实例交接的凭证与状态机，与联邦运行时无关）；
//   - PeerDirectory：联邦节点名录，**无条件**构造（可见性只取决于 registry
//     可达，不取决于联邦是否启用）。
package federation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pigeonbox/core/pkg/paircode"
	"github.com/pigeonbox/core/pkg/security"
)

const (
	inviteTTL      = 10 * time.Minute // p2pc 配对窗口（spec §3.1）
	inviteMaxCount = 20
)

// Invite 待收邀请（收件箱条目）。
type Invite struct {
	ID         string
	Code       string
	FromName   string
	Note       string
	ReceivedAt time.Time
}

// InviteSnapshot 对外快照（含过期倒计时，前端渲染用）。
type InviteSnapshot struct {
	ID         string    `json:"id"`
	Code       string    `json:"code"`
	FromName   string    `json:"from_name"`
	Note       string    `json:"note"`
	ReceivedAt time.Time `json:"received_at"`
	ExpiresIn  int64     `json:"expires_in"` // 秒
}

// InviteMailbox 联邦信封收件箱（内存态；不依赖 p2pc 运行时）。
//
// 无条件构造（联邦关闭时仍可用——收件箱是跨实例交接的凭证与状态机，不是
// 联邦运行时的附属物；见文件头命名约定）。
//
// clock 为构造期注入的时钟（nil=time.Now）；构造后不再变更，故读路径无需
// 加锁（与搬迁前直传包的 nowFn 语义一致）。
type InviteMailbox struct {
	mu      sync.Mutex
	invites []Invite
	clock   func() time.Time
}

// NewInviteMailbox 构造收件箱（clock 可注入；nil=真实时钟）。
func NewInviteMailbox(clock func() time.Time) *InviteMailbox {
	return &InviteMailbox{clock: clock}
}

// now 当前时间（注入优先）。
func (m *InviteMailbox) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// AddInvite 邀请入箱（口令格式校验 + 同码去重 + 满箱拒绝）。
func (m *InviteMailbox) AddInvite(code, fromName, note string) (Invite, error) {
	code = paircode.Normalize(code)
	if len(code) != 12+2 { // 12 位字符 + 2 连字符（paircode.Normalize 输出 4-4-4）
		return Invite{}, errors.New("邀请口令格式非法（需 XXXX-XXXX-XXXX）")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireInvitesLocked()
	for _, it := range m.invites {
		if it.Code == code {
			return it, nil // 同口令去重（重复推送幂等）
		}
	}
	if len(m.invites) >= inviteMaxCount {
		return Invite{}, fmt.Errorf("收件箱已满（%d），先处理或等待过期", inviteMaxCount)
	}
	inv := Invite{
		ID:         newEnvelopeID(),
		Code:       code,
		FromName:   sanitizeInline(fromName, 64),
		Note:       sanitizeInline(note, 200),
		ReceivedAt: m.now(),
	}
	m.invites = append(m.invites, inv)
	return inv, nil
}

// ListInvites 未过期邀请（新到在前）。
func (m *InviteMailbox) ListInvites() []InviteSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireInvitesLocked()
	out := make([]InviteSnapshot, 0, len(m.invites))
	for i := len(m.invites) - 1; i >= 0; i-- {
		it := m.invites[i]
		out = append(out, InviteSnapshot{
			ID: it.ID, Code: it.Code, FromName: it.FromName, Note: it.Note,
			ReceivedAt: it.ReceivedAt,
			ExpiresIn:  int64((it.ReceivedAt.Add(inviteTTL)).Sub(m.now()).Seconds()),
		})
	}
	return out
}

// TakeInvite 认领（出箱）；不存在/已过期报错。
func (m *InviteMailbox) TakeInvite(id string) (Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireInvitesLocked()
	for i, it := range m.invites {
		if it.ID == id {
			m.invites = append(m.invites[:i], m.invites[i+1:]...)
			return it, nil
		}
	}
	return Invite{}, errors.New("邀请不存在或已过期")
}

// ExpiresIn 某条信封的剩余有效期（秒；已过期返回 0）。
//
// 供**已出箱**信封的展示方使用（认领端点返回信封时算剩余时间）——
// 收件箱只对在箱条目给快照，出箱后条目已不在列表里，无处可查。
// 时钟同收件箱（构造期注入的时钟优先，测试用假时钟一致）。
func (m *InviteMailbox) ExpiresIn(inv Invite) int64 {
	left := int64(m.now().Sub(inv.ReceivedAt.Add(inviteTTL)).Seconds())
	if left < 0 {
		return 0
	}
	return left
}

// expireInvitesLocked 清过期（调用方持 mu）。
func (m *InviteMailbox) expireInvitesLocked() {
	now := m.now()
	kept := m.invites[:0]
	for _, it := range m.invites {
		if now.Sub(it.ReceivedAt) < inviteTTL {
			kept = append(kept, it)
		}
	}
	m.invites = kept
}

// sanitizeInline 单行文本消毒（剥控制符/换行，截断长度）。
func sanitizeInline(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

// newEnvelopeID 短随机信封 ID（16 hex；与直传任务 ID 同形不同域）。
func newEnvelopeID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// PushInvite 把口令推送到目标实例的邀请收件端点（P3 免口令交接）。
// targetURL = 目标实例基址（如 https://b.example.com）；
// 实际 POST <target>/api/v1/p2p/invite，10s 超时。
func PushInvite(ctx context.Context, targetURL, code, fromName, note string) error {
	base := strings.TrimRight(strings.TrimSpace(targetURL), "/")
	if base == "" {
		return errors.New("目标实例地址不能为空")
	}
	// SSRF 守卫（与 registry_url 同策略：私网目标需 allow_private_networks）
	if err := security.ValidateEndpointURL(base); err != nil {
		return fmt.Errorf("目标地址校验失败: %w", err)
	}
	payload, _ := json.Marshal(map[string]string{
		"code":      paircode.Normalize(code),
		"from_name": sanitizeInline(fromName, 64),
		"note":      sanitizeInline(note, 200),
	})
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		base+"/api/v1/p2p/invite", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("推送失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("目标实例返回 %d", resp.StatusCode)
	}
	return nil
}
