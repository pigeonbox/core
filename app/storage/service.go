package storage

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
	corestorage "github.com/pigeonbox/core/storage"
)

// Runtime 存储运行时接口（bootstrap 注入单例 *storage.StorageService）。
type Runtime interface {
	Reload(cfg *corestorage.StorageConfig) error
	EffectiveType() corestorage.StorageType
}

// RuntimePersister 运行时存储配置持久化（admin 域实现，bootstrap 装配；
// 存储域不直接碰 system_configs，保持域间零耦合）。
type RuntimePersister interface {
	LoadRuntimeStorage(ctx context.Context) *conf.StorageConfig
	SaveRuntimeStorage(ctx context.Context, cfg *conf.StorageConfig) error
}

// Service 存储服务
type Service struct {
	// fallbackConfig 构造时的配置快照，**只在全局快照缺失时**兜底。
	//
	// ⚠️ 2026-10-31：全局配置改为 copy-on-write（原子换指针）后，长期持有
	// conf 快照读取（Snapshot）返回的指针 = 永远持有**旧快照** ⇒ 存储域读到的
	// 永远是热更新前的值（在线切换后端后 GetStorageInfo 仍报旧后端）。
	// ⇒ 改为一律通过 cfg() 取**当前**快照。
	fallbackConfig *conf.AppConfiguration
	runtime        Runtime
	persister      RuntimePersister
	// objectLister 被 DB 引用的对象清单来源（存储域不直接碰 DB；admin 域注入）。
	// 存储迁移（migrate.go）据此枚举待搬对象。
	objectLister ObjectLister
}

// NewService 创建存储服务
func NewService() *Service {
	return &Service{
		fallbackConfig: conf.Snapshot(),
	}
}

// cfg 取当前生效的全局配置快照（copy-on-write 后每次都可能不同）。
//
// 全局快照缺失时回落到构造时的兜底快照（保持旧行为：不会 nil 解引用）。
func (s *Service) cfg() *conf.AppConfiguration {
	if c := conf.Snapshot(); c != nil {
		return c
	}
	return s.fallbackConfig
}

// SetRuntime 注入存储运行时（在线切换后端热生效）
func (s *Service) SetRuntime(rt Runtime) { s.runtime = rt }

// SetPersister 注入持久化（在线切换写穿 DB，重启不丢）
func (s *Service) SetPersister(p RuntimePersister) { s.persister = p }

// baseURL 对外基础地址（server.base_url）。未配置时如实返回空串，不再回退
// 拼 http://server.host:port——host 是监听地址（常为 0.0.0.0），对外不可达，
// 拼出的 URL 是坏链（与 2026-10-07 分享链接 0.0.0.0 事故同根因；share 域
// ResolveBase 明文禁止该回退，同仓两套 base 推导规则的矛盾在此收口）。
// 消费链 ConfigFromConf→StorageConfig.BaseURL→GetFileURL：base 空时显式报错
// （该链路当前无生产消费方），不产虚构地址。
func (s *Service) baseURL() string {
	return s.cfg().Server.BaseURL
}

// candidateFor 复制全局存储段并覆盖目标类型（切换/更新的候选配置）
func (s *Service) candidateFor(t string) *conf.StorageConfig {
	c := &conf.StorageConfig{Type: t, StoragePath: s.cfg().Storage.StoragePath}
	if s.cfg().Storage.S3 != nil {
		cp := *s.cfg().Storage.S3
		c.S3 = &cp
	}
	if s.cfg().Storage.WebDAV != nil {
		cp := *s.cfg().Storage.WebDAV
		c.WebDAV = &cp
	}
	if s.cfg().Storage.FTP != nil {
		cp := *s.cfg().Storage.FTP
		c.FTP = &cp
	}
	if s.cfg().Storage.SFTP != nil {
		cp := *s.cfg().Storage.SFTP
		c.SFTP = &cp
	}
	if s.cfg().Storage.AzureBlob != nil {
		cp := *s.cfg().Storage.AzureBlob
		c.AzureBlob = &cp
	}
	if s.cfg().Storage.HDFS != nil {
		cp := *s.cfg().Storage.HDFS
		c.HDFS = &cp
	}
	if s.cfg().Storage.OneDrive != nil {
		cp := *s.cfg().Storage.OneDrive
		c.OneDrive = &cp
	}
	// 云厂商段（oss/cos/bos/ks3/obs）随 type 归属复制
	for _, seg := range []**conf.CloudStorageConfig{
		&c.OSS, &c.COS, &c.BOS, &c.KS3, &c.OBS,
	} {
		*seg = nil
	}
	switch t {
	case "oss":
		c.OSS = cloneCloud(s.cfg().Storage.OSS)
	case "cos":
		c.COS = cloneCloud(s.cfg().Storage.COS)
	case "bos":
		c.BOS = cloneCloud(s.cfg().Storage.BOS)
	case "ks3":
		c.KS3 = cloneCloud(s.cfg().Storage.KS3)
	case "obs":
		c.OBS = cloneCloud(s.cfg().Storage.OBS)
	}
	return c
}

// cloneCloud 浅拷贝厂商段
func cloneCloud(src *conf.CloudStorageConfig) *conf.CloudStorageConfig {
	if src == nil {
		return nil
	}
	cp := *src
	return &cp
}

// validateEndpoints 远端端点 SSRF 校验（scheme 白名单 + 私网策略）。
// 2026-10-05 审计 P2 补全：云厂商显式 endpoint 与 FTP/SFTP host 此前不在
// 校验范围，管理员可配 oss.endpoint=http://127.0.0.1:6379 借 Probe 做内网探测。
func validateEndpoints(c *conf.StorageConfig) error {
	if c.S3 != nil && c.S3.Endpoint != "" {
		if err := security.ValidateEndpointURL(c.S3.Endpoint); err != nil {
			return fmt.Errorf("S3 endpoint 校验失败: %w", err)
		}
	}
	if c.WebDAV != nil && c.WebDAV.Endpoint != "" {
		if err := security.ValidateEndpointURL(c.WebDAV.Endpoint); err != nil {
			return fmt.Errorf("WebDAV endpoint 校验失败: %w", err)
		}
	}
	if c.HDFS != nil && c.HDFS.Endpoint != "" {
		if err := security.ValidateEndpointURL(c.HDFS.Endpoint); err != nil {
			return fmt.Errorf("HDFS endpoint 校验失败: %w", err)
		}
	}
	if c.AzureBlob != nil && c.AzureBlob.Endpoint != "" {
		if err := security.ValidateEndpointURL(c.AzureBlob.Endpoint); err != nil {
			return fmt.Errorf("AzureBlob endpoint 校验失败: %w", err)
		}
	}
	for name, seg := range map[string]*conf.CloudStorageConfig{
		"OSS": c.OSS, "COS": c.COS, "BOS": c.BOS, "KS3": c.KS3, "OBS": c.OBS,
	} {
		if seg != nil && seg.Endpoint != "" {
			if err := security.ValidateEndpointURL(seg.Endpoint); err != nil {
				return fmt.Errorf("%s endpoint 校验失败: %w", name, err)
			}
		}
	}
	if c.FTP != nil && c.FTP.Host != "" {
		if err := security.ValidateEndpointHost(c.FTP.Host); err != nil {
			return fmt.Errorf("FTP host 校验失败: %w", err)
		}
	}
	if c.SFTP != nil && c.SFTP.Host != "" {
		if err := security.ValidateEndpointHost(c.SFTP.Host); err != nil {
			return fmt.Errorf("SFTP host 校验失败: %w", err)
		}
	}
	return nil
}

// activate 存储切换公共流程：SSRF 校验 → 认证级 Probe → 热重载 → 提交全局配置 → 持久化。
// Probe/Reload 任一失败都不改动现有配置（内存与运行时保持原后端）。
func (s *Service) activate(ctx context.Context, candidate *conf.StorageConfig) error {
	if err := validateEndpoints(candidate); err != nil {
		return err
	}
	target := corestorage.ConfigFromConf(candidate, s.baseURL())
	if err := corestorage.ProbeConfig(ctx, target); err != nil {
		return fmt.Errorf("存储连通性验证失败: %w", err)
	}
	if s.runtime != nil {
		if err := s.runtime.Reload(target); err != nil {
			return fmt.Errorf("切换存储后端失败: %w", err)
		}
	}
	// ⚠️ **copy-on-write**（2026-10-31 更正）：此前这里是**原地改写**全局 conf
	// 的四个字段（Type / StoragePath / S3 / WebDAV，分四次赋值）⇒
	// 并发读者可能看到**撕裂的组合**（如 Type 已是 s3、而 S3 还是旧值）
	// ⇒ 「读到的存储配置」与「实际生效的存储」对不上。
	//
	// 在线切换存储是管理员操作（不是启动竞态窗口）⇒ **必然发生**。
	//
	// 现在整段替换 + 原子换指针：读者要么看到完整旧快照、要么看到完整新快照。
	conf.UpdateGlobalConfig(func(next *conf.AppConfiguration) {
		next.Storage.Type = candidate.Type
		next.Storage.StoragePath = candidate.StoragePath
		next.Storage.S3 = candidate.S3
		next.Storage.WebDAV = candidate.WebDAV
	})
	if s.persister != nil {
		if err := s.persister.SaveRuntimeStorage(ctx, candidate); err != nil {
			return fmt.Errorf("存储已生效但持久化失败（重启后可能回退到原配置）: %w", err)
		}
	}
	return nil
}

// GetStorageInfo 获取存储信息
func (s *Service) GetStorageInfo(ctx context.Context) (*StorageInfo, error) {
	// 获取可用存储类型
	availableStorages := []string{
		"local", "s3", "oss", "cos", "bos", "ks3", "obs", "gcs", "webdav",
		"ftp", "sftp", "azureblob", "hdfs", "onedrive",
	}
	if s.cfg().Storage.Type != "" {
		availableStorages = append(availableStorages, "local")
	}

	// 获取各存储类型的详细信息
	storageDetails := make(map[string]*StorageDetail)

	// 本地存储详情
	storageDetails["local"] = &StorageDetail{
		Type:         "local",
		Available:    true,
		StoragePath:  s.getStoragePath(),
		UsagePercent: s.getDiskUsage(),
	}

	// 当前存储类型
	currentType := s.cfg().Storage.Type
	if currentType == "" {
		currentType = "local"
	}

	// 存储配置
	storageConfig := s.getStorageConfig()

	// 实际生效的后端（远端驱动构造失败降级 local 时与 Current 不同）
	effective := currentType
	if s.runtime != nil {
		effective = string(s.runtime.EffectiveType())
	}

	return &StorageInfo{
		Current:        currentType,
		Effective:      effective,
		Available:      availableStorages,
		StorageDetails: storageDetails,
		StorageConfig:  storageConfig,
	}, nil
}

// SwitchStorage 切换存储类型：校验 → 认证级 Probe → 热重载（新写入立即走新后端）
// → 更新全局配置 → 持久化到 system_configs（重启不丢）。
func (s *Service) SwitchStorage(ctx context.Context, storageType string) error {
	switch storageType {
	case "local", "s3", "webdav", "oss", "cos", "bos", "ks3", "obs", "gcs",
		"ftp", "sftp", "azureblob", "hdfs", "onedrive":
	default:
		return fmt.Errorf("不支持的存储类型: %s", storageType)
	}
	return s.activate(ctx, s.candidateFor(storageType))
}

// TestStorageConnection 测试存储连接。
// local：路径存在可写；s3/webdav：端点 URL 校验（scheme 白名单 + 私网策略）
// 后做真实 HTTP 探测（不要求认证通过，能建立连接即可）。
func (s *Service) TestStorageConnection(ctx context.Context, storageType string) error {
	if storageType == "" {
		return fmt.Errorf("存储类型不能为空")
	}
	if storageType == "local" {
		path := s.getStoragePath()
		if path == "" {
			return fmt.Errorf("存储路径未配置")
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("存储路径不存在: %s", path)
		}
		testFile := path + "/.test_write"
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			return fmt.Errorf("存储路径不可写: %w", err)
		}
		_ = os.Remove(testFile)
		return nil
	}
	// 远端类型：按既有配置构造驱动，走认证级 Probe（凭据/可达性在此暴露，
	// 自动覆盖 s3/webdav/ftp/sftp/gcs/azureblob/hdfs/onedrive 与云厂商预设）
	candidate := s.candidateFor(storageType)
	return corestorage.BuildAndProbe(ctx, corestorage.ConfigFromConf(candidate, s.baseURL()))
}

// probeHTTP 通用端点探测：能建立 TLS/HTTP 连接即视为可达（401/403/404 都算通）。
func probeHTTP(ctx context.Context, endpoint string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("端点不可达: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

// probeWebDAV WebDAV 探测：OPTIONS 请求 + 基本认证
func probeWebDAV(ctx context.Context, endpoint, username, password string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, endpoint, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("WebDAV 端点不可达: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("WebDAV 认证失败（401），请检查用户名/密码")
	}
	return nil
}

// UpdateStorageConfig 更新存储配置。
// 流程与 SwitchStorage 一致：SSRF 校验 → 认证级 Probe → 热重载 → 更新内存
// → 持久化到 system_configs（管理端配置记录，重启后由 bootstrap 恢复）。
// 注：use_ssl/path_style 未随请求传入时保留现值（use_ssl 缺省 true，与管理端历史行为一致）。
func (s *Service) UpdateStorageConfig(ctx context.Context, req *UpdateConfigRequest) error {
	// 请求体即完整候选配置——校验 → Probe → 热切换 → 持久化。
	// 旧嵌套形态 {type, config:{...}} 已删（2026-10-25 删除旧版本兼容）。
	// JSON 冲突注意：顶层 Type（深度 0，tag "type"）会赢过内嵌 StorageConfig.Type
	// （深度 1）——类型以 req.Type 兜底为准。
	req.StorageConfig.Type = req.Type
	candidate := req.StorageConfig
	if candidate.StoragePath == "" {
		candidate.StoragePath = s.cfg().Storage.StoragePath
	}
	if err := validateEndpoints(&candidate); err != nil {
		return err
	}
	return s.activate(ctx, &candidate)
}

// getStoragePath 获取存储路径
func (s *Service) getStoragePath() string {
	if s.cfg().Storage.StoragePath != "" {
		return s.cfg().Storage.StoragePath
	}
	if s.cfg().App.DataPath != "" {
		return s.cfg().App.DataPath
	}
	return "./data"
}

// getDiskUsage 获取磁盘使用率（0-100，statfs 真实计算；失败返回 0）
func (s *Service) getDiskUsage() int32 {
	total, free, err := diskUsage(s.getStoragePath())
	if err != nil || total <= 0 {
		return 0
	}
	used := total - free
	return int32(used * 100 / total)
}

// diskUsage 返回路径所在文件系统的 (总空间, 可用空间, error)
func diskUsage(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	return total, free, nil
}

// getStorageConfig 获取存储配置
func (s *Service) getStorageConfig() *StorageConfig {
	return &StorageConfig{
		Type:        s.cfg().Storage.Type,
		StoragePath: s.getStoragePath(),
		WebDAV:      s.getWebDAVConfig(),
		S3:          s.getS3Config(),
		NFS:         s.getNFSConfig(),
	}
}

// getWebDAVConfig 获取 WebDAV 配置（来自全局配置）
func (s *Service) getWebDAVConfig() *WebDAVConfig {
	if s.cfg().Storage.WebDAV != nil {
		return &WebDAVConfig{
			URL:      s.cfg().Storage.WebDAV.Endpoint,
			Username: s.cfg().Storage.WebDAV.Username,
			Password: s.cfg().Storage.WebDAV.Password,
		}
	}
	return &WebDAVConfig{}
}

// getS3Config 获取 S3 配置（来自全局配置）
func (s *Service) getS3Config() *S3Config {
	if s.cfg().Storage.S3 != nil {
		return &S3Config{
			EndpointURL:     s.cfg().Storage.S3.Endpoint,
			RegionName:      s.cfg().Storage.S3.Region,
			BucketName:      s.cfg().Storage.S3.Bucket,
			AccessKeyID:     s.cfg().Storage.S3.AccessKey,
			SecretAccessKey: s.cfg().Storage.S3.SecretKey,
		}
	}
	return &S3Config{}
}

// getNFSConfig 获取 NFS 配置。
// NFS 不是受支持的存储后端（14 种后端清单见 storage/backends.go 注册表，
// 无 nfs scheme；conf 亦无 NFS 配置段）——本结构是管理面展示层的预留占位，
// 恒返回空值如实表达"未配置/不支持"，接入真实 NFS 后端时再行填充。
func (s *Service) getNFSConfig() *NFSConfig {
	return &NFSConfig{
		Server:     "",
		Path:       "",
		MountPoint: "",
		Version:    "",
		Options:    "",
		Timeout:    0,
		AutoMount:  0,
		RetryCount: 0,
		SubPath:    "",
	}
}

// ==================== 响应模型 ====================

type StorageInfo struct {
	Current        string                    `json:"current"`
	Effective      string                    `json:"effective"` // 实际生效后端（降级时与 current 不同）
	Available      []string                  `json:"available"`
	StorageDetails map[string]*StorageDetail `json:"storage_details"`
	StorageConfig  *StorageConfig            `json:"storage_config"`
}

type StorageDetail struct {
	Type         string `json:"type"`
	Available    bool   `json:"available"`
	StoragePath  string `json:"storage_path"`
	UsagePercent int32  `json:"usage_percent"`
	Error        string `json:"error,omitempty"`
}

type StorageConfig struct {
	Type        string        `json:"type"`
	StoragePath string        `json:"storage_path"`
	WebDAV      *WebDAVConfig `json:"webdav,omitempty"`
	S3          *S3Config     `json:"s3,omitempty"`
	NFS         *NFSConfig    `json:"nfs,omitempty"`
}

type WebDAVConfig struct {
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Password string `json:"password"`
	RootPath string `json:"root_path"`
	URL      string `json:"url"`
}

type S3Config struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	BucketName      string `json:"bucket_name"`
	EndpointURL     string `json:"endpoint_url"`
	RegionName      string `json:"region_name"`
	Hostname        string `json:"hostname"`
	Proxy           string `json:"proxy"`
}

type NFSConfig struct {
	Server     string `json:"server"`
	Path       string `json:"path"`
	MountPoint string `json:"mount_point"`
	Version    string `json:"version"`
	Options    string `json:"options"`
	Timeout    int32  `json:"timeout"`
	AutoMount  int32  `json:"auto_mount"`
	RetryCount int32  `json:"retry_count"`
	SubPath    string `json:"sub_path"`
}

// UpdateConfigRequest 更新配置请求
// UpdateConfigRequest 存储配置更新请求。
//
// 形态：**只有**扁平一种——请求体即 conf.StorageConfig 同构 JSON
// （type/storage_path/quota/s3/webdav/ftp/sftp/azureblob/hdfs/onedrive/oss..obs），
// 后端整体校验 → Probe → 热切换 → 持久化。
//
// 旧的嵌套形态 {type, config:{storage_path/webdav/s3}} 已删（2026-10-25 删除旧版本
// 兼容）：它只能切 local/webdav/s3 三种、且字段名与实际配置模型对不上（旧契约遗留），
// 保留它等于让**两种写入口径**并存。
type UpdateConfigRequest struct {
	Type string `json:"type"`
	// 扁平形态：内嵌 conf.StorageConfig，JSON 字段同名绑定
	conf.StorageConfig
}
