package p2px

// 浏览器文件直发（2026-10-15:p2pcweb 体验融入管理台）。
// 管理端点把 multipart 上传的文件先落 <DataDir>/p2p/upload/<uploadID>/，
// 再经 SendUploaded 编排 p2pc send。
//
// 授权模型：上传目录自带发送授权——文件是本端点刚写入的，等效白名单；
// 不查 send_roots（白名单防的是「借此读服务器既有文件」，上传直发做不到）。
// 任务终态自动清理临时目录；孤儿目录（进程崩溃残留）24h 兜底清扫。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// uploadStaleAge 上传孤儿目录兜底清扫年龄（正常任务终态即清，此为崩溃残留）。
const uploadStaleAge = 24 * time.Hour

// UploadRoot 上传临时区根目录（<DataDir>/p2p/upload）。
func (s *Service) UploadRoot() string {
	return filepath.Join(s.deps.DataDir, "p2p", "upload")
}

// NewUploadDir 建一个上传会话目录（端点先写文件、后编排；目录名与任务 ID
// 同空间随机串，清扫时可按「是否在册任务」判活）。
func (s *Service) NewUploadDir() (string, error) {
	dir := filepath.Join(s.UploadRoot(), newTaskID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("上传目录创建失败: %w", err)
	}
	return dir, nil
}

// UploadQuotaBytes 生效单任务配额（上传端点流式落盘的硬闸；guard 不过即错）。
func (s *Service) UploadQuotaBytes(ctx context.Context) (int64, error) {
	if _, _, err := s.guard(ctx); err != nil {
		return 0, err
	}
	cfg, ok := s.deps.GetDirect(ctx)
	if !ok {
		return defaultMaxTaskBytes, nil
	}
	return effectiveQuota(cfg.MaxTaskBytes, defaultMaxTaskBytes), nil
}

// SendUploaded 编排浏览器上传文件直发：与 Send 同参同语义，唯授权从
// send_roots 白名单换成上传目录前缀校验（symlink 落点一并核）。
func (s *Service) SendUploaded(ctx context.Context, sessionDir string, files []string, opts ...SendOption) (*Task, error) {
	if len(files) == 0 {
		return nil, errors.New("files 不能为空")
	}
	root := s.UploadRoot()
	// 与 validateSendPaths 同款 symlink 归一:macOS /var→/private/var 一类
	// 临时目录符号链接会让未归一的 root 前缀永远对不上
	if real, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = real
	}
	cleanDir, err := filepath.EvalSymlinks(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("上传目录不可用: %w", err)
	}
	if cleanDir != root && !strings.HasPrefix(cleanDir, root+string(os.PathSeparator)) {
		return nil, errors.New("上传目录越界")
	}
	for _, f := range files {
		real, rerr := filepath.EvalSymlinks(f)
		if rerr != nil {
			return nil, fmt.Errorf("上传文件不可用: %s", filepath.Base(f))
		}
		if real != cleanDir && !strings.HasPrefix(real, cleanDir+string(os.PathSeparator)) {
			return nil, errors.New("上传文件越界")
		}
	}
	return s.sendWithAuth(ctx, files, append(opts, WithUploadDir(cleanDir)))
}

// sweepUploads 上传孤儿目录清扫（monitorLoop 周期调用）：超龄且不属任何
// 在册活跃任务即删（终态任务 1h 后从 map 剪除，其目录按年龄兜底）。
func (s *Service) sweepUploads() {
	root := s.UploadRoot()
	// 与 SendUploaded 的 uploadDir 记录口径一致(symlink 归一后路径)
	if real, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = real
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	// 活跃任务的上传会话目录豁免（目录名与任务 ID 是两个随机空间,按值匹配）
	active := map[string]bool{}
	s.mu.Lock()
	for _, t := range s.tasks {
		t.mu.Lock()
		if t.state == StateWaiting || t.state == StateRunning || t.state == StateRetrying {
			if t.uploadDir != "" {
				active[t.uploadDir] = true
			}
		}
		t.mu.Unlock()
	}
	s.mu.Unlock()

	cutoff := time.Now().Add(-uploadStaleAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(root, e.Name())
		if active[full] {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(full)
	}
}
