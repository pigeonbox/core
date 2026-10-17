package user

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pigeonbox/contracts/errcode"
	usermodel "github.com/pigeonbox/contracts/gen/user"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/oekit/kitx/uidgen"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type CreateUserReq struct {
	Username string
	Email    string
	Password string
	Nickname string
}

type UpdateUserReq struct {
	Nickname string
	Avatar   string
	Role     string
	Status   string
}

// DefaultsProvider 系统级用户默认值来源（管理后台持久化配置优先，回退 yaml）。
// 消费侧接口：bootstrap 注入 admin 域适配器；未注入时回退全局 conf——
// 本域不依赖 admin 包（跨域依赖收口为装配点）。
type DefaultsProvider interface {
	// DefaultStorageQuota 用户存储配额默认值（字节，0 = 不限）
	DefaultStorageQuota(ctx context.Context) int64
	// DefaultUploadSize 单次上传大小默认上限（字节，0 = 不限）
	DefaultUploadSize(ctx context.Context) int64
	// AllowUserRegistration 是否开放注册（管理后台"用户配置"持久化值优先）
	AllowUserRegistration(ctx context.Context) bool
}

type Service struct {
	repo         *dao.UserRepository
	apiKeyRepo   *dao.UserAPIKeyRepository
	fileCodeRepo *dao.FileCodeRepository
	defaults     DefaultsProvider
}

func NewService(gormDB *gorm.DB) *Service {
	return &Service{
		repo:         dao.NewUserRepository(gormDB),
		apiKeyRepo:   dao.NewUserAPIKeyRepository(gormDB),
		fileCodeRepo: dao.NewFileCodeRepository(gormDB),
	}
}

// SetDefaultsProvider 注入系统级默认值来源（bootstrap 装配调用）。
func (s *Service) SetDefaultsProvider(p DefaultsProvider) { s.defaults = p }

// systemDefaults 无注入 provider 时的回退：直接读全局 yaml 配置
// （与 admin 域"无持久化记录回退 yaml"的兜底语义一致）。
func systemDefaults(ctx context.Context) (quota, uploadSize int64) {
	_ = ctx
	if cfg := conf.GetGlobalConfig(); cfg != nil {
		return cfg.User.UserStorageQuota, cfg.User.UserUploadSize
	}
	return 0, 0
}

func (s *Service) defaultStorageQuota(ctx context.Context) int64 {
	if s.defaults != nil {
		return s.defaults.DefaultStorageQuota(ctx)
	}
	q, _ := systemDefaults(ctx)
	return q
}

func (s *Service) defaultUploadSize(ctx context.Context) int64 {
	if s.defaults != nil {
		return s.defaults.DefaultUploadSize(ctx)
	}
	_, u := systemDefaults(ctx)
	return u
}

// RegistrationAllowed 是否开放注册（系统级设置来源：注入 provider 的管理后台
// 持久化值优先，未注入回退 yaml）。gen/handler/user 的 Register 入口经此消费，
// 不再直接 import admin 域包级单例（跨面依赖收口为装配点，2026-10-10）。
func (s *Service) RegistrationAllowed(ctx context.Context) bool {
	if s.defaults != nil {
		return s.defaults.AllowUserRegistration(ctx)
	}
	if cfg := conf.GetGlobalConfig(); cfg != nil {
		return cfg.User.AllowUserRegistration
	}
	return false
}

// SystemDefaultStorageQuota 系统级默认存储配额（字节，0 = 不限）。
// 与域内配额闸（defaultStorageQuota）同源；供 handler 展示层消费
// （用户级 max_storage_quota 覆盖仍由调用方自行优先）。
func (s *Service) SystemDefaultStorageQuota(ctx context.Context) int64 {
	return s.defaultStorageQuota(ctx)
}

func (s *Service) Create(ctx context.Context, req *CreateUserReq) (*model.UserResp, error) {

	// username 字符集白名单（2026-10-05 审计 P3）：注册/建号共用本收口。
	// 昵称不受限（展示字段）；username 进日志/审计/键值，收紧为安全字符集。
	if err := utils.ValidateUsername(req.Username); err != nil {
		return nil, err
	}

	existing, err := s.repo.GetByUsername(ctx, req.Username)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if existing != nil {
		return nil, errors.New("username already exists")
	}

	existing, err = s.repo.GetByEmail(ctx, req.Email)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if existing != nil {
		return nil, errors.New("email already exists")
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &model.User{
		Username:     req.Username,
		Email:        req.Email,
		PasswordHash: string(hashedPassword),
		Nickname:     req.Nickname,
		Status:       "active",
		Role:         "user",
	}

	if err := s.repo.Create(ctx, user); err != nil {
		return nil, err
	}

	return user.ToResp(), nil
}

func (s *Service) GetByID(ctx context.Context, id uint) (*model.UserResp, error) {
	user, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("user not found")
		}
		return nil, err
	}
	return user.ToResp(), nil
}

func (s *Service) Update(ctx context.Context, id uint, req *UpdateUserReq) (*model.UserResp, error) {
	user, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("user not found")
		}
		return nil, err
	}

	roleChanged := req.Role != "" && req.Role != user.Role
	statusChanged := req.Status != "" && req.Status != user.Status
	if req.Nickname != "" {
		user.Nickname = req.Nickname
	}
	if req.Avatar != "" {
		user.Avatar = req.Avatar
	}
	if req.Role != "" {
		user.Role = req.Role
	}
	if req.Status != "" {
		user.Status = req.Status
	}

	if err := s.repo.Update(ctx, user); err != nil {
		return nil, err
	}
	// 角色/状态变更 bump 会话纪元：降权/封禁即时踢出既有会话
	// （2026-10-05 审计 P2；此前降权的 admin JWT 有效至自然过期）
	if roleChanged || statusChanged {
		if err := s.repo.BumpSessionEpoch(ctx, id); err == nil {
			middleware.InvalidateIdentity(id)
		}
	}

	return user.ToResp(), nil
}

func (s *Service) Delete(ctx context.Context, id uint) error {
	_, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("user not found")
		}
		return err
	}

	return s.repo.Delete(ctx, id)
}

func (s *Service) List(ctx context.Context, page, pageSize int) ([]*model.UserResp, int64, error) {
	users, total, err := s.repo.List(ctx, page, pageSize)
	if err != nil {
		return nil, 0, err
	}

	resps := make([]*model.UserResp, len(users))
	for i, user := range users {
		resps[i] = user.ToResp()
	}

	return resps, total, nil
}

// Login 用户登录
func (s *Service) Login(ctx context.Context, username, password string) (*model.UserResp, string, error) {
	user, err := s.repo.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", errors.New("用户名或密码错误")
		}
		return nil, "", err
	}

	// 验证密码
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, "", errors.New("用户名或密码错误")
	}

	// 检查用户状态
	if user.Status != "active" {
		return nil, "", errors.New("用户账号已被禁用")
	}

	// 生成 JWT Token
	token, err := auth.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		return nil, "", errors.New("生成令牌失败")
	}

	return user.ToResp(), token, nil
}

// LoginByEmail 通过邮箱登录
func (s *Service) LoginByEmail(ctx context.Context, email, password string) (*model.UserResp, string, error) {
	user, err := s.repo.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", errors.New("邮箱或密码错误")
		}
		return nil, "", err
	}

	// 验证密码
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, "", errors.New("邮箱或密码错误")
	}

	// 检查用户状态
	if user.Status != "active" {
		return nil, "", errors.New("用户账号已被禁用")
	}

	// 生成 JWT Token
	token, err := auth.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		return nil, "", errors.New("生成令牌失败")
	}

	return user.ToResp(), token, nil
}

// ChangePassword 修改密码
func (s *Service) ChangePassword(ctx context.Context, userID uint, oldPassword, newPassword string) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return errors.New("用户不存在")
	}

	// 验证旧密码
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(oldPassword)); err != nil {
		return errors.New("旧密码错误")
	}

	// 生成新密码哈希
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	user.PasswordHash = string(hashedPassword)
	if err := s.repo.Update(ctx, user); err != nil {
		return err
	}
	// 改密后 bump 会话纪元：其他已签发 JWT 即时失效（2026-10-05 审计 P2）
	if err := s.repo.BumpSessionEpoch(ctx, userID); err == nil {
		middleware.InvalidateIdentity(userID)
	}
	return nil
}

// UpdateUserStats 更新用户统计信息。
// 用 SQL 表达式原子自增（此前为 GetByID+Update 读改写，并发下丢更新）。
func (s *Service) UpdateUserStats(userID uint, statsType string, value int64) error {
	ctx := context.Background()

	updates := map[string]interface{}{}
	switch statsType {
	case "upload", "uploads":
		updates["total_uploads"] = gorm.Expr("total_uploads + ?", value)
	case "download", "downloads":
		updates["total_downloads"] = gorm.Expr("total_downloads + ?", value)
	case "storage":
		updates["total_storage"] = gorm.Expr("total_storage + ?", value)
	default:
		return nil
	}
	return s.repo.UpdateColumns(ctx, userID, updates)
}

// QuotaExceededError 存储配额超限（handler 侧可断言后返回 CodeStorageQuota）
type QuotaExceededError struct{ Msg string }

func (e *QuotaExceededError) Error() string { return e.Msg }
func (e *QuotaExceededError) ErrCode() int  { return errcode.CodeStorageQuota }

// CheckQuota 存储配额强制检查（上传前调用）。
// 生效优先级：用户级 MaxStorageQuota > 系统默认 user.user_storage_quota；0 = 不限。
// 已用配额以 file_codes 表实时聚合为准（比 users.total_storage 计数器更抗漂移）。
func (s *Service) CheckQuota(ctx context.Context, userID uint, addBytes int64) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		// 用户不存在/查询失败时不阻断上传（统计与配额是运营约束，不是可用性前提）
		return nil
	}
	limit := user.MaxStorageQuota
	if limit <= 0 {
		// 系统默认取管理后台"用户配置"持久化值（无记录回退 yaml）
		limit = s.defaultStorageQuota(ctx)
	}
	if limit <= 0 {
		return nil
	}
	used, err := s.fileCodeRepo.GetTotalSizeByUserID(ctx, userID)
	if err != nil {
		return nil // 统计失败放行，避免存储故障阻断业务
	}
	if used+addBytes > limit {
		return &QuotaExceededError{Msg: fmt.Sprintf(
			"存储配额超限（已用 %d / 上限 %d 字节，本次需 %d 字节）", used, limit, addBytes)}
	}
	return nil
}

// SetStorageQuota 管理员设置用户存储配额（0 = 不限，回退系统默认）
func (s *Service) SetStorageQuota(ctx context.Context, userID uint, quota int64) (*model.UserResp, error) {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	user.MaxStorageQuota = quota
	if err := s.repo.Update(ctx, user); err != nil {
		return nil, err
	}
	return user.ToResp(), nil
}

// SetUploadSize 管理员设置用户单次上传大小上限（0 = 用系统默认）
func (s *Service) SetUploadSize(ctx context.Context, userID uint, size int64) (*model.UserResp, error) {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	user.MaxUploadSize = size
	if err := s.repo.Update(ctx, user); err != nil {
		return nil, err
	}
	return user.ToResp(), nil
}

// GetUploadSizeCap 生效的单次上传大小上限（接线：此前 users.max_upload_size
// 字段存在但上传链路从不读取，管理员给用户降限无实际效果）。
// 优先级：用户级 MaxUploadSize > 系统默认 user.user_upload_size；0 = 不限。
// 用户不存在/查询失败返回 0（不阻断，配额类是运营约束）。
func (s *Service) GetUploadSizeCap(ctx context.Context, userID uint) int64 {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return 0
	}
	if user.MaxUploadSize > 0 {
		return user.MaxUploadSize
	}
	return s.defaultUploadSize(ctx)
}

// GetStats 获取用户统计信息
func (s *Service) GetStats(ctx context.Context, userID uint) (*model.UserStats, error) {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 文件数从 FileCode 表实时统计(替代原 TODO 硬编码 0)
	fileCount, err := s.fileCodeRepo.CountByUserID(ctx, userID)
	if err != nil {
		// 统计失败不阻塞整体返回,记 0
		fileCount = 0
	}

	return &model.UserStats{
		UserID:         user.ID,
		TotalUploads:   user.TotalUploads,
		TotalDownloads: user.TotalDownloads,
		TotalStorage:   user.TotalStorage,
		FileCount:      int(fileCount),
	}, nil
}

// ==================== API Key 相关常量和类型 ====================

const (
	apiKeyPrefix   = "pb_sk_"
	maxUserAPIKeys = 5 // 每个用户最多保留的有效密钥数量
)

// CreateAPIKeyReq 创建 API Key 请求
type CreateAPIKeyReq struct {
	Name          string
	ExpiresAt     *time.Time
	ExpiresInDays *int64
}

// CreateAPIKeyResp 创建 API Key 响应
type CreateAPIKeyResp struct {
	Key    string
	APIKey *APIKeyData
}

// APIKeyData API Key 数据
type APIKeyData struct {
	ID         uint
	Name       string
	Prefix     string
	LastUsedAt *time.Time
	LastUsedIP string
	ExpiresAt  *time.Time
	CreatedAt  *time.Time
	Revoked    bool
}

// ==================== API Key 方法 ====================

// GenerateRandomKey 生成32位随机字符串（16字节 hex，crypto/rand 经 kit/uidgen 统一收口；
// crypto/rand 不可用属进程级灾难，uidgen 以 panic 上抛而非返回 error）
func GenerateRandomKey() string {
	return uidgen.RandomHex(16)
}

// HashAPIKey 计算密钥的 SHA256 哈希
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// GetKeyPrefix 获取密钥的前缀（用于展示）
func GetKeyPrefix(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

// CreateAPIKey 为用户创建新的 API Key
func (s *Service) CreateAPIKey(ctx context.Context, userID uint, req *CreateAPIKeyReq) (*CreateAPIKeyResp, error) {

	// 限制有效密钥数量
	count, err := s.apiKeyRepo.CountActiveByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("统计用户 API Key 失败: %w", err)
	}
	if count >= maxUserAPIKeys {
		return nil, fmt.Errorf("最多只能保留 %d 个有效 API Key，请先删除旧的密钥", maxUserAPIKeys)
	}

	// 处理过期时间
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		if req.ExpiresAt.After(time.Now().Add(-1 * time.Minute)) {
			t := req.ExpiresAt.UTC()
			expiresAt = &t
		}
	} else if req.ExpiresInDays != nil && *req.ExpiresInDays > 0 {
		t := time.Now().UTC().Add(time.Duration(*req.ExpiresInDays) * 24 * time.Hour)
		expiresAt = &t
	}

	// 生成随机密钥
	plainKey := apiKeyPrefix + GenerateRandomKey()
	keyHash := HashAPIKey(plainKey)
	keyPrefix := GetKeyPrefix(plainKey)

	// 归一化名称
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "API Key"
	}
	if len(name) > 100 {
		name = name[:100]
	}

	// 创建记录
	record := &model.UserAPIKey{
		UserID:    userID,
		Name:      name,
		Prefix:    keyPrefix,
		KeyHash:   keyHash,
		ExpiresAt: expiresAt,
		Revoked:   false,
	}

	if err := s.apiKeyRepo.Create(ctx, record); err != nil {
		return nil, fmt.Errorf("保存 API Key 失败: %w", err)
	}

	return &CreateAPIKeyResp{
		Key: plainKey,
		APIKey: &APIKeyData{
			ID:         record.ID,
			Name:       record.Name,
			Prefix:     record.Prefix,
			LastUsedAt: record.LastUsedAt,
			ExpiresAt:  record.ExpiresAt,
			CreatedAt:  &record.CreatedAt,
			Revoked:    record.Revoked,
		},
	}, nil
}

// ListAPIKeys 获取用户的全部 API Key 列表（包含已撤销）
func (s *Service) ListAPIKeys(ctx context.Context, userID uint) ([]*APIKeyData, error) {

	keys, err := s.apiKeyRepo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	result := make([]*APIKeyData, len(keys))
	for i, key := range keys {
		result[i] = &APIKeyData{
			ID:         key.ID,
			Name:       key.Name,
			Prefix:     key.Prefix,
			LastUsedAt: key.LastUsedAt,
			LastUsedIP: key.LastUsedIP,
			ExpiresAt:  key.ExpiresAt,
			CreatedAt:  &key.CreatedAt,
			Revoked:    key.Revoked,
		}
	}

	return result, nil
}

// DeleteAPIKey 撤销指定的 API Key
func (s *Service) DeleteAPIKey(ctx context.Context, userID, id uint) error {

	err := s.apiKeyRepo.RevokeByID(ctx, userID, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errors.New("API Key 不存在或已撤销")
	}
	if err != nil {
		return fmt.Errorf("撤销 API Key 失败: %w", err)
	}
	return nil
}

// ==================== 用户文件列表方法 ====================

// GetUserFiles 获取用户文件列表（支持分页）
func (s *Service) GetUserFiles(ctx context.Context, userID uint, page, pageSize int) (*usermodel.UserFileList, error) {
	fileCodeRepo := s.fileCodeRepo

	// 获取文件列表
	files, total, err := fileCodeRepo.GetFilesByUserIDWithPagination(ctx, userID, page, pageSize)
	if err != nil {
		return nil, fmt.Errorf("获取用户文件列表失败: %w", err)
	}

	// 转换为响应格式
	fileItems := make([]*usermodel.UserFileItem, len(files))
	for i, file := range files {
		// 提取文件名（DisplayName 收口:UUID 名/旧布局/直传 Text 契约）
		fileName := file.DisplayName()
		if fileName == "" && file.FilePath != "" {
			// 从路径中提取文件名
			parts := strings.Split(file.FilePath, "/")
			if len(parts) > 0 {
				fileName = parts[len(parts)-1]
			}
		}

		fileItems[i] = &usermodel.UserFileItem{
			ID:           int64(file.ID),
			Code:         file.Code,
			Prefix:       file.Prefix,
			Suffix:       file.Suffix,
			FileName:     fileName,
			FilePath:     file.FilePath,
			Size:         file.Size,
			ExpiredCount: int32(file.ExpiredCount),
			UsedCount:    int32(file.UsedCount),
			CreatedAt:    file.CreatedAt.Format("2006-01-02 15:04:05"),
			UpdatedAt:    file.UpdatedAt.Format("2006-01-02 15:04:05"),
		}

		// 格式化过期时间
		if file.ExpiredAt != nil {
			fileItems[i].ExpiredAt = file.ExpiredAt.Format("2006-01-02 15:04:05")
		}
	}

	// 计算分页信息
	totalPages := int32((total + int64(pageSize) - 1) / int64(pageSize))
	if totalPages < 1 {
		totalPages = 1
	}

	return &usermodel.UserFileList{
		Files: fileItems,
		Pagination: &usermodel.UserFilePagination{
			Page:       int32(page),
			PageSize:   int32(pageSize),
			Total:      total,
			TotalPages: totalPages,
			HasNext:    int32(page) < totalPages,
			HasPrev:    int32(page) > 1,
		},
	}, nil
}

// RevokeAllAPIKeys 一键吊销用户全部有效 API Key（应急止损），返回吊销数量。
func (s *Service) RevokeAllAPIKeys(ctx context.Context, userID uint) (int64, error) {
	n, err := s.apiKeyRepo.RevokeAllByUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("吊销全部 API Key 失败: %w", err)
	}
	return n, nil
}
