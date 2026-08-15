#!/usr/bin/env python3
"""grok_fetch_probe — 探测 grok2api（xAI）端点对各站点 URL 的直接抓取能力。

背景：grok 模型自带联网抓取，能直接读取 URL 原文（实测 example.com / github /
go.dev 博客标题正文均与真实一致）。本脚本批量探测一组代表性站点，判定每个站点
grok 能否直接抓取，用来决定 webtag「grok 直连抓取」与「本地抓取器回退」的边界。

判定方式：让 grok 抓取 URL 并只输出 JSON {accessible, title, excerpt}；脚本汇总成
✅/❌ 表。accessible=false 表示被登录墙/反爬/404 挡住——这些站点需要本地抓取器回退。

用法（端点/key 从环境变量读，勿硬编码真实凭据）：
    AI_BASE_URL=http://<host>:8000/v1 AI_API_KEY=<key> \
    AI_MODEL=grok-4.3-fast python3 scripts/grok_fetch_probe.py

不进 CI（依赖外部端点+真实网络+成本）；按需手动跑，扩 URL 清单即可。
"""
import concurrent.futures
import json
import os
import re
import sys
import time
import urllib.request

BASE = os.environ.get("AI_BASE_URL", "http://localhost:8000/v1").rstrip("/") + "/chat/completions"
KEY = os.environ.get("AI_API_KEY", "")
MODEL = os.environ.get("AI_MODEL", "grok-4.3-fast")

if not KEY:
    sys.exit("set AI_API_KEY (and AI_BASE_URL/AI_MODEL) env vars; no credentials are hardcoded")

# 代表性 URL 清单：覆盖国内/国外社区、问答、博客、视频、论文、社交。
# 标注 expect 为已知预期（emp = 实测，2026-06-15）：方便回归对比。
URLS = [
    ("知乎",       "https://www.zhihu.com/question/19550225"),
    ("v2ex",      "https://www.v2ex.com/t/1000000"),
    ("linux.do",  "https://linux.do/t/topic/250000"),
    ("CSDN首页",   "https://www.csdn.net"),
    ("CSDN文章",   "https://blog.csdn.net/weixin_44799217/article/details/123060889"),
    ("掘金",       "https://juejin.cn"),
    ("SegmentFault", "https://segmentfault.com"),
    ("微信公号",    "https://mp.weixin.qq.com/s/Bki5jUZ_5Y0Rj0sN0z0Y0A"),
    ("StackOverflow", "https://stackoverflow.com/questions/11227809"),
    ("HackerNews", "https://news.ycombinator.com/item?id=1"),
    ("reddit",    "https://www.reddit.com/r/golang/"),
    ("medium",    "https://medium.com/@kentbeck_7670"),
    ("B站",       "https://www.bilibili.com/video/BV1uv411q7Mv"),
    ("twitter/X", "https://twitter.com/golang"),
    ("维基",       "https://en.wikipedia.org/wiki/Go_(programming_language)"),
    ("arxiv",     "https://arxiv.org/abs/1706.03762"),
]

INSTR = ('抓取该网页。只输出 JSON：{"accessible":true,"title":"真实标题","excerpt":"正文前60字"}。'
         '无法访问/登录墙/反爬/404 则 accessible=false。URL: ')


def probe(cat, url):
    body = json.dumps({
        "model": MODEL, "stream": False, "temperature": 0, "max_tokens": 400,
        "messages": [{"role": "user", "content": INSTR + url}],
    }).encode()
    req = urllib.request.Request(
        BASE, data=body,
        headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"},
    )
    t0 = time.time()
    try:
        content = json.loads(urllib.request.urlopen(req, timeout=90).read().decode())
        text = content["choices"][0]["message"]["content"]
        match = re.search(r"\{.*\}", text, re.S)
        data = json.loads(match.group(0)) if match else {}
        mark = "OK" if data.get("accessible") else "NO"
        return (cat, mark, round(time.time() - t0, 1), str(data.get("title", ""))[:42], str(data.get("excerpt", ""))[:38])
    except Exception as exc:  # noqa: BLE001 - probe 容错，任何异常都记为不可用
        return (cat, "ERR", round(time.time() - t0, 1), "ERR " + str(exc)[:30], "")


def main():
    rows = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        for row in pool.map(lambda a: probe(*a), URLS):
            rows.append(row)
    ok = sum(1 for r in rows if r[1] == "OK")
    print(f"endpoint={BASE} model={MODEL}")
    print(f"{'站点':14s} {'':3s} {'秒':>5s}  标题 / 正文开头")
    print("-" * 100)
    for cat, mark, dt, title, exc in rows:
        print(f"{cat:14s} {mark:3s} {dt:>5}  {title}  |  {exc}")
    print("-" * 100)
    print(f"可直接抓取：{ok}/{len(rows)}")


if __name__ == "__main__":
    main()
