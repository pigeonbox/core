package dao

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

type UserRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewUserRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewUserRepository(gormDB *gorm.DB) *UserRepository {
	return &UserRepository{conn: gormDB}
}

func (r *UserRepository) db() *gorm.DB {
	return r.conn
}

func (r *UserRepository) Create(ctx context.Context, user *model.User) error {
	return r.db().WithContext(ctx).Create(user).Error
}

func (r *UserRepository) Update(ctx context.Context, user *model.User) error {
	return r.db().WithContext(ctx).Save(user).Error
}

// UpdateColumns 按列原子更新（统计自增等并发场景，替代读改写）
func (r *UserRepository) UpdateColumns(ctx context.Context, id uint, updates map[string]interface{}) error {
	return r.db().WithContext(ctx).Model(&model.User{}).Where("id = ?", id).Updates(updates).Error
}

// CreateFirstAdminIfNoAdmin 事务内"无管理员才创建"——setup 初始化的原子防护
// （2026-10-05 审计 P2：原 check-then-act 两步间并发窗口可抢建管理员）。
// 返回是否真的创建了（false=已有管理员，调用方回"系统已初始化"）。
func (r *UserRepository) CreateFirstAdminIfNoAdmin(ctx context.Context, admin *model.User) (bool, error) {
	created := false
	err := r.db().Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.User{}).Where("role = ?", "admin").Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		if err := tx.Create(admin).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

// BumpSessionEpoch 会话纪元 +1（原子自增）：使该用户全部已签发 JWT 即时失效。
// 改密（本人/管理员重置）、封禁/停用、角色变更时调用。
func (r *UserRepository) BumpSessionEpoch(ctx context.Context, id uint) error {
	return r.db().Model(&model.User{}).Where("id = ?", id).
		Update("session_epoch", gorm.Expr("session_epoch + 1")).Error
}

// UpdatePasswordHash 管理员重置密码（直接写 bcrypt 哈希）
func (r *UserRepository) UpdatePasswordHash(ctx context.Context, id uint, passwordHash string) error {
	return r.db().WithContext(ctx).Model(&model.User{}).Where("id = ?", id).
		Update("password_hash", passwordHash).Error
}

// UserFilter 用户列表筛选（管理端）
type UserFilter struct {
	Keyword  string // 匹配 username/email/nickname
	Status   string // active/inactive/banned；空 = 全部
	Role     string // admin/user；空 = 全部
	Page     int
	PageSize int
}

// ListFiltered 带筛选的用户分页列表（管理端用户管理）
func (r *UserRepository) ListFiltered(ctx context.Context, f UserFilter) ([]*model.User, int64, error) {
	page, pageSize := normalizePage(f.Page, f.PageSize)
	q := r.db().WithContext(ctx).Model(&model.User{})
	if f.Keyword != "" {
		like, esc := LikeContains(f.Keyword)
		q = q.Where("username LIKE ? OR email LIKE ? OR nickname LIKE ? "+esc, like, like, like)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Role != "" {
		q = q.Where("role = ?", f.Role)
	}
	return paginate[model.User](q.Order("created_at DESC"), page, pageSize)
}

func (r *UserRepository) Delete(ctx context.Context, id uint) error {
	return r.db().WithContext(ctx).Delete(&model.User{}, id).Error
}

func (r *UserRepository) GetByID(ctx context.Context, id uint) (*model.User, error) {
	var user model.User
	err := r.db().WithContext(ctx).First(&user, id).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *UserRepository) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	var user model.User
	err := r.db().WithContext(ctx).Where("username = ?", username).First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	var user model.User
	err := r.db().WithContext(ctx).Where("email = ?", email).First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetByOIDCSub 按 OIDC subject 精确匹配（P2 SSO）
func (r *UserRepository) GetByOIDCSub(ctx context.Context, sub string) (*model.User, error) {
	var user model.User
	err := r.db().WithContext(ctx).Where("oidc_sub = ?", sub).First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *UserRepository) List(ctx context.Context, page, pageSize int) ([]*model.User, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	return paginate[model.User](r.db().WithContext(ctx).Model(&model.User{}), page, pageSize)
}

// Count returns the total number of users
func (r *UserRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.User{}).Count(&count).Error
	return count, err
}

// CountAdminUsers returns the total number of admin users
func (r *UserRepository) CountAdminUsers(ctx context.Context) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.User{}).Where("role = ?", "admin").Count(&count).Error
	return count, err
}
