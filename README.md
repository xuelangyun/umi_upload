# UMI 数据中心（Go 单文件版）

基于 Go 重构的 UMI 采集数据管理与上传中心。存储类型由命名配置显式选择，默认 **阿里云 OSS** 使用官方 `github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss` SDK v2；只有手动选择 MinIO / S3 时才使用 `minio-go/v7`。多个配置可独立保存和切换。已移除外部 `ossutil.exe` 与 Python 运行时依赖，前端静态资源通过 `go:embed` 内嵌至单个可执行文件 `umi_upload.exe`，拷贝到 Windows 机器即可直接双击启动；EXE 内嵌专用云存储上传图标，不使用 Go 默认图标。

---

## 一、Windows 直接启动

1. **直接双击 `umi_upload.exe`**（或双击 `start.bat`）：
   - 自动在同级目录创建/升级 `umi_web.db`（兼容原版 v4/v5/v6 SQLite 数据库，可直接无缝沿用旧 `umi_web.db`）；
   - 自动监听 `http://127.0.0.1:8765` 并打开系统默认浏览器；
   - 关闭控制台窗口或点击页面右上角「退出」即可安全落盘停止。
2. 命令行可选参数：
   ```bat
   umi_upload.exe --port 8765 --host 127.0.0.1 --db umi_web.db --no-browser
   ```

### Linux 直接启动

1. 下载并解压 `umi_upload-linux-amd64.zip`。
2. 在图形桌面中直接打开 `umi_upload-linux-amd64`：
   - 如果当前没有终端，程序会自动打开系统可用的终端模拟器，并在终端前台运行；
   - 终端显示启动日志和运行状态，程序退出后终端随之关闭；
   - 关闭终端窗口或按 `Ctrl+C` 会触发正常清理，等待正在执行的任务退出并关闭 SQLite；
   - 如果已经从终端启动，则直接在当前终端运行，不会再打开新终端。
3. 也可以手动打开终端运行：
   ```bash
   chmod +x ./umi_upload-linux-amd64
   ./umi_upload-linux-amd64
   ```

自动打开终端支持 `x-terminal-emulator`、GNOME Terminal、GNOME Console、Konsole、Xfce Terminal、MATE Terminal 和 xterm。图形桌面中如果找不到终端模拟器，会提示并退出，避免服务在后台无窗口运行。无图形桌面时保持命令行前台启动，适合 SSH 等场景。

### 状态与数据集接口优化

- `GET /api/state?light=1`：页面初始化/手动刷新使用的轻量元数据，不返回数据集列表。
- `GET /api/state?realtime=1`：只返回扫描、上传任务和带宽的内存状态，用于 SSE 断线降级，不访问 SQLite。
- `GET /api/datasets`：单独获取数据集列表，支持 ETag / `If-None-Match`；未变化时返回 `304 Not Modified`。
- SSE 状态帧包含数据集版本号；上传/校验记录变化时，前端只刷新数据集快照，不等待整个任务结束，也不会重复传输未变化的列表。
- 数据集快照接口临时失败时，前端自动退避重试（1 秒起步，最长 15 秒）；下一次 SSE 版本变化会立即触发重试。
- 数据集列表在内存中按版本缓存；扫描、上传、校验、移动修改数据时失效，Bucket 配置变化时自动重建。原 `/api/state` 完整返回方式保留兼容。
- 前端按数据快照 ETag 缓存过滤/排序结果和汇总统计；同一快照下的重复绘制不再重复全量过滤、排序和计数。数据内容未变（304）时不重建表格 DOM。

### Web 前端代码约定

- 保留当前经典脚本的加载顺序和共享状态边界；在未完成模块依赖拆分前，不把脚本直接改成 ES Modules。
- 新增代码优先使用 `const`、必要时使用 `let`、模板字符串、解构和早返回。
- JSON API 统一走 `requestJSON` / `api`，检查 HTTP 状态和 JSON 响应；长请求使用 `AbortController`，结果应用前检查请求是否仍是最新请求。
- 自定义 Enter 快捷操作必须跳过 IME 组合输入：同时跟踪 `compositionstart` / `compositionend` 状态，并检查 `KeyboardEvent.isComposing`；对 Safari 的事件派发顺序差异，使用 `compositionend` 的 `event.timeStamp` 做 50ms 边界保护。不要使用已弃用的 `keyCode`，同时保留原生表单提交路径。
- CI 对 `web/js/*.js` 执行 `node --check`，不引入额外前端构建依赖。
- 现代 Web API 和编码建议参考 [Google Chrome Modern Web Guidance](https://github.com/GoogleChrome/modern-web-guidance-src)，仅采用项目当前运行环境适合的能力。
### 实时状态推送

页面首次加载和用户操作后仍通过 HTTP 获取完整状态；上传进度、扫描进度、任务日志增量及动态带宽状态通过 `GET /api/events` SSE 长连接推送。服务端使用单一共享状态采样器，每 500ms 最多生成一次状态快照并广播给所有浏览器，每 15 秒发送心跳。每个连接独立维护日志游标；单个客户端发送过慢、积压超过 8 条事件时会被断开，由浏览器自动重连并通过完整快照恢复，不会阻塞上传线程或其他客户端。重连后重新获取完整状态补齐期间遗漏的数据。SSE 不替代配置、任务控制和数据查询等 HTTP API。

### Windows EXE 图标

图标源文件位于 `winres/umi_upload_icon.png`，资源定义位于 `winres/winres.json`。执行 `go run scripts/package_release.go -out dist` 时，打包脚本会先调用固定版本的 `go-winres v0.3.3` 生成 Windows amd64 资源对象，再编译 EXE 并自动清理临时资源文件。

---

## 二、Go 版与 sample 的功能对应

- **多套存储配置**：支持命名保存、切换、另存为和删除。每套配置独立保存存储类型、Endpoint、AK/SK、Bucket 和并发参数；默认使用阿里云 OSS Go SDK v2。
- **旧配置迁移**：升级时把旧版 `settings` 自动迁移为“默认配置”，保留原 Endpoint、AK/SK、Bucket 和并发参数；旧版本原有阿里云 SDK 调用方式不变。

1. **存储协议由配置显式选择**
   - 默认及旧配置迁移后均使用阿里云 OSS Go SDK v2（`github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss`），不依据主机名猜测，阿里云自定义 CNAME 也不会被误判；
   - 只有手动选择 MinIO / S3 时才使用 MinIO Go SDK v7，AWS Signature V4 + path-style。
2. **上传策略映射**
   - Go 版支持 **逐文件夹（`folder`）**、**逐分片（`slice`，后台预哈希 + 每目录远端清单核对）**、**流水线（`pipeline`，整会话连续上传）**；
   - sample 的 `ossutil` 外部进程引擎已由纯 Go SDK 上传路径替代，不再要求用户安装 `ossutil.exe`；原 `ossutil slice/session` 分别映射为 Go 版 `slice/pipeline`，但不再执行 ossutil 命令行。
   - 阿里云 OSS SDK v2 保留原有 SDK checkpoint、分片并发、网络恢复及原有 API 调用；MinIO/S3 方式支持并发 multipart 和断网后重试，当前失败的单个大文件可能从头重新上传；已完成文件在数据集重试时会跳过。
3. **最少依赖（`CGO_ENABLED=0` 纯 Go 静态编译）**
   - 核心依赖：
     - `github.com/aliyun/alibabacloud-oss-go-sdk-v2`（阿里云 OSS）
     - `github.com/minio/minio-go/v7`（MinIO / S3 Signature V4）
     - `github.com/ncruces/go-sqlite3`（零 CGO 纯 Go SQLite 驱动，支持 WAL 并发读写）
   - YAML 解析、4 表联立 `.xlsx` 报表生成、HTTP Range 视频流播放、UTF-16 字典序清单签名均由 Go 标准库直接实现。
4. **业务规则与防呆红线按 sample 对齐**
   - **双格式共存与分桶隔离**：`ISuit-V3_YYYY-MM-DD-HH-MM-SS`（默认 `hw-upload`）与 `UMI_v3_YYYYMMDD_HHMMSS`（默认 `prt-huawei`）按会话目录名自动识别、分桶隔离；
   - **严格两阶段提交**：全量数据文件上传 + 远端数量/大小核对 → 写入并回读校验 `manifest.json`（与 Python 版 `revision` 逐字节完全一致） → 最后写入并校验 `_READY`；
   - **快速校验 / 深度 SHA-256 校验 / 校验补录**：支持快速核对清单与逐文件下载比对 SHA-256，远端一致但本地无记录时自动单次补录；
   - **错放修正与视频预览**：`ISuit-V3` 自动错放检测与一键移动；`UMI_v3` 内置浏览器 `Range (206)` 多机位视频预览与 `normal ↔ calibration` 手动双向移动；
   - **统计导出与多出口并发建议**：支持筛选、按会话去重统计、导出 4 工作表 `.xlsx` 报表与官方兼容 `umi-upload-history.json`，以及多 WAN 出口并发参数估算。

---

## 三、动态上传带宽调优

上传任务运行期间可在「存储与并发配置 → 动态上传带宽调优」修改目标带宽和并发参数，无需停止或重启程序。

- `target_mbps`：目标总上传带宽（Mbps），默认 350；设置为 0 时表示持续探索当前环境可达到的吞吐上限。
- `auto_tune`：开启后每 20 秒根据实际上传吞吐调整并发；关闭后仍支持运行中手动修改并发。
- `max_jobs`：文件级并发上限（跨当前批次数据集共享），默认 48。
- `max_parallel`：单个对象后续上传请求使用的分片并发上限，默认 12。
- `jobs`、`ds_jobs`：运行中调整文件级和数据集级任务准入上限；降低上限不会中断已经开始的上传。
- `parallel`：修改后对后续开始的对象上传生效，不会重建或中断正在执行的 Multipart 上传。
- 保存 `target_mbps`、`auto_tune` 等非并发设置时，保留当前任务已自动探索出的并发；明确修改 `jobs`、`ds_jobs`、`max_jobs`、`parallel` 或 `max_parallel` 时，重新应用对应并发配置。
- 每次启动新的上传任务，都会从当前保存的 `jobs`、`parallel` 配置重新初始化，不继承上一次任务的自动调优结果。
- 配置快照发布、运行时限流器更新与调优动作复核串行执行；关闭自动调优或最新目标已达到时，不执行基于旧配置的并发增加。

自动调优只调整任务准入和后续对象上传的分片并发，不取消已开始的对象上传，不改变文件排序、SHA-256、远端核对、manifest 与 `_READY` 两阶段提交顺序。已存在的远端文件不会计入网络吞吐统计。

可通过本机接口 `GET /api/bandwidth` 查看调优状态。状态中的速度是应用层观测到的上传流量滑动窗口估算：MinIO/S3 大文件按 SDK HTTP 请求体读取统计，小文件在上传成功后按对象大小补齐；OSS 按本次新完成的分片统计。OSS checkpoint 中已完成的历史分片只计入逻辑进度，不重复计入当前吞吐。S3 Multipart 内部重试产生的重复请求体读取也会计入；小对象的内部重试无法单独拆分统计。它不等同于服务端确认吞吐或路由器 WAN 端口计数器。程序无法直接控制 H3C 多 WAN 的连接分配，也无法保证达到目标；当 OSS、运营商线路、磁盘或 CPU 成为瓶颈时，会受限于实际可用能力。

## 四、CI/CD 自动打包 Release（GitLab CI & GitHub Actions）

仓库已内置 **`.gitlab-ci.yml`**（以及 `.github/workflows/release.yml`），零外部打包工具依赖（由纯 Go 标准库脚本 `scripts/package_release.go` 完成交叉编译、ZIP 打包、SHA-256 计算及 Release 发布）：

1. **合并到主分支（`main`）自动发版**：
   - 自动运行全套回归测试 `go test ./...`；
   - 自动交叉编译单文件 `dist/umi_upload.exe` 并打包 `dist/umi_upload-windows-amd64.zip`、`dist/SHA256SUMS.txt`；
   - 在 GitLab 中自动上传至 **Generic Package Registry** + **Job Artifacts**，并创建 `release-YYYYMMDD-<short_sha>` **GitLab Release**（通过 `CI_JOB_TOKEN` 自动鉴权；若需使用个人/项目令牌可配置可选变量 `GITLAB_TOKEN`）。
2. **推送版本标签（如 `v1.0.0`）正式发版**：
   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```
   自动将版本号注入二进制（`umi_upload.exe --version`）并发布对应版本的正式 Release。

---

## 五、本地开发与手动编译

环境要求：Go 1.26+

```bash
# 运行完整回归测试（含并发竞争检测与 Python 清单哈希一致性校验）
go test -v -race ./...

# 一键构建 Windows 单文件可执行程序 + 发布压缩包到 dist/
go run scripts/package_release.go -out dist

# 或直接编译到当前目录 umi_upload.exe
./build.sh
# Windows 下运行：
build.bat
```
