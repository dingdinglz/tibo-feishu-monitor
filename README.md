# Tibo Feishu Monitor

Monitor Tibo's X updates or Codex Resets, and send Feishu cards with optional daily reset reports.

Go 常驻程序，支持两个可切换的数据源，向多个飞书群自定义机器人推送：

- `source: x`（默认）：Python `twscrape` 读取 [@thsottiaux](https://x.com/thsottiaux) 的主帖、引用帖和本人回复，不转发他人的评论或原生转发。
- `source: codex-resets`：直接调用 [Codex Resets 公共 API](https://codex-resets.com/api/docs)，通知新出现的已执行重置/备用重置额度记录；可每日定时发送当前状态卡片。**不需要 Python、X 账号或 TypeSafe Key**。

## 行为约定

下列动态、上下文及 TypeSafe 规则针对默认的 X 数据源；API 数据源的差异见「切换到 Codex Resets 与每日卡片」。

- 默认一次抓取结束后，等待 **3 分钟 + 独立随机的 1–30 秒**。基础间隔可在 YAML 中调整；不会重叠抓取。
- 默认第一次成功取得时间线时建立静默基线，基线以前的帖子不通知；重启保留基线、续页进度和待发送任务。开启 `-debug` 时，另有下面说明的测试推送。
- 自动补抓最近 **24 小时**（可配置），重叠读取，不只比较最大帖子 ID。窗口外的缺口会告警；已入队的旧消息不因超出窗口而丢弃。
- **每条主帖、引用、回复各发一张飞书 Card 2.0 卡片**；一次新增 N 条就向每个启用的目标逐条发送 N 张，不合并成汇总卡片。
- SQLite 事务保存帖子、各目标的独立发送任务和采集进度。某个群失败，不阻塞其他群或后续采集。
- 卡片包含类型、作者、北京时间、原文、帖子 ID、最多一层回复/引用上下文、**本次额度重置概率（百分比 + 进度条）**和“查看原帖”按钮。超长原文按最终请求体字节数截断并明确提示，**不拆成多张卡片**；完整原文仍保存在业务库中，可通过原帖链接查看。上下文不可见时照常推送。
- 临时发送错误指数退避；永久配置错误暂停该目标，修正配置并重启后重试。连续失败达到阈值通知其他可用目标，恢复通知一次；未开启 `-debug` 时，正常无更新不向群里发消息。

**随机偏移不能保证不封号**。只使用自己有权使用的账号，会话失效需手动更新。遵守 X 的访问规则和限流；程序不清除账号锁、不自动登录、不处理验证码。

## 环境

- Go **1.23+**（构建时需要；SQLite 使用纯 Go 驱动，运行不依赖 CGO）。
- 仅 X 数据源需要 Python **3.10+**，固定 `twscrape==0.20.1`；API 数据源可跳过虚拟环境和 pip 安装。
- macOS 或 Linux；Linux 常驻推荐 systemd。不提供 Web 管理界面。
- 飞书客户端 7.20 或更新版本，以支持 Card 2.0。
- X 数据源需要能访问 X 的网络/可选代理及有效 twscrape 账号库；API 数据源只需能直连 `https://codex-resets.com`。

```bash
git clone https://github.com/dingdinglz/tibo-feishu-monitor.git
cd tibo-feishu-monitor

# 请确认 python3 >= 3.10；某些 macOS 的 python3 仍是旧版本。
python3 --version
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
make build
cp -n config.example.yaml config.yaml  # 不覆盖已有配置
chmod 600 config.yaml
```

仓库只提供 `config.example.yaml`。复制后自行配置 Python、账号库路径和 Webhook；实际的 `config.yaml` 已加入 `.gitignore`。

也可以复用已安装 twscrape 的 Python 解释器，将配置替换为自己的路径：

```yaml
scrape:
  python: /absolute/path/to/python
  script: scripts/fetch.py
  accounts_db: ${TWSCRAPE_ACCOUNTS_DB}
```

已有账号库不需要重新登录；程序不提供账号导入命令。新机器需按 [twscrape 官方说明](https://github.com/vladkens/twscrape#start-with-cookies) 单独配置 Cookie 会话。不要把账号库放进仓库，建议权限 `0600`。程序只提示现有账号库权限过宽，不替你修改它。

## 配置与启动

编辑 `config.yaml`。所有相对文件路径相对于**配置文件所在目录**，不是执行命令时的目录。支持 `~/` 和 `${环境变量名}`；引用未设置的变量会报错，禁用目标不要求凭据。

```bash
export TWSCRAPE_ACCOUNTS_DB='/absolute/path/to/accounts.db'
export TYPESAFE_API_KEY='替换为你的 TypeSafe API Key'
# 在自己的终端或受限环境文件里设置，不要粘贴到聊天、Git 或共享日志中：
export FEISHU_TEAM_WEBHOOK='https://open.feishu.cn/open-apis/bot/v2/hook/替换为你的地址'
./bin/tibo-monitor run -config config.yaml
```

Webhook 列表是**全部发送**，不是主备切换：

```yaml
webhooks:
  - id: team-a
    enabled: true
    url: ${FEISHU_TEAM_WEBHOOK}
    secret: ""                 # 开启签名后改为 ${FEISHU_TEAM_SECRET}
    prefix: "Tibo 更新"
  - id: team-b
    enabled: true
    url: ${FEISHU_OTHER_WEBHOOK}
    secret: ${FEISHU_OTHER_SECRET}
    prefix: "Tibo 更新"
```

允许直接在 YAML 填 URL/密钥，但更推荐环境变量。程序**不会自动加载 `.env` 文件**；systemd 用 `EnvironmentFile`，手动启动时自行导出环境变量。程序不在日志或业务库中保存 Webhook、签名密钥、TypeSafe API Key、Cookie 或代理凭据。

### 切换到 Codex Resets 与每日卡片

在现有 `config.yaml` 中加入以下配置，保留原来的 `database` 和 `webhooks`，然后重启：

```yaml
source: codex-resets          # 也接受 https://codex-resets.com/api/docs
codex_resets:
  timeout: 20s

daily_report:
  enabled: true
  time: "09:00"             # 自行选择每日时间，24 小时制 HH:MM
  timezone: Asia/Shanghai    # IANA 时区，例如 UTC、America/Los_Angeles
```

也可只用 `database`、上述配置和 `webhooks` 创建新配置。`scrape` / `typesafe` 可删除，也可原样保留：API 模式不解析它们的环境变量、不检查 Python/账号文件、不持有账号库锁、不调用 TypeSafe。此 API 固定追踪 thsottiaux，不支持自定义 `target`。`source` 不填仍为 `x`；改回 `x` 并重启即可恢复原采集方式。

**新增重置通知：** 按 `poll.interval` 读取 `/api/v1/resets`，遵循游标分页、补抓窗口、限流冷却和现有发送重试。第一次成功建立独立静默基线，不把 API 历史刷到群里；切换来源不会复用 X 游标，也不会因 X 已见过相同帖子 ID 而漏掉后续 API 重置记录。再次切回沿用该来源的基线。已有发送任务继续处理，不清空数据库。API 不提供完整 X 时间线，普通动态、28 天挑战进展不在此模式的覆盖范围；预告与预测在日报里展示，不作为已执行重置通知。

**每日状态卡片：** 参考 Codex Resets 页面，突出距最近重置的时长、发生时间，展示重置次数、平均间隔、最长等待；有待执行预告或有效预测时一起展示。所有卡片附 [数据来源：Codex Resets](https://codex-resets.com) 链接。

- 默认关闭；只在 `source: codex-resets` 且 `enabled: true` 时创建日报。X 模式会提示该功能未生效。
- 独立调度，每 30 秒检查一次，不受 X/API 轮询周期或采集阻塞影响；到点后请求最新快照，发送还可能受网络、队列和目标限流影响。
- 以配置时区的**日期 + 目标 ID**持久去重，重启不重复创建当天任务。到点后才启动会补今天一张，不补停机期间的往日报表。新启用目标可收到当天一张。
- API 失败不消耗当天名额，退避后重新读取；飞书失败走持久发送队列，失败目标不影响其他目标。次日新卡片会替换尚未发送的旧日报，避免恢复时刷出过期日报；普通重置事件不会因此丢弃。关闭功能/切回 X 后不创建新日报，已经入队的任务仍可能补发。
- 卡片是**采集时的快照**，明确标注采集时间、时区和上游生成时间；发送重试不会假装数据刚刚更新。API 有 CDN 缓存，不承诺实时性。
- `regular` 是上游标记的已执行重置，`banked` 是备用重置额度；没有 X 公告的 `observed` 记录显示为网站观测。不是对每个用户实际额度的权威核验。
- `scheduled_reset` 即便计划时间已过仍显示待执行；`active_watch` 明确标为第三方 AI 预测，非官方承诺，过期预测不展示，缺失概率不冒充 0%。
- 次数和平均间隔来自 `/api/v1/status`；最长等待来自 `/api/v1/resets` **完整历史中相邻记录的最大间隔**，不包含仍在进行的等待。完整分页失败会重试，不用局部数据；两接口缓存导致数量/最新记录及时间不一致时，最长等待显示暂无数据。少于两条记录时也不计算。

只读验证当前数据（不创建业务库、不发送飞书、不调用 TypeSafe）：

```bash
./bin/tibo-monitor probe -config config.yaml
```

API 模式会读取状态和完整历史，输出含最长间隔的日报快照以及来源链接。`--pages` 仅适用于 X。验证无误后执行 `run` 启动；仓库不会自动修改运行配置或安装定时服务。

### 调度和抓取

| 配置 | 默认 | 含义 |
|---|---:|---|
| `poll.interval` | `3m` | 基础间隔，至少 `1m`，每次额外加 1–30 秒 |
| `poll.catch_up_window` | `24h` | 自动补抓窗口，最多 `720h` |
| `scrape.timeout` | `75s` | Python 抓取及补上下文整体预算 |
| `scrape.wait_timeout` | `15s` | 等待可用账号的上限 |
| `scrape.max_pages` | `5` | 每轮页预算，至少 2；续抓时包含最新头页 |
| `scrape.context_limit` | `4` | 每轮最多额外读取几条父帖，支持 0 |
| `scrape.proxy` | 空 | 仅 X 抓取使用，支持 HTTP(S)/SOCKS5 URL |
| `delivery.send_interval` | `1s` | 每个目标的最短发送间隔，包含告警 |
| `delivery.retry_initial` / `retry_max` | `30s` / `30m` | 临时发送错误的指数退避 |
| `alerts.failure_threshold` | `3` | 连续失败阈值；永久发送配置错误立即提醒 |
| `alerts.cooldown` | `30m` | 同一故障通知冷却时间 |

飞书、Codex Resets 和 TypeSafe 默认直连，不继承系统 HTTP 代理；X 还可能使用 twscrape 账号库自带的代理。失败抓取也会退避，平台给出的冷却时间优先；基础轮询周期不是端到端延迟承诺。

### 额度重置概率（TypeSafe AI）

每条主帖、回复、引用卡片都在**发送前**调用 TypeSafe System One（默认 `jev-latest`），用 Noul 的真实 `P(yes)` 显示百分比和十格进度条。问题限定为：**这条动态是否在预告、承诺或确认本次 Codex 使用额度/速率额度重置**。这是依据帖子内容的 AI 估计，不是已重置的保证，也不是额度剩余量或等待进度。

```yaml
typesafe:
  api_key: ""             # 空值读取 TYPESAFE_API_KEY，也支持 ${TYPESAFE_API_KEY}
  model: jev-latest
  timeout: 15s            # 包含有限重试的整体预算，1s..1m
  recent_posts: 8         # 最多取此前几条本人动态，1..10
  history_window: 72h     # 相对被分析帖子时间，1h..720h
```

- 使用当前原文、可用的回复/引用上下文，以及业务库中**此前**的本人动态。历史按时间和数字 ID 排序，不将未来帖子或其他作者当作他的发言；正文有长度上限。没有足够历史时仍分析，并在卡片标注实际参考数量。不额外增加 X 抓取请求。
- 例如 `Day 10/ Reset` → `Landing within the next 2 hours` → `Done and dusted` 需要结合上下文；普通寒暄、修 bug、自然定时恢复、他人要求重置或否认重置，不应只因附近出现 reset 就算作重置承诺。
- 同一普通帖子向多个群发送、失败重试或重启时复用已持久化的成功判断。**每个新的 debug 测试批次重新调用真实 API**，不复用普通帖或上一轮 debug 的概率；同批次多群共享一次结果。
- 未配置 Key、接口超时/鉴权/限流失败或返回无效概率时，照常发送原帖，概率区明确显示“暂不可用”，不使用关键词、随机数或固定 0%/50% 冒充结果；日志记录不可用状态。成功判断会保存概率、模型版本、评估时间和参考条数，不保存凭据。
- 推理在采集事务之外执行，不让外部 AI 请求占住 SQLite 事务。AI 请求可能增加推送延迟，但不改变正常帖子的去重和 debug 入队规则。
- `probe` / `status` 不调用 AI，也不要求 Key；`test-send` 仍是单独的人工测试通知，没有待分析帖子。联网推理只发生在 `run` 的帖子卡片发送路径。

TypeSafe Key 可在 [TypeSafe 控制台](https://console.typesafe.ai) 创建。配置后重启程序生效；无需修改已有 `config.yaml`，仅设置环境变量也可使用默认参数。

### Webhook 变更

- `id` 是稳定标识，不随地址或列表顺序变化。更换 URL/签名/前缀后重启；已成功的消息不重发，未成功的继续发。
- 新 `id` 仅接收启用后**首次发现**的动态，不给已入库的帖子创建历史任务。
- 禁用或从配置删除：保留原有积压，暂停发送；禁用期间不为该目标创建新的任务。
- 相同 `id` 重新启用：继续此前积压。想作为全新目标使用，应换新 `id`。
- 不支持热加载。监控目标 ID 与业务库绑定，换账号请使用新的业务数据库。

## 命令

### 调试：没有新增也发送最新一条

```bash
./bin/tibo-monitor run -config config.yaml -debug
```

- 每轮成功抓取后，有新增则正常发送；没有新增则向所有启用的目标重发最新一条本人动态（含回复、引用），使用橙色标题并标注 `DEBUG 测试`。
- 首次建立静默基线时也可以触发测试推送；最新一条早于补抓窗口时仍可用于测试，不扩大正常补抓范围。
- 按发布时间和帖子 ID 判断最新，不直接取接口返回的第一项。抓取失败或没有可用帖子时不伪造测试消息。
- X 模式每个新测试批次在推送前真实调用 TypeSafe，卡片展示该次概率；调用失败显示“暂不可用”，不填模拟值。API 模式则重发 `/status` 的最近重置记录，不调用 TypeSafe；没有记录不发送测试卡片。
- 调试任务独立入队，不修改正常帖子的去重记录。失败照常重试；同一目标还有未完成调试任务时，不重复积压新的测试批次，正常动态优先发送。
- 去掉 `-debug` 并重启后停止创建调试推送；已经入队但尚未成功的任务仍按发送队列补发。此参数仅用于 `run`，不会开启 twscrape 的敏感详细日志。

### 只读验证数据源，不发飞书

```bash
./bin/tibo-monitor probe -config config.yaml  # X 模式可加 --pages 2
```

X 模式输出公开帖子的 JSON、页数和覆盖状态；API 模式输出日报数据快照（见上文）。两种模式均不创建/更新监控业务数据库，不发送飞书。**twscrape 自身会更新账号库的使用统计和限流锁**，这是正常读取的一部分。

`probe` 不要求 Webhook 环境变量。页预算不足导致 `complete=false` 不等于没有新帖；`error` 非空或进程退出码非零才是明确的抓取失败。首次正式 `run` 仍会单独建立基线。

### 查看持久状态

```bash
./bin/tibo-monitor status -config config.yaml
```

包含最后运行的数据源、该来源的最后请求成功时间/完整采集时间/基线、帖子及 API 重置记录数、最近日报入队日期、待发数量、目标是否暂停及最后发送时间；不包含凭据。

### 手动发送一条测试消息

按[飞书官方指南](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot) 创建群自定义机器人并配置安全校验，然后显式执行：

```bash
./bin/tibo-monitor test-send -config config.yaml -destination team -confirm
```

只向指定目标发一张明确标记的测试卡片，验证与真实帖子相同的 Card 2.0 发送路径。命令中的 `team` 必须与自己的配置 `id` 一致；没有 `-confirm` 不发送。普通启动不自动发测试消息；`run -debug` 的测试推送规则见上文。

签名算法采用官方的 `HMAC-SHA256(key=timestamp+"\n"+secret, message="")`，每次尝试重新签名。关键词校验需让 `prefix` 包含所配置关键词；IP 白名单由运行机器出口地址决定。系统时间应同步。卡片中的动态正文使用 `plain_text`，不执行原文中的 Markdown 或交互标签；`<at ...>` 另外做防提及处理。所有按钮仅打开 X 原帖/上下文或 Codex Resets 来源链接，无服务端回调，不需要飞书应用权限。运维告警和恢复提醒仍采用文本消息。

卡片整个 HTTP 请求体（包括签名、时间戳和关键词前缀）限制在 19,000 字节以内，为 Webhook 的 20KB 限制留出余量。新版启动时会将旧版未完成的**普通帖子**分片按“帖子 + 目标”合并为一张待发送卡片，保留重试时间和尝试次数；已发送记录不会重发。旧版调试消息仍兼容，并以测试卡片发送。

## Linux systemd 部署

仓库内 `deploy/tibo-monitor.service` 是模板，**不会自动安装或启用服务**。

1. 创建专用低权限用户 `tibo`。
2. 将程序、`scripts/`、`requirements.txt`、虚拟环境放到 `/opt/tibo-monitor`；不要复制 macOS 的虚拟环境到 Linux，应在 Linux 重建。
3. 配置放 `/etc/tibo-monitor/config.yaml`，环境变量放 `/etc/tibo-monitor/env`（权限 `0600`，不要有 `export` 前缀）。
4. 将业务库和账号库放 `/var/lib/tibo-monitor/`，目录归 `tibo` 所有、权限 `0700`，文件 `0600`。SQLite 所在目录必须可写，以创建 WAL 和锁文件。
5. 配置使用以下**绝对路径**，避免 `/etc` 下的相对路径指错：

```yaml
database: /var/lib/tibo-monitor/monitor.db
scrape:
  python: /opt/tibo-monitor/.venv/bin/python
  script: /opt/tibo-monitor/scripts/fetch.py
  accounts_db: /var/lib/tibo-monitor/accounts.db
```

环境文件示意：

```text
FEISHU_TEAM_WEBHOOK=https://open.feishu.cn/open-apis/bot/v2/hook/替换为你的地址
FEISHU_TEAM_SECRET=替换为你的签名密钥
TYPESAFE_API_KEY=替换为你的TypeSafe密钥
```

```bash
sudo cp deploy/tibo-monitor.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tibo-monitor
sudo journalctl -u tibo-monitor -f
```

服务限制只允许写 `/var/lib/tibo-monitor`。如调整数据目录，也要调整 `ReadWritePaths`。停止时发 SIGTERM，程序取消抓取、保留未确认任务并关闭数据库。

## 测试

```bash
make test                    # Go 单元测试、SQLite 恢复/去重测试、HTTP 模拟测试，含 race
make test-python             # Python 离线模拟分页测试，不需要账号
# 或指定另一个已安装 twscrape 的 Python：
make test-python PYTHON=/absolute/path/to/python
go vet ./...
```

测试不向 X/Codex Resets/飞书/TypeSafe 发送真实请求；新增 API 协议与分页、来源切换、时区/DST 调度、日报去重/重启/失败重试和卡片用离线模拟测试覆盖。TypeSafe HTTP 协议、概率渲染、debug 每批次重新评估和历史上下文用离线模拟测试覆盖。实际联网只读验证使用 `probe`；真实飞书推送必须单独执行 `test-send` 或启用 `run`。

## 完整性与运维边界

- twscrape 访问的是 X 网页接口，不是可靠的事件流。已删除、不可见、X 未返回的帖子无法保证抓到。
- 不能因看到一个旧帖/置顶帖就停止；抓取按窗口重叠扫描。超出页预算会保存续页，下一轮先刷新最新头页，再接续旧页。
- 续页完成不立即宣称整个窗口完整：只刷一页最新头部可能漏掉期间的突发多页新增，因此还需要一次从头扫描确认。持续高流量时应提高页预算或接受“覆盖不完整”告警，不能默默推进完整进度。
- 库无声中止、空响应、超时、解析异常不会被当成“没有新增”。上游兼容性变化可能需要升级 twscrape 后重新测试；当前适配固定在 `0.20.1`。
- 网络超时可能发生在飞书已经收到后。本地去重避免常规重复，但为优先不漏，会重试结果不确定的发送，因此不能保证跨网络恰好一次。
- 每个目标按已入队帖子的时间逐张发送；多条新增不合并，不并发轰炸同一个机器人。迟到才发现的旧帖无法插到已经发出的消息前；告警优先于普通积压，调试卡片排在正常动态之后。
- 所有机器人都失效时，飞书无法承担自身故障通知，只能检查服务日志和 `status`。没有独立邮件/短信通道。
- 业务库和账号库分开。业务库保留去重记录、原文及发送状态，不自动清理；定期备份并监控磁盘。不要直接删除业务库来排错，删除意味着下一次重新建立基线。
- 只运行一个实例；程序对业务库加进程锁，X 模式另对账号库加锁。不要同时用其他程序高频读取同一个账号库，不要手动清锁规避冷却。

设计约定见 [`docs/design.md`](docs/design.md)，凭据管理与发布检查见 [`SECURITY.md`](SECURITY.md)。

接口资料：[twscrape](https://github.com/vladkens/twscrape)、[飞书群自定义机器人](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)、[飞书 Card 2.0](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-structure)。
