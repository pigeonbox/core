// Package backup 统一备份包（2026-10-31 起）：换机器 / 换后端 / 灾备三件事一个工具。
//
// # 为什么要有
//
// 此前「把整套实例搬到另一台机器」只有两条路，且都**不可靠**：
//
//	· 手工拷 data 目录 ⇒ 拷到一半、DB 与对象对不上 ⇒ 恢复后一半分享取不到文件；
//	· 只拷 DB ⇒ 对象全丢 ⇒ 所有文件分享失效。
//
// ⇒ 这里定义**一个格式**，把「DB 快照 + 文件清单 + 存储对象 + 校验」打成**一个包**，
//
//	导出与恢复都是同一套代码 ⇒ 灾备演练可以在隔离实例上真的跑一遍。
//
// # 备份包格式 v1（tar.gz）
//
//	pigeonbox-backup.tar.gz
//	├── manifest.json      格式版本 + 组件版本 + 对象清单（逐个 sha256）
//	├── db/backup.db       数据库**一致快照**（SQLite：VACUUM INTO）
//	├── files.json         分享/取件码清单（独立核对用 ⇒ 链接不变的可证据）
//	└── objects/<相对路径>  存储对象（路径与 file_codes.file_path 同形）
//
// # 三条语义（缺一条就危险）
//
//  1. **取件码与分享链接不变**：DB 快照原样带走（含 file_codes / file_code_files /
//     system_configs）⇒ 恢复后用户的对外名片（码 / 链接）照旧可用。
//     files.json 是这份承诺的**独立副本**，便于事后逐条核对。
//  2. **恢复是破坏性操作**：它会替换 DB 与对象 ⇒ 必须**显式确认**（Confirm），
//     且必须**停服**后执行（DB 文件被替换时，运行中的实例会读到半旧半新的数据）。
//  3. **导出/恢复全程不碰取件次数**：这是运维动作，不是取件动作。
//
// # 为什么导出/恢复是「离线子命令」而不是 HTTP 端点
//
// 导出/恢复天然是**同机**操作（要读本地 DB 与本地存储），做成 HTTP 端点反而多一层
// 「服务端正在运行 ⇒ 恢复会砸自己的脚」的风险；而 `pb` 是**纯 HTTP 客户端**
// （不链接 core），要让 `pb backup` 能落地就得把 core 拖进这个轻量 CLI（体积 +
// 数十 MB）。⇒ 形态定为**server 二进制的离线子命令**（`pigeonbox backup export` /
// `pigeonbox restore`），与「恢复必须停服」的事实一致。
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// FormatVersion 当前备份包格式版本。
//
// **格式本身按版本演进**（规划稿要求）：格式一变就 +1，老包用老版本号解析
// ⇒ 恢复时遇到**不认识**的版本必须**拒绝**（宁可报错也不猜着还原）。
const FormatVersion = 1

// 备份包内固定文件名（manifest 的读取方据此定位）。
const (
	ManifestName   = "manifest.json"
	DBEntryName    = "db/backup.db"
	FileListName   = "files.json"
	ObjectDirName  = "objects/"
	UnsupportedFmt = "不认识的备份包格式版本"
)

// Manifest 备份包清单（manifest.json）。
//
// Objects 逐个带 sha256 ⇒ 恢复前可**逐字节校验**，杜绝「包在传输中损坏/被截断」
// 却照样落盘。
type Manifest struct {
	// BackupFormat 格式版本（见 FormatVersion）。
	BackupFormat int `json:"backup_format"`
	// CreatedAt 导出时间（RFC3339）。
	CreatedAt string `json:"created_at"`
	// AppVersion 导出时的应用版本（构建版本）。
	AppVersion string `json:"app_version"`
	// CoreVersion 导出时的 core 版本（构建版本）。
	CoreVersion string `json:"core_version"`
	// DBType 数据库类型（当前仅 sqlite 支持快照；见 ErrUnsupportedDB）。
	DBType string `json:"db_type"`
	// StorageType 导出时的存储后端类型（local / s3 / …）。
	StorageType string `json:"storage_type"`
	// ObjectCount / ObjectBytes 对象数与总字节。
	ObjectCount int64 `json:"object_count"`
	ObjectBytes int64 `json:"object_bytes"`
	// FileCount 分享/取件码记录数。
	FileCount int64 `json:"file_count"`
	// Objects 对象清单（逐个 sha256）。
	Objects []ObjectEntry `json:"objects"`
	// MissingObjects **DB 引用了但磁盘上已经没有**的对象（存量数据本身缺失）。
	//
	// 为什么要单独记：真机实录（2026-10-31，10.10.10.3）——DB 里有 300+ 条分享记录，
	// 其中若干对象文件早被清理 ⇒ 若「读不到就整体失败」，**备份包在这台实例上根本导不出来**
	// ⇒ 灾备能力在最需要它的机器上反而不可用。
	// ⇒ 改成：缺失对象**跳过并如实记账**（不是静默吞掉）；其余故障（权限/网络/驱动错）
	// 仍然整体报错 —— 真故障不能吞。
	MissingObjects []string `json:"missing_objects"`
}

// ObjectEntry 备份包内的一个存储对象。
type ObjectEntry struct {
	// Path 存储相对路径（与 file_codes.file_path 同形 ⇒ 恢复时原样写回）。
	Path string `json:"path"`
	// Size 字节数。
	Size int64 `json:"size"`
	// SHA256 内容摘要（hex）⇒ 恢复前逐个校验。
	SHA256 string `json:"sha256"`
}

// FileEntry files.json 的一条分享记录（**独立核对用**的「链接不变」证据）。
type FileEntry struct {
	ID         uint   `json:"id"`
	Code       string `json:"code"`
	FilePath   string `json:"file_path"`
	Size       int64  `json:"size"`
	Status     string `json:"status"`
	ExpiredAt  string `json:"expired_at"`
	UsedCount  int    `json:"used_count"`
	UUIDFileNm string `json:"uuid_file_name"`
}

// FileList files.json 的结构。
type FileList struct {
	// Count 记录条数。
	Count int `json:"count"`
	// Files 分享记录。
	Files []FileEntry `json:"files"`
}

// ErrUnsupportedDB 非 SQLite 库无法做**一致性快照**（VACUUM INTO 是 SQLite 专有）。
//
// 不做「降级成逐表导出」：那种包恢复起来语义含糊（自增、索引、触发器都可能丢）
// ⇒ 宁可明确报错，让用户用 pg_dump/mysqldump，再走「换库」那条专门的流程。
var ErrUnsupportedDB = fmt.Errorf("当前仅支持 SQLite 的一致性快照；PG/MySQL 库请用 pg_dump/mysqldump")

// ErrRestoreNeedsConfirm 恢复未显式确认（破坏性操作必须显式确认）。
var ErrRestoreNeedsConfirm = fmt.Errorf("恢复会替换数据库与对象，属破坏性操作：须显式确认")

// sha256Hex 计算内容摘要（hex）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// safeEntryName 校验备份包内的条目名，返回**规范化**后的相对路径。
//
// # 为什么必须校验
//
// tar 条目名是**攻击面**：一个名为 `../../etc/cron.d/x` 的条目在解压时就会写到
// 打包目录之外（路径穿越）⇒ 恢复一个来路不明的包 = 往机器上写任意文件。
// ⇒ 规则：必须非空、**不得绝对路径**、不得含 `..` 段、不得是 `.`。
func safeEntryName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("备份包含空条目名")
	}
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", fmt.Errorf("备份包含非法条目名（绝对路径/反斜杠）: %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("备份包含非法条目名（路径穿越）: %q", name)
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", fmt.Errorf("备份包含非法条目名（路径穿越）: %q", name)
		}
	}
	return clean, nil
}
