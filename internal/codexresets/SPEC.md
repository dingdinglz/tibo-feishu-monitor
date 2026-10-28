---
id: tibo-codex-resets
type: module-design
status: draft
title: Codex Resets 只读协议适配
parent: tibo-architecture
depends-on: [tibo-model]
---

# Codex Resets 只读协议适配

## 职责

从固定生产 HTTPS 地址读取状态及分页重置历史，检查协议版本、必需字段与领域类型。显式 null 与遗漏字段有区别；允许未知附加字段以兼容上游扩展。历史读取必须完整成功，页失败、重复游标或预算耗尽不能以部分历史冒充成功。

每次方法调用的总超时覆盖全部分页，限制单响应大小及页数。客户端允许并发请求，但共享服务端冷却期限，防止日报绕过轮询取得的 Retry-After；不自行安排重试或缓存响应。

## 边界

不读取 X 会话或 TypeSafe 凭据，不继承环境 HTTP 代理，不跟随重定向；错误只暴露固定类别及等待时间，不保留原始 URL、响应或底层敏感异常。

不决定基线、通知资格、日报日期或最长间隔统计，也不把 API 数据转成帖子或 AI 判断。缓存一致性与来源语义见 `tibo-architecture`。

**未确认的长期约束（由现有导入关系推断）：**保持只依赖领域类型的只读来源适配器，不接触配置加载、业务库或发送端。
