# UMI 数据中心（阿里云 OSS Go SDK 单文件版）

基于 Go 标准库 + **阿里云官方 `alibabacloud-oss-go-sdk-v2`** 重构打包的 UMI 采集数据管理与上传中心。已移除外部 `ossutil.exe` 与 Python 运行时依赖，前端静态资源通过 `go:embed` 内嵌至单个可执行文件 `umi_upload.exe`，拷贝到任意 Windows 机器即可直接双击启动。

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

---

## 二、核心变更与特性对齐（功能 100% 保持不变）

1. **`ossutil` 完全替换为阿里云官方 Go SDK（`alibabacloud-oss-go-sdk-v2`）**
   - 大文件（≥ 10 MiB）自动启用 SDK 分片并发上传（`ParallelNum = parallel`）与本地 `.ucp` 断点续传检查点；
   - 支持 **逐文件夹（`folder`）**、**逐分片（`slice`，后台预哈希 + 每目录远端清单核对）**、**流水线（`pipeline`，整会话无空档连续上传）** 三种上传顺序；
   - 完整保留 **断网自动等待与恢复（`net_wait`）**：网络抖动/中断时自动循环探测 OSS 连通性，恢复后接着续传并跳过已传文件；即使进程重启，也会通过 `umi_ckpt/` 自动断点续传。
2. **最少依赖（`CGO_ENABLED=0` 纯 Go 静态编译）**
   - 直接依赖仅 2 个核心模块：
     - `github.com/aliyun/alibabacloud-oss-go-sdk-v2`（阿里云官方 OSS Go SDK v2）
     - `github.com/ncruces/go-sqlite3`（零 CGO 纯 Go SQLite 驱动，支持 WAL 并发读写）
   - YAML 解析、4 表联立 `.xlsx` 报表生成、HTTP Range 视频流播放、UTF-16 字典序清单签名均由 Go 标准库直接实现。
3. **业务规则与防呆红线 100% 保持一致**
   - **双格式共存与分桶隔离**：`ISuit-V3_YYYY-MM-DD-HH-MM-SS`（默认 `hw-upload`）与 `UMI_v3_YYYYMMDD_HHMMSS`（默认 `prt-huawei`）按会话目录名自动识别、分桶隔离；
   - **严格两阶段提交**：全量数据文件上传 + 远端数量/大小核对 → 写入并回读校验 `manifest.json`（与 Python 版 `revision` 逐字节完全一致） → 最后写入并校验 `_READY`；
   - **快速校验 / 深度 SHA-256 校验 / 校验补录**：支持快速核对清单与逐文件下载比对 SHA-256，远端一致但本地无记录时自动单次补录；
   - **错放修正与视频预览**：`ISuit-V3` 自动错放检测与一键移动；`UMI_v3` 内置浏览器 `Range (206)` 多机位视频预览与 `normal ↔ calibration` 手动双向移动；
   - **统计导出与多出口并发建议**：支持筛选、按会话去重统计、导出 4 工作表 `.xlsx` 报表与官方兼容 `umi-upload-history.json`，以及多 WAN 出口并发参数估算。

---

## 三、CI/CD 自动打包 Release（GitLab CI & GitHub Actions）

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

## 四、本地开发与手动编译

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
