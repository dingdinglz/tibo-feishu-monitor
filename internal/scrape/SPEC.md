---
id: tibo-scrape
type: module-design
status: draft
title: X 采集子进程桥接
parent: tibo-architecture
depends-on: [tibo-model, tibo-python-fetch]
---

# X 采集子进程桥接

## 职责

每次抓取启动一个受时限控制的 Python 子进程，以 stdin 发送单个 JSON 请求，从 stdout 读取版本化结果。依赖 `tibo-python-fetch` 是进程协议边，不是 Go 导入。输出有大小上限，结果经类型、作者、时间、数量及错误码校验后才可交给编排层；帖子与上下文链接从已验证字段重建。

丢弃原始 stderr，不把上游账号/代理异常带入日志。取消时先发 SIGTERM，让 Python 释放其资源，并设置有界退出等待。

## 边界

桥接可返回有效部分数据及错误，不能把子进程退出、缺失有效头页或解析异常变成“没有新增”。完整覆盖与持久游标推进由 `tibo-architecture` 的采集契约约束，不由进程正常退出单独证明。

不直接访问账号库内容、业务库或飞书，也不实现时间线解析。**未确认的长期约束（由当前分工推断）：**保留一次请求/一次进程的协议边界，不将 Python 变成常驻服务。
