# 番茄阅读器（本机版）

把番茄小说手机端能看到的章节，在电脑浏览器里按小说的样子排版阅读。

整条链路都在本机完成：**注册设备 → 本地计算请求签名 → 取回章节密文 → 解密 → 还原字体混淆 → 网页排版**。
不需要任何外部签名服务，不包含任何官方二进制文件，也不需要改动手机 App。

> **使用边界**：本项目仅用于个人学习与协议实现研究。请勿用于商业用途，请勿二次分发通过本工具获取的内容；使用时请遵守相关服务条款与所在地法律。仓库内不含任何官方 APK、原生库或字体文件。

---

## 能做什么

| 功能 | 说明 |
|---|---|
| 首页 | 推荐位、分类榜单、最近更新（公开接口） |
| 搜索 | 站内搜索 + 首页书目兜底 |
| 目录 | 分卷展示全部章节，支持倒序 |
| 正文 | 章节正文解密后按段落排版，段落缩进、行距可调 |
| 字体还原 | 把私用区（PUA）乱码字符还原成真实汉字 |
| 预取 | 读到第 i 章时后台补齐附近若干章，翻页秒开 |
| 账号 | 扫码登录或粘贴 Cookie，同步云端书架与阅读进度 |
| 书架 | 云端书架、最近阅读、本地续读记录 |
| 排版 | 主题、字体、字号、行距、页宽、翻页模式、自动续读 |

---

## 工作原理

```
浏览器  ──▶  Go 服务(:8080)  ──▶  番茄公开接口
  页面/排版      │
                ├─ 设备注册：device_id / install_id
                ├─ 内容密钥：按设备申请，用于解密章节密文
                ├─ 请求签名：x-argus / x-ladon / x-ss-stub …（本地纯算）
                ├─ 章节解密：AES-128-CBC（iv 前置）
                └─ 字体还原：PUA → 汉字映射表
```

四个关键环节：

1. **设备**：按 App 的注册流程申请设备号，密钥与设备绑定；失效时自动重新注册。
2. **签名**：`internal/fqsign` 用 SM3、Simon、AES 等标准算法在本地计算所需请求头，不依赖 native 库。
3. **解密**：章节内容是 `base64(iv ‖ 密文)`，用该设备的内容密钥 AES-128-CBC 解开，必要时再 gzip 解压。
4. **字体**：番茄会给约六成汉字下发「字形正确、码位错误」的字体；`internal/font` 用字形位图比对还原成真实汉字（映射表见 `data/font_map.json`）。

---

## 快速开始

### 源码运行

```bash
go run ./cmd/server -addr :8080
# 打开 http://localhost:8080/
```

### 脚本启动

```bash
./start.sh          # 编译并后台启动，默认 8080
./start.sh --status
./start.sh --stop
```

### Docker

```bash
docker compose up -d --build
# 打开 http://localhost:8080/
```

镜像由你自己构建，仓库不提供预构建镜像。

---

## 配置

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-addr` | `FQ_ADDR` | `:8080` | 监听地址 |
| `-book` | `FQ_BOOK_ID` | 空 | 默认书籍 ID |
| `-font-map` | `FQ_FONT_MAP` | `data/font_map.json` | 字体映射表路径 |
| `-timeout` | — | `20` | 上游超时（秒） |
| `-account-file` | `FQ_ACCOUNT_FILE` | `data/account.json` | 账号会话（含登录凭据，权限 0600） |
| `-prefetch-window` | `FQ_PREFETCH_WINDOW` | `3` | 预取前后章数 |
| `-prefetch-workers` | `FQ_PREFETCH_WORKERS` | `1` | 预取并发 |
| `-prefetch-delay-ms` | `FQ_PREFETCH_DELAY_MS` | `1500` | 预取每章间隔（限流时调大） |

---

## HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | 状态、字体条数、设备号 |
| GET | `/api/home` | 首页聚合（推荐 + 榜单 + 更新） |
| GET | `/api/rank?gender=&categoryId=&mold=&limit=` | 分类榜单 |
| GET | `/api/recent?limit=` | 全站最近更新 |
| GET | `/api/search?q=` | 搜索 |
| GET | `/api/book/{bookId}` | 书籍信息与目录 |
| GET | `/api/chapter/{itemId}?bookId=` | 章节正文（已解密、已还原字体） |
| GET | `/api/prefetch/status` | 预取状态 |
| POST | `/api/prefetch` | 调整预取窗口 |
| POST | `/api/font/reload` | 热加载字体映射表 |
| POST | `/api/device` | 重新注册设备 |
| GET/POST | `/api/account*` | 登录、书架、进度 |

---

## 字体还原

番茄每本书可能下发不同的混淆字体，映射表需要自己生成：

```bash
pip install fonttools pillow numpy brotli
python3 tools/build_font_map_img.py --obf obf.woff2 --ref SourceHanSansSC-Regular.otf --out data/font_map.json
```

未覆盖的字符会在页面顶部给出提示，正文仍可阅读。

---

## 项目结构

```
cmd/server/          HTTP 服务与页面（静态资源编译期内嵌）
internal/fqapi/      设备注册、签名调度、章节取数与解密、账号与书架
internal/fqsign/     请求签名算法（SM3 / Simon / Ladon / x-argus）
internal/font/       PUA 字体映射与还原
tools/               字体映射表生成脚本
data/                字体映射表
```

---

## 已知限制

- **会被限流**：短时间大量取章会触发风控，预取会自动暂停一段时间，手动重试即可；把 `-prefetch-delay-ms` 调大更稳。
- **映射表可能不全**：新书新字体需要重新生成。
- **只读自己账号**：仅同步自己的书架与进度，不提供任何写入他人数据的能力。
- **不提供内容分发**：没有下载库、没有站点，也不随仓库分发任何作品内容。

---

## 许可

GPL-3.0，见 [LICENSE](LICENSE)。第三方来源与归属见 [NOTICE](NOTICE)。
