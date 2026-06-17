# onley

本地文件去重工具。通过 MD5 建立文件索引，找出重复文件，支持交互式清理——可在单台机器上使用，也可跨多台机器协同工作。

## 功能特性

- 基于 MD5 的全目录树重复文件检测
- 索引以 SQLite 文件形式存储在本地
- 断点续扫：重新扫描时，大小和修改时间未变的文件会被跳过
- 可配置的 goroutine worker 池并发扫描
- 交互式清理（`clean`）或自动清理（`clean-all`）
- 多机模式：replica 将本地索引与 master 对比，删除本地冗余副本，或将独有文件通过 HTTP 迁移到 master

## 安装

```sh
go install onley/cmd/onley@latest
```

或从源码构建：

```sh
git clone <repo>
cd onley
make build      # 生成 ./onley
```

## 快速上手

```sh
# 扫描目录，建立索引
onley scan ~/Downloads

# 查看重复文件
onley dupes

# 交互式选择保留哪些文件
onley clean

# 或自动保留每组第一个文件，删除其余
onley clean-all

# 查看统计信息
onley stats
```

## 子命令

| 命令 | 说明 |
|---|---|
| `scan <dir>` | 扫描 `dir` 并对每个文件计算 MD5 建立索引 |
| `dupes` | 列出所有重复文件组 |
| `clean` | 交互式：逐组选择要保留的文件 |
| `clean-all` | 非交互式：每组保留第一个文件，删除其余（需一次确认） |
| `stats` | 显示总文件数和重复文件数 |
| `serve` | 启动 master HTTP 服务（用于多机模式） |
| `replica check` | 将本地索引与 master 比对，生成并执行计划 |

## 全局参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-db <path>` | `onley.db` | SQLite 数据库文件路径 |
| `-workers <n>` | `max(1, CPU数-1)` | 并发 hash worker 数量 |

## 扫描

```sh
onley scan /Volumes/data
onley -workers 4 scan /Volumes/data
```

扫描进度按 worker 逐行显示。对同一目录进行第二次扫描时，大小和修改时间未变的文件会被跳过，中断后续扫的开销很低。

## 清理

`clean` 逐组展示重复文件，并询问要保留哪些。输入单个编号、逗号分隔的多个编号，或直接回车跳过该组。

```
── Group 1  MD5: d8e8fca2dc0f896fd7cb4cb0031ba249  size: 4.0 KB ──
  [1] /Users/alice/docs/report.pdf
  [2] /Users/alice/backup/report.pdf
Keep number(s) (e.g. 1 or 1,2; Enter to skip): 1
```

`clean-all` 为非交互模式：每组按路径字母顺序保留第一个文件，一次确认后删除其余文件。

## 多机模式（Replica）

多机模式允许多台机器将各自的独有文件统一汇聚到一台 master 上。

**在 master 机器上**，启动 HTTP 服务：

```sh
onley -db /data/master.db serve -port 8080 -store /data/files
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-port <n>` | `8080` | 监听端口 |
| `-store <dir>` | `onley-store` | 接收迁移文件的存储目录 |

**在每台 replica 机器上**，先扫描本地文件，再执行比对：

```sh
onley -db local.db scan /my/files
onley -db local.db replica check -master http://master-host:8080
```

`replica check` 对本地索引中的每个文件查询 master，生成操作计划：

- **删除本地副本** — master 已有该 MD5，本地文件冗余。
- **迁移到 master** — master 没有该文件，将上传至 master 后删除本地副本。

执行前会完整展示计划，输入 `y` 确认，按回车或输入 `n` 取消。

```
Comparing 1 234 file(s) with master...

Delete locally (already on master, 892 file(s)):
  /my/files/photo_001.jpg
  ...

Migrate to master (not on master, 342 file(s)):
  /my/files/project_final_v3.zip
  ...

Proceed with the above? [y/N]
```

迁移文件通过 HTTP multipart 上传，master 以内容寻址方式存储（路径格式：`<store>/<md5前2位>/<md5后30位>/<文件名>`）。上传成功后，replica 本地文件及其索引记录均被删除。

## Master HTTP API

master 对外暴露一组小型 JSON API，供 `replica check` 内部调用，也可直接访问。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/v1/health` | 返回 `{"ok":true}` |
| `GET` | `/v1/check?md5=<hash>` | 返回 `{"found":bool,"paths":[...]}` |
| `POST` | `/v1/ingest` | Multipart 上传，字段：`file`、`md5` |

## 开发

```sh
make test       # 运行所有测试
make coverage   # 生成覆盖率报告 → coverage.html
make lint       # golangci-lint
make clean      # 删除二进制文件和覆盖率文件
```

运行环境要求：Go 1.22+
