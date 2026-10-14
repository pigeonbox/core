// invite.go — 实例间推送邀请（P3，spec §14：免口令交接）。
//
// 语义：邀请 = 口令 bearer（与手工交接同信任级——口令本身即授权，通道只是
// 递送）；收件箱内存态（cap 20 / TTL 10 分钟 = p2pc 配对窗口，进程重启丢弃
// 可接受，口令同样已失效）；出站推送目标 URL 走 SSRF 守卫（与 registry_url
// 同策略，私网目标需 security.ssrf.allow_private_networks）。
package p2px

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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

// AddInvite 邀请入箱（口令格式校验 + 同码去重 + 满箱拒绝）。
func (s *Service) AddInvite(code, fromName, note string) (Invite, error) {
	code = normalizeCode(code)
	if len(code) != 12+2 { // 12 位字符 + 2 连字符（normalizeCode 输出 4-4-4）
		return Invite{}, errors.New("邀请口令格式非法（需 XXXX-XXXX-XXXX）")
	}
	s.invMu.Lock()
	defer s.invMu.Unlock()
	s.expireInvitesLocked()
	for _, it := range s.invites {
		if it.Code == code {
			return it, nil // 同口令去重（重复推送幂等）
		}
	}
	if len(s.invites) >= inviteMaxCount {
		return Invite{}, fmt.Errorf("收件箱已满（%d），先处理或等待过期", inviteMaxCount)
	}
	inv := Invite{
		ID:         newTaskID(),
		Code:       code,
		FromName:   sanitizeInline(fromName, 64),
		Note:       sanitizeInline(note, 200),
		ReceivedAt: s.now(),
	}
	s.invites = append(s.invites, inv)
	return inv, nil
}

// ListInvites 未过期邀请（新到在前）。
func (s *Service) ListInvites() []InviteSnapshot {
	s.invMu.Lock()
	defer s.invMu.Unlock()
	s.expireInvitesLocked()
	out := make([]InviteSnapshot, 0, len(s.invites))
	for i := len(s.invites) - 1; i >= 0; i-- {
		it := s.invites[i]
		out = append(out, InviteSnapshot{
			ID: it.ID, Code: it.Code, FromName: it.FromName, Note: it.Note,
			ReceivedAt: it.ReceivedAt,
			ExpiresIn:  int64((it.ReceivedAt.Add(inviteTTL)).Sub(s.now()).Seconds()),
		})
	}
	return out
}

// TakeInvite 认领（出箱）；不存在/已过期报错。
func (s *Service) TakeInvite(id string) (Invite, error) {
	s.invMu.Lock()
	defer s.invMu.Unlock()
	s.expireInvitesLocked()
	for i, it := range s.invites {
		if it.ID == id {
			s.invites = append(s.invites[:i], s.invites[i+1:]...)
			return it, nil
		}
	}
	return Invite{}, errors.New("邀请不存在或已过期")
}

// expireInvitesLocked 清过期（调用方持 invMu）。
func (s *Service) expireInvitesLocked() {
	now := s.now()
	kept := s.invites[:0]
	for _, it := range s.invites {
		if now.Sub(it.ReceivedAt) < inviteTTL {
			kept = append(kept, it)
		}
	}
	s.invites = kept
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
		"code":      normalizeCode(code),
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
