---
id: tibo-monitor
type: module-design
status: draft
title: 采集与发送编排
parent: tibo-architecture
depends-on: [tibo-config, tibo-model, tibo-store, tibo-feishu, tibo-codex-resets]
---

# 采集与发送编排

## 职责

协调顺序采集循环、每目标发送 worker 和可选日报 worker；决定何时调用来源、提交状态、准备卡片及登记故障。静默基线、续抓、每日调度、debug 和推理缓存语义统一见 `tibo-architecture`。

外部抓取、发送、推理和 API 提供器通过接口注入；持久库和卡片格式器是具体依赖，不能把本包描述成完全隔离基础设施的纯领域层。API 客户端类型仅用于识别安全错误与冷却提示，不在这里实现 HTTP 协议。

## 边界与故障隔离

外部推理不占用采集事务。现有实现以一个全局互斥锁串行化所有判断，不只是同帖请求；不同群的发送循环独立，但推理延迟仍可能相互影响。上游推理失败允许原帖继续发送，存储或卡片准备内部失败则不能当成普通网络故障悄悄跳过。

不直接依赖 Python 或 TypeSafe 的具体客户端，也不自行操作 SQL、账号会话或 Webhook 签名。**未确认的长期约束：**这些由现有导入关系体现的编排边界继续保持，而非强制改造成新的抽象架构。
