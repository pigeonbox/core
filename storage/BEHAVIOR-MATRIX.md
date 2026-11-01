# 存储驱动行为矩阵（跨后端一致性）

> 最后更新：2026-11-02（core#36 真机联调）
> 口径：**真服务实测**，不是读代码推断。取证方式见本文件末尾「取证方式」。

## 为什么有这份矩阵

同一套 `storage/opendal.Operator` 契约要盖住 10 个后端（S3 系 7 个 + WebDAV / FTP /
SFTP / Azure Blob / HDFS / OneDrive）。**契约相同、行为不同**的地方是缺陷温床：
调用方按 S3 的心智写代码，换个后端就静默出错。

2026-11-02 的真机联调（core#36）实测出 **4 个真实缺陷**，全部属于「单测全绿、真机不可用」：

| # | 后端 | 缺陷 | 后果 | 状态 |
|---|---|---|---|---|
| 1 | Azure Blob | `containerURL` 在 SharedKey 模式下不补 `?` | `Probe`/`List`/`RemoveAll` 全线 400（**blob 级读写看起来正常** ⇒ 症状极隐蔽） | ✅ 已修 |
| 2 | Azure Blob | canonicalized resource 的查询参数用 URL 的 `name=value` 而非规范的 `name:value` | 含查询参数的请求签名不匹配 ⇒ 403 AuthorizationFailure | ✅ 已修 |
| 3 | Azure Blob | `listAll` 不查 HTTP 状态码 | 鉴权失败被当成「空列表」静默吞掉 ⇒ 后端已坏而调用方不知情 | ✅ 已修 |
| 4 | HDFS | `List` 对不存在的目录返回 404 错误，而 S3/Azure 同语义是**空列表** | 后台 janitor 的 presign 孤儿清理（`ListRemoteObjects("uploads/")`）在 HDFS 后端整个任务报错退出 | ✅ 已修 |

## 契约矩阵

「期望」列 = S3 系（对象存储的事实标准）的行为；本仓以它为准绳。

| 能力 | S3 系（期望） | WebDAV | FTP | SFTP | Azure Blob | HDFS | OneDrive |
|---|---|---|---|---|---|---|---|
| `Write`/`Read` 往返 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `Stat` 缺失 ⇒ `os.ErrNotExist` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `List` 缺失前缀 ⇒ **空列表、无错** | ✅ | ❌¹ | ❌¹ | ❌¹ | ✅ | ✅ | ✅ |
| `List` 的 `Path` = 相对所列目录的名字 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `RemoveAll` 空 key ⇒ 拒绝 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `Copy` / `Rename` | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| `ReadRange`（区间读） | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| `Presign`（真预签名 URL） | ✅ | ❌（回退自家 URL） | ❌ | ❌ | ❌ | ❌ | ❌ |

¹ **WebDAV/FTP/SFTP 对缺失目录返回错误（`os.ErrNotExist`），不是空列表** —— 这是**对的**：
这三个后端有**真目录**，目录不存在就该报错。S3 系与 Azure Blob 是**无目录**的对象存储，
「前缀不存在」=「没东西」⇒ 空列表。
⇒ 所以 `List` 的空列表语义只对**对象存储型**后端成立；调用方若要跨后端统一，
须把「目录不存在」按后端类型区分对待（HDFS 属**有真目录**但本次对齐到了对象存储语义，
见下方说明）。

**HDFS 为何对齐到「空列表」**：HDFS 有真目录，但本仓 `List` 的调用方是
`StorageService.ListRemoteObjects(prefix)`（后台 janitor 的 presign 孤儿清理），
它列举的是**对象前缀**而非目录 ⇒ 前缀不存在 = 没有孤儿对象 = 空集。
原先 HDFS 返回 404 ⇒ janitor 整个任务报错退出（而 S3 后端正常返回空集、无事可做）
⇒ **孤儿清理在 HDFS 后端下不可用**。对齐后行为与 S3 一致。

### 「❌」的确切语义（不是「坏了」，是「该能力不存在」）

- **`Copy`/`Rename` 非 S3 后端**：`Operator` 返回 `copy not implemented for scheme: <x>` /
  `rename not implemented for scheme: <x>`（显式报错，不静默）。
  业务影响：换用非 S3 后端时，「复制分享」「改名分享」这类操作不可用。
  ⚠️ 注意 `S3.Rename` 的实现是 `Copy` + `Delete`（非服务端原子改名）。
- **`ReadRange` 非 S3 后端**：`Operator.ReadRange` 返回 `ErrRangeUnsupported`
  （`storage driver: range read unsupported`）。业务影响：**HTTP Range 下载 / 断点续传
  在非 S3 后端不可用** ⇒ `StorageService.GetFileReaderRange` 把该错误上抛，
  调用方须回退到全量流式（200）。这是**已知的能力缺口**，不是本次修的缺陷。
- **`Presign` 非 S3 后端**：回退为「自家直传 URL」（`/api/v1/presign/upload-direct`），
  走自家服务中转 ⇒ 客户端不能直传存储后端。S3 系是真预签名 URL（客户端直传直下）。
  ⚠️ `Presign` 在**任何**后端都不会返回「不支持」错误（`op.presignEnabled=false` 时
  一律回退），所以**别把「Presign 返回成功」当成「这个后端支持直传」**。

### 「✅ 但要注意」的差异

- **Azure Blob 的 `List` 返回的 `Size` 恒为 0**（容器级列举只给名字，不给大小）；
  S3/HDFS 给真实大小。业务影响：依赖 `List` 拿文件大小的代码在 Azure 上会拿到 0
  ⇒ **必须用 `Stat` 取大小**。
- **Azure Blob 无目录语义**：`List` 把前缀下的第一层当作「目录条目」返回
  （`Path` 带尾部 `/`、`IsDir=true`）；HDFS 有真目录。
- **HDFS 的 `Delete` 对非空目录 ⇒ 403**（`PathIsNotEmptyDirectoryException`）；
  S3/Azure 没有目录 ⇒ 无此概念。需要删目录 ⇒ 用 `RemoveAll`（递归）。
- **HDFS 的 `Read` 对目录 ⇒ 404**（把目录当文件读）。
- **`os.ErrNotExist` 判定用 `errors.Is`，不要用 `os.IsNotExist`**：
  驱动用 `fmt.Errorf("%w: ...", os.ErrNotExist)` 包装 ⇒ `errors.Is` 成立，
  但 `os.IsNotExist` **不解包 `%w` 链** ⇒ 返回 false。
  ⚠️ 仓内 `storage/storage.go` 的本地分支用的是 `os.IsNotExist`（本地路径由 `os.Stat`
  返回 `*PathError`，那里 `os.IsNotExist` 成立）⇒ **两处口径不同是有原因的，别顺手统一**。

## 后端能力速查（选型参考）

| 后端 | 需要真机/凭据 | 直传（真预签名） | 区间读 | 适用场景 |
|---|---|---|---|---|
| S3 / OSS / COS / BOS / KS3 / OBS / GCS | 账号 + 桶 | ✅ | ✅ | 公有云对象存储（默认首选） |
| WebDAV / FTP / SFTP | 服务地址 | ❌（中转） | ❌ | NAS / 内网文件服务 |
| Azure Blob | 账号 + 容器（或 SAS） | ❌（中转） | ❌ | Azure 生态 |
| HDFS | NameNode WebHDFS 地址 | ❌（中转） | ❌ | Hadoop 集群 |
| OneDrive | Azure AD 应用 + refresh_token | ❌（中转） | ❌ | Microsoft 生态 |

## 取证方式

本矩阵的每一行都来自**真服务实测**，不是读代码推断：

- **Azure Blob** ⇒ Azurite（微软官方模拟器，走**真** Azure Blob REST 协议）：
  `scripts/live-storage-gate.sh` 起 `npx azurite`，用驱动的 SharedKey 签名直连。
- **HDFS** ⇒ `apache/hadoop:3.3.6` 真单节点 + 真 WebHDFS（`scripts/live-storage-services.sh`）。
- **门禁** ⇒ `scripts/live-storage-gate.sh`（真服务不可达即红；SKIP 不许冒充通过）。
- **门禁有没有牙齿** ⇒ `scripts/check-live-storage-mutation.py`
  （4 条变异逐条注入，**每条都必须让门禁转红**；漏网即退出码 1）。
- **用例** ⇒ `storage/opendal/live_azure_test.go` / `live_hdfs_test.go`
  （`//go:build live`，真服务不可达时显式 SKIP）。

本地跑：

```bash
bash scripts/live-storage-gate.sh          # 起真服务 + 跑用例 + 收尾
python3 scripts/check-live-storage-mutation.py   # 变异自证（慢：每条变异起一轮真服务）
```
