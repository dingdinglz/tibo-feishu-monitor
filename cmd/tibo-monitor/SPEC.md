---
id: tibo-cli
type: module-design
status: draft
title: 命令入口与运行装配
parent: tibo-architecture
depends-on: [tibo-config, tibo-lock, tibo-model, tibo-monitor, tibo-store, tibo-scrape, tibo-codex-resets, tibo-feishu, tibo-typesafe]
---

# 命令入口与运行装配

## 职责

拥有命令解析、启动检查、资源生命周期和具体适配器装配。`run` 才建立业务库、同步目标并启动编排；`status` 只读已存在业务库；`probe` 读取来源但不创建业务任务；`test-send` 必须同时指定目标和确认参数。

X 模式检查 Python、脚本和账号库，并持有账号库锁；API 模式不做这些检查，也不装配推理客户端。启动时先取得业务库锁，再打开库和恢复队列。SIGINT/SIGTERM 通过共享 context 取消工作。

## 边界

这是具体实现的装配点，不拥有分页、去重或重试算法；这些运行契约见 `tibo-architecture`。探测对业务库只读不意味着账号库只读：twscrape 仍可更新自身使用统计和限流状态。

**未确认的长期约束（由现有代码推断）：**外部适配器选择与资源所有权继续集中在入口层，业务包不反向依赖命令包。
