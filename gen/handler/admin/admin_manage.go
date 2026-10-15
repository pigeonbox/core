// admin_manage.go 管理端增强：用户 CRUD / 文件管理 / 富统计 Dashboard。
// 2026-10-10 IDL 化收编：由 customHandler 手写路由（transport/http/handler/
// admin_manage.go）迁入 gen 层，路由经 idl/admin.thrift 注册（gen/router/admin）。
// 均在 /admin 组（AdminMiddleware 保护），操作经审计落 admin_operation_logs。
//
// 信封沿用 pkg/resp 助手（成功 code=0/"success"+trace_id，错误=业务码）；仅
// AdminTransferLogs / AdminListFilesFiltered 走 legacy 信封（code=200 +
// data{items,total,page,page_size}），item 字段为手工小写 snake_case，逐字段保形
// （迁移前后响应 JSON 逐字段一致）。响应体由本文件的 map 构造 + resp/legacy 助手
// 产出，与迁移前逐字节一致；contracts/gen/admin 的 Req/Resp 仅作契约/文档。
package admin

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"golang.org/x/crypto/bcrypt"

	adminapp "github.com/pigeonbox/core/app/admin"
	userapp "github.com/pigeonbox/core/app/user"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
)

var (
	manageSvc     *adminapp.Service
	manageUserSvc *userapp.Service
)

// SetManageServices 注入管理端增强服务（bootstrap 调用）。
// storage 注入不在此处——全站存储实例由 transport/http/handler.SetManageStorage
// 持有（share 多文件/寄件码 handler 依赖面），gen handler 不 import storage（规则6）。
func SetManageServices(svc *adminapp.Service, userSvc *userapp.Service) {
	manageSvc = svc
	manageUserSvc = userSvc
}

func getManageSvc() *adminapp.Service {
	if manageSvc == nil {
		// fail-fast：装配缺失必须显式暴露（bootstrap 保证 SetManageServices 先于服务流量）
		panic("admin manage service not initialized: SetManageServices must be called before serving")
	}
	return manageSvc
}

func getManageUserSvc() *userapp.Service {
	if manageUserSvc == nil {
		panic("user manage service not initialized: SetManageServices must be called before serving")
	}
	return manageUserSvc
}

// audit 管理端增强操作审计（委托 app/admin 统一入口，消除与 service 层的孪生实现）
func audit(ctx context.Context, action, target string, success bool) {
	adminapp.Audit(ctx, action, target, success)
}

// ---- 本地样板（自 transport/http/handler/helpers.go 收口处复制，保持 wire 一致） ----

// paramID 解析路径参数 :id（失败写 400 响应并返回 ok=false）。
func paramID(c *app.RequestContext, label string) (uint, bool) {
	id64, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		resp.NewErrorWithMessage(c, 10001, label+" ID 格式错误")
		return 0, false
	}
	return uint(id64), true
}

// parsePage 分页查询参数解析（缺失/非法回退 1/20；上限截断由 dao clampPage 统一兜底）。
func parsePage(c *app.RequestContext) (page, pageSize int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	return
}

// respondLegacyPage 管理端分页的历史约定包络：前端判 code===200 且读
// data.items/total（与 resp.Success 的 code=0 体系不同）——形状是既有前端
// 契约，只收敛调用点、不改线上响应体。
func respondLegacyPage(c *app.RequestContext, items any, total int64, page, pageSize int) {
	c.JSON(consts.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"items":     items,
			"total":     total,
			"page":      page,
			"page_size": pageSize,
		},
	})
}

// ==================== 用户管理（CRUD 补齐） ====================

// AdminCreateUser 创建用户
// POST /admin/users  {username,email,password,nickname,role,max_storage_quota}
func AdminCreateUser(ctx context.Context, c *app.RequestContext) {
	var req struct {
		Username        string `json:"username"`
		Email           string `json:"email"`
		Password        string `json:"password"`
		Nickname        string `json:"nickname"`
		Role            string `json:"role"`
		MaxStorageQuota int64  `json:"max_storage_quota"`
	}
	if err := c.BindAndValidate(&req); err != nil || req.Username == "" || req.Password == "" {
		resp.NewErrorWithMessage(c, 10001, "username 与 password 必填")
		return
	}
	// role 白名单（2026-10-05 审计 P3：此前未校验，可写入任意角色串）
	if req.Role != "" && req.Role != "admin" && req.Role != "user" {
		resp.NewErrorWithMessage(c, 10001, "role 仅支持 admin/user")
		return
	}
	userSvc := getManageUserSvc()
	created, err := userSvc.Create(ctx, &userapp.CreateUserReq{
		Username: req.Username,
		Email:    req.Email,
		Password: req.Password,
		Nickname: req.Nickname,
	})
	if err == nil && req.Role != "" {
		_, err = userSvc.Update(ctx, created.ID, &userapp.UpdateUserReq{Role: req.Role})
	}
	if err == nil && req.MaxStorageQuota > 0 {
		_, err = userSvc.SetStorageQuota(ctx, created.ID, req.MaxStorageQuota)
	}
	if err != nil {
		resp.NewErrorWithMessage(c, 40002, "创建用户失败: "+err.Error())
		return
	}
	audit(ctx, "user.create", fmt.Sprintf("user %d (username=%s) created", created.ID, created.Username), true)
	resp.Success(c, created)
}

// AdminUpdateUser 更新用户（昵称/角色/状态/配额）
// PUT /admin/users/:id  {nickname,role,status,max_storage_quota,max_upload_size}
func AdminUpdateUser(ctx context.Context, c *app.RequestContext) {
	id64, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		resp.NewErrorWithMessage(c, 10001, "用户 ID 格式错误")
		return
	}
	var req struct {
		Nickname        string `json:"nickname"`
		Role            string `json:"role"`
		Status          string `json:"status"`
		MaxStorageQuota *int64 `json:"max_storage_quota"`
		MaxUploadSize   *int64 `json:"max_upload_size"`
	}
	if err := c.BindAndValidate(&req); err != nil {
		resp.NewErrorWithMessage(c, 10001, err.Error())
		return
	}
	if req.Status != "" && req.Status != "active" && req.Status != "inactive" && req.Status != "banned" {
		resp.NewErrorWithMessage(c, 10001, "status 仅支持 active/inactive/banned")
		return
	}
	if req.Role != "" && req.Role != "admin" && req.Role != "user" {
		resp.NewErrorWithMessage(c, 10001, "role 仅支持 admin/user")
		return
	}

	userSvc := getManageUserSvc()
	updated, err := userSvc.Update(ctx, uint(id64), &userapp.UpdateUserReq{
		Nickname: req.Nickname,
		Role:     req.Role,
		Status:   req.Status,
	})
	if err == nil && req.MaxStorageQuota != nil {
		_, err = userSvc.SetStorageQuota(ctx, uint(id64), *req.MaxStorageQuota)
	}
	if err == nil && req.MaxUploadSize != nil {
		_, err = userSvc.SetUploadSize(ctx, uint(id64), *req.MaxUploadSize)
	}
	if err != nil {
		resp.NewErrorWithMessage(c, 40001, "更新用户失败: "+err.Error())
		return
	}
	audit(ctx, "user.update", fmt.Sprintf("user %d updated (role=%s status=%s)", id64, req.Role, req.Status), true)
	resp.Success(c, updated)
}

// AdminDeleteUser 删除用户（级联软删分享，带审计）
// DELETE /admin/users/:id
func AdminDeleteUser(ctx context.Context, c *app.RequestContext) {
	id64, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		resp.NewErrorWithMessage(c, 10001, "用户 ID 格式错误")
		return
	}
	if err := getManageSvc().DeleteUser(ctx, uint(id64)); err != nil {
		resp.NewErrorWithMessage(c, 40001, "删除用户失败: "+err.Error())
		return
	}
	resp.SuccessWithMessage(c, "用户已删除", nil)
}

// AdminResetUserPassword 管理员重置用户密码
// POST /admin/users/:id/reset-password  {password}
func AdminResetUserPassword(ctx context.Context, c *app.RequestContext) {
	id64, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		resp.NewErrorWithMessage(c, 10001, "用户 ID 格式错误")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := c.BindAndValidate(&req); err != nil || len(req.Password) < 6 {
		resp.NewErrorWithMessage(c, 10001, "password 必填且至少 6 位")
		return
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		resp.NewErrorWithMessage(c, 10008, "密码哈希失败")
		return
	}
	repo := getManageSvc()
	if err := repo.ResetUserPassword(ctx, uint(id64), string(hashed)); err != nil {
		resp.NewErrorWithMessage(c, 40001, "重置密码失败: "+err.Error())
		return
	}
	audit(ctx, "user.reset_password", fmt.Sprintf("user %d password reset", id64), true)
	resp.SuccessWithMessage(c, "密码已重置", nil)
}

// AdminListUsersFiltered 带筛选的用户列表（keyword/status/role）
// GET /admin/users/filter?keyword=&status=&role=&page=&page_size=
func AdminListUsersFiltered(ctx context.Context, c *app.RequestContext) {
	page, pageSize := parsePage(c)
	users, total, err := getManageSvc().ListUsersFiltered(ctx, adminapp.UserListFilter{
		Keyword:  c.Query("keyword"),
		Status:   c.Query("status"),
		Role:     c.Query("role"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		resp.NewErrorWithMessage(c, 10008, "查询用户失败: "+err.Error())
		return
	}
	resp.Page(c, users, total, page, pageSize)
}

// ==================== 文件管理（详情/编辑/批量/下载） ====================

// AdminFileDetail 文件详情
// GET /admin/files/:id
func AdminFileDetail(ctx context.Context, c *app.RequestContext) {
	id, ok := paramID(c, "文件")
	if !ok {
		return
	}
	fc, children, err := getManageSvc().GetFileDetail(ctx, id)
	if err != nil {
		resp.NewErrorByCode(c, 20008)
		return
	}
	// P0 多文件：附子文件列表（旧单文件无子表行时回退主表合成）
	files := make([]map[string]interface{}, 0, len(children))
	for _, ch := range children {
		files = append(files, map[string]interface{}{
			"id":   ch.ID,
			"name": ch.DisplayName(),
			"size": ch.Size,
			"hash": ch.FileHash,
		})
	}
	if len(files) == 0 && fc.GetFilePath() != "" {
		files = append(files, map[string]interface{}{
			"id": 0, "name": fc.DisplayName(), "size": fc.Size, "hash": fc.FileHash,
		})
	}
	resp.Success(c, map[string]interface{}{
		"share":      fc,
		"files":      files,
		"file_count": len(files),
	})
}

// AdminGetUserSettings 获取"用户配置"段（生效值：持久化优先，回退 yaml）。
// GET /admin/config/user
func AdminGetUserSettings(ctx context.Context, c *app.RequestContext) {
	u := adminapp.EffectiveUserSettings(ctx)
	resp.Success(c, &u)
}

// AdminUpdateUserSettings 在线更新"用户配置"段：写穿 DB 并即时生效
// （注册开关/配额默认/会话时长）。
// PUT /admin/config/user
func AdminUpdateUserSettings(ctx context.Context, c *app.RequestContext) {
	var req adminapp.UserSettings
	if err := c.BindAndValidate(&req); err != nil {
		resp.NewErrorWithMessage(c, 10001, err.Error())
		return
	}
	if err := adminapp.Default().UpdateUserSettings(ctx, req); err != nil {
		resp.NewErrorWithMessage(c, 10008, "更新失败: "+err.Error())
		return
	}
	resp.Success(c, map[string]interface{}{"affected": 1})
}

// AdminUpdateFile 编辑文件（延期 / 改剩余次数）
// PUT /admin/files/:id  {expire_value, expire_style, expired_count}
func AdminUpdateFile(ctx context.Context, c *app.RequestContext) {
	id, ok := paramID(c, "文件")
	if !ok {
		return
	}
	var req struct {
		ExpireValue  *int   `json:"expire_value"`
		ExpireStyle  string `json:"expire_style"`
		ExpiredCount *int   `json:"expired_count"`
	}
	if err := c.BindAndValidate(&req); err != nil {
		resp.NewErrorWithMessage(c, 10001, err.Error())
		return
	}

	var expireAt *time.Time
	if req.ExpireValue != nil {
		if t := utils.CalculateExpireTime(*req.ExpireValue, req.ExpireStyle); t != nil {
			expireAt = t
		}
	}
	if err := getManageSvc().UpdateFileExpire(ctx, id, expireAt, req.ExpiredCount); err != nil {
		resp.NewErrorWithMessage(c, 10008, "更新失败: "+err.Error())
		return
	}
	audit(ctx, "file.update", fmt.Sprintf("file %d updated (expire set, count=%v)", id, req.ExpiredCount), true)
	resp.SuccessWithMessage(c, "已更新", nil)
}

// AdminBatchDeleteFiles 批量删除文件
// POST /admin/files/batch-delete  {ids:[]}
func AdminBatchDeleteFiles(ctx context.Context, c *app.RequestContext) {
	var req struct {
		IDs []uint `json:"ids"`
	}
	if err := c.BindAndValidate(&req); err != nil || len(req.IDs) == 0 {
		resp.NewErrorWithMessage(c, 10001, "ids 必填")
		return
	}
	n, err := getManageSvc().BatchDeleteFiles(ctx, req.IDs)
	if err != nil {
		resp.NewErrorWithMessage(c, 10008, "批量删除失败: "+err.Error())
		return
	}
	audit(ctx, "file.batch_delete", fmt.Sprintf("%d files deleted", n), true)
	resp.Success(c, map[string]interface{}{"deleted": n})
}

// AdminRestoreFiles 从回收站恢复（软删 → 存活）
// POST /admin/files/restore {ids:[]}
func AdminRestoreFiles(ctx context.Context, c *app.RequestContext) {
	var req struct {
		IDs []uint `json:"ids"`
	}
	if err := c.BindAndValidate(&req); err != nil || len(req.IDs) == 0 {
		resp.NewErrorWithMessage(c, 10001, "ids 必填")
		return
	}
	n, err := getManageSvc().RestoreFiles(ctx, req.IDs)
	if err != nil {
		resp.NewErrorWithMessage(c, 50001, "恢复失败: "+err.Error())
		return
	}
	audit(ctx, "file.restore", fmt.Sprintf("%d files restored", n), true)
	resp.Success(c, map[string]interface{}{"restored": n})
}

// AdminPurgeFiles 彻底删除（DB 硬删 + 存储对象删除，不可恢复）
// POST /admin/files/purge {ids:[]}
func AdminPurgeFiles(ctx context.Context, c *app.RequestContext) {
	var req struct {
		IDs []uint `json:"ids"`
	}
	if err := c.BindAndValidate(&req); err != nil || len(req.IDs) == 0 {
		resp.NewErrorWithMessage(c, 10001, "ids 必填")
		return
	}
	n, err := getManageSvc().PurgeFiles(ctx, req.IDs)
	if err != nil {
		resp.NewErrorWithMessage(c, 50001, "彻底删除失败: "+err.Error())
		return
	}
	audit(ctx, "file.purge", fmt.Sprintf("%d files purged permanently", n), true)
	resp.Success(c, map[string]interface{}{"purged": n})
}

// AdminBatchExtendFiles 批量延期
// POST /admin/files/batch-extend  {ids:[], expire_value, expire_style}
func AdminBatchExtendFiles(ctx context.Context, c *app.RequestContext) {
	var req struct {
		IDs         []uint `json:"ids"`
		ExpireValue int    `json:"expire_value"`
		ExpireStyle string `json:"expire_style"`
	}
	if err := c.BindAndValidate(&req); err != nil || len(req.IDs) == 0 {
		resp.NewErrorWithMessage(c, 10001, "ids 必填")
		return
	}
	expireAt := utils.CalculateExpireTime(req.ExpireValue, req.ExpireStyle)
	if expireAt == nil {
		resp.NewErrorWithMessage(c, 10001, "无效的过期样式: "+req.ExpireStyle)
		return
	}
	n, err := getManageSvc().BatchExtendFiles(ctx, req.IDs, *expireAt)
	if err != nil {
		resp.NewErrorWithMessage(c, 10008, "批量延期失败: "+err.Error())
		return
	}
	audit(ctx, "file.batch_extend", fmt.Sprintf("%d files extended to %s", n, expireAt.Format(time.RFC3339)), true)
	resp.Success(c, map[string]interface{}{"extended": n})
}

// AdminDownloadFile 管理端下载：302 到公开下载端点（附服务端签发的下载令牌）
// GET /admin/files/:id/download
func AdminDownloadFile(ctx context.Context, c *app.RequestContext) {
	id, ok := paramID(c, "文件")
	if !ok {
		return
	}
	fc, err := getManageSvc().GetFileByID(ctx, id)
	if err != nil {
		resp.NewErrorByCode(c, 20008)
		return
	}
	target := "/share/download?code=" + fc.Code
	if tk := security.GenerateDownloadToken(fc.Code); tk != "" {
		target += "&token=" + tk
	}
	c.Redirect(consts.StatusFound, []byte(target))
}

// ==================== Dashboard 富统计 ====================

// AdminEnhancedStats 富指标（昨日对比 / 下载总量 / top 后缀 / 类型分布 / 存储用量与配额）
// GET /admin/stats/enhanced
func AdminEnhancedStats(ctx context.Context, c *app.RequestContext) {
	stats, err := getManageSvc().EnhancedStats(ctx)
	if err != nil {
		resp.NewErrorWithMessage(c, 10008, "统计失败: "+err.Error())
		return
	}
	resp.Success(c, stats)
}

// AdminStatsTrend 趋势序列（连续 N 天，缺失日补 0）：
// uploads 来自 file_codes 按天创建数，downloads 来自 transfer_logs（pkg/transfer 写入）。
// GET /admin/stats/trend?days=7
func AdminStatsTrend(ctx context.Context, c *app.RequestContext) {
	days := 7
	if v, err := strconv.Atoi(c.Query("days")); err == nil && v > 0 && v <= 30 {
		days = v
	}
	series, err := getManageSvc().TrendSeries(ctx, days)
	if err != nil {
		resp.NewErrorWithMessage(c, 10008, "统计上传趋势失败: "+err.Error())
		return
	}
	resp.Success(c, map[string]interface{}{"days": series})
}

// ==================== 传输日志（补齐断线：此前前端调用的端点不存在、
// transfer_log 表无写入方，页面一直空数据） ====================

// AdminTransferLogs 传输日志分页查询
// GET /admin/logs/transfer?page=&page_size=&operation=&keyword=
func AdminTransferLogs(ctx context.Context, c *app.RequestContext) {
	page, pageSize := parsePage(c)
	query := model.TransferLogQuery{
		Operation: c.Query("operation"),
		Search:    c.Query("keyword"),
		Page:      page,
		PageSize:  pageSize,
	}
	logs, total, err := getManageSvc().GetTransferLogs(ctx, query)
	if err != nil {
		resp.NewErrorWithMessage(c, 10008, "查询传输日志失败: "+err.Error())
		return
	}
	// 显式小写字段映射（gorm.Model 默认序列化为大写 CreatedAt，与前端约定不符）
	items := make([]map[string]interface{}, 0, len(logs))
	for _, lg := range logs {
		items = append(items, map[string]interface{}{
			"id":          lg.ID,
			"operation":   lg.Operation,
			"file_code":   lg.FileCode,
			"file_name":   lg.FileName,
			"file_size":   lg.FileSize,
			"username":    lg.Username,
			"ip":          lg.IP,
			"duration_ms": lg.DurationMs,
			"created_at":  lg.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	// 前端 TransferLogs.vue 判 code===200 且读 data.items/total，故不用 resp.Success（code=0）
	respondLegacyPage(c, items, total, page, pageSize)
}

// ==================== 分享治理（2026-10-03）：强过滤列表 + 状态机 ====================

// AdminListFilesFiltered 管理端文件列表组合过滤。
// GET /admin/files/filter?keyword=&user_id=&upload_type=&owner_ip=&status=&
//
//	min_size=&max_size=&created_after=&created_before=&expired=&page=&page_size=
//
// 相比 IDL /admin/files（仅 keyword）补充滥用定位维度，返回项含 owner_ip/status/
// upload_type/user_id（owner_ip 仅管理端可见）。
func AdminListFilesFiltered(ctx context.Context, c *app.RequestContext) {
	q := model.FileCodeQuery{}
	q.Keyword = c.Query("keyword")
	q.UploadType = c.Query("upload_type")
	q.OwnerIP = c.Query("owner_ip")
	q.Status = c.Query("status")
	if s := c.Query("user_id"); s != "" {
		if id, err := strconv.ParseUint(s, 10, 64); err == nil && id > 0 {
			uid := uint(id)
			q.UserID = &uid
		}
	}
	if s := c.Query("min_size"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			q.MinSize = &v
		}
	}
	if s := c.Query("max_size"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			q.MaxSize = &v
		}
	}
	if s := c.Query("created_after"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			q.CreatedAfter = &t
		}
	}
	if s := c.Query("created_before"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			q.CreatedBefore = &t
		}
	}
	if s := c.Query("expired"); s == "true" || s == "false" {
		b := s == "true"
		q.Expired = &b
	}
	// 回收站筛选（回收站=仅已删 / all=含已删；缺省仅存活）
	q.Deleted = c.Query("deleted")
	// 健康洞察过滤（2026-10-07；口径见 dao.applyHealthFilter，空=不过滤）
	q.Health = c.Query("health")
	q.Page, _ = strconv.Atoi(c.Query("page"))
	q.PageSize, _ = strconv.Atoi(c.Query("page_size"))

	files, total, err := getManageSvc().GetFilesFiltered(ctx, q)
	if err != nil {
		resp.NewErrorWithMessage(c, 50001, "查询文件列表失败: "+err.Error())
		return
	}
	// P0 多文件：批量取子文件数（一次 GROUP BY）
	ids := make([]uint, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.ID)
	}
	counts := getManageSvc().ChildFileCounts(ctx, ids)
	items := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		item := fileGovernanceItem(f)
		if counts != nil {
			item["file_count"] = counts[f.ID]
		} else {
			item["file_count"] = int64(0)
		}
		items = append(items, item)
	}
	respondLegacyPage(c, items, total, q.Page, q.PageSize)
}

// fileGovernanceItem 管理端文件治理视图（含管控字段；owner_ip 仅管理端可见）
func fileGovernanceItem(f *model.FileCode) map[string]interface{} {
	fileName := f.DisplayName()
	isText := f.Text != "" && f.FilePath == ""
	item := map[string]interface{}{
		"id":            f.ID,
		"code":          f.Code,
		"file_name":     fileName,
		"is_text":       isText,
		"text_preview":  "",
		"size":          f.Size,
		"expired_at":    nil,
		"expired_count": f.ExpiredCount,
		"used_count":    f.UsedCount,
		"viewer_count":  f.ViewerCount,
		"status":        f.Status,
		"upload_type":   f.UploadType,
		"user_id":       f.UserID,
		"owner_ip":      f.OwnerIP,
		"require_auth":  f.RequireAuth,
		"created_at":    f.CreatedAt.Format("2006-01-02 15:04:05"),
	}
	if isText {
		preview := f.Text
		if runes := []rune(preview); len(runes) > 120 {
			preview = string(runes[:120]) + "..." // 按 rune 截断，防切碎 UTF-8
		}
		item["text_preview"] = preview
	}
	if f.ExpiredAt != nil {
		item["expired_at"] = f.ExpiredAt.Format("2006-01-02 15:04:05")
	}
	// 回收站标记（软删行 deleted_at 非空；deleted=only 筛选的行据此灰显/恢复）
	item["deleted"] = f.DeletedAt.Valid
	return item
}

// AdminSetFileStatus 设置单个分享管控状态（禁用/恢复/待审）
// PUT /admin/files/:id/status  {status: normal|blocked|pending_review}
func AdminSetFileStatus(ctx context.Context, c *app.RequestContext) {
	var req struct {
		Status string `json:"status"`
	}
	if err := c.BindAndValidate(&req); err != nil || !model.ValidShareStatus(req.Status) {
		resp.NewErrorWithMessage(c, 10001, "status 必须为 normal/blocked/pending_review")
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		resp.NewErrorWithMessage(c, 10001, "非法的文件 ID")
		return
	}
	n, err := getManageSvc().SetFilesStatus(ctx, []uint{uint(id)}, req.Status)
	if err != nil {
		resp.NewErrorWithMessage(c, 50001, "设置状态失败: "+err.Error())
		return
	}
	audit(ctx, "file."+req.Status, fmt.Sprintf("file %d status -> %s (affected=%d)", id, req.Status, n), true)
	resp.Success(c, map[string]interface{}{"affected": n})
}

// AdminBatchSetFilesStatus 批量设置分享管控状态（滥用处置主路径：定位 IP 后批量禁用）
// POST /admin/files/batch-status  {ids: [..], status: normal|blocked|pending_review}
func AdminBatchSetFilesStatus(ctx context.Context, c *app.RequestContext) {
	var req struct {
		IDs    []uint `json:"ids"`
		Status string `json:"status"`
	}
	if err := c.BindAndValidate(&req); err != nil || !model.ValidShareStatus(req.Status) {
		resp.NewErrorWithMessage(c, 10001, "status 必须为 normal/blocked/pending_review 且 ids 非空")
		return
	}
	if len(req.IDs) == 0 {
		resp.NewErrorWithMessage(c, 10001, "ids 不能为空")
		return
	}
	if len(req.IDs) > 500 {
		resp.NewErrorWithMessage(c, 10001, "单批最多 500 条")
		return
	}
	n, err := getManageSvc().SetFilesStatus(ctx, req.IDs, req.Status)
	if err != nil {
		resp.NewErrorWithMessage(c, 50001, "批量设置状态失败: "+err.Error())
		return
	}
	audit(ctx, "file.batch-"+req.Status, fmt.Sprintf("batch status -> %s ids=%d (affected=%d)", req.Status, len(req.IDs), n), true)
	resp.Success(c, map[string]interface{}{"affected": n})
}
