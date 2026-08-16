#!/usr/bin/env python3
"""把被来源站风控拦下的链接，用本机真实浏览器重新抓回来并回填。

为什么这件事只能在本机做
------------------------
mp.weixin.qq.com 一类的站点按「源 IP + 请求频率」封锁，机房 IP 被无差别拦截。
生产跑在 OCI 上，实测连发两个请求就触发连接重置（约 30 分钟软封），而 jina
回退走的同样是机房出口 —— 它拿回来的就是那张验证页。所以重抓必须从住宅 IP
发起，也就是这台机器；把 agent-browser 装到生产服务器上解决不了任何问题。

为什么必须是有头浏览器
----------------------
微信的验证页能识破 headless Chrome：无头模式下点「确定」不推进，页面始终停在
wappoc_appmsgcaptcha。所以用 --headed 开真窗口，由人过一次验证；cookie 留在
具名 session 里，之后一段时间内不必再过。

回填走哪条路
------------
抓到的正文通过 /api/ingest 的 browser_capture 源回填。那条通路本来就是给浏览器
插件用的（插件在真实浏览器里抓页面再送回来），这里只是换了个浏览器驱动，后端
零改动。

用法
----
    export CAIRN_BASE_URL=https://webtag.alpenl.com
    export CAIRN_API_TOKEN=<token>          # 或用 --token

    rescue_blocked_links.py list            # 列出待救链接
    rescue_blocked_links.py rescue --all    # 逐条救
    rescue_blocked_links.py rescue <url>    # 只救指定的一条
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

# 与后端 internal/fetcher/interstitial.go 保持同一套判据。脚本这一侧的作用是
# 兜底：万一验证没真的过掉，就地拦下，绝不把验证页当正文推回库里 —— 那正是
# 这个脚本要修的问题本身。
INTERSTITIAL_URL_MARKERS = ("/mp/wappoc_appmsgcaptcha", "/mp/verifycode")
INTERSTITIAL_PHRASE_SETS = (
    ("环境异常", "去验证"),
    ("访问过于频繁",),
    ("完成验证后即可继续访问",),
    ("just a moment", "cloudflare"),
    ("attention required", "cloudflare"),
)
INTERSTITIAL_MAX_CHARS = 1200

# 后端把这类失败归到 blocked_by_origin（errsafe.ErrBlockedByOrigin）。历史上
# 在修复之前入库的那些，status 是 done、正文是验证页，只能靠分类器留下的
# captcha_verification_wall 认出来。
BLOCKED_ERROR_PREFIX = "blocked_by_origin"
CAPTCHA_CLASSIFICATION = "captcha_verification_wall"

DEFAULT_SESSION = "cairn-rescue"
VERIFY_POLL_SECONDS = 3


class RescueError(RuntimeError):
    """脚本自身可以解释清楚的失败，区别于未预料的异常。"""


# ---------------------------------------------------------------- agent-browser


def browser(*args: str, timeout: int = 120) -> str:
    """调用 agent-browser 并返回 stdout。失败时带上 stderr，便于定位。"""
    proc = subprocess.run(
        ["agent-browser", *args],
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()
        raise RescueError(f"agent-browser {' '.join(args)} 失败: {detail}")
    return proc.stdout.strip()


def browser_eval(session: str, script: str) -> object:
    """在页面里执行 JS 并把结果解析成 Python 对象。

    agent-browser 回显的是 JSON 字面量（字符串会带引号），所以先 json 解一层；
    我们的脚本统一返回 JSON 字符串，于是需要再解一层。
    """
    raw = browser("eval", script, "--session", session)
    try:
        value = json.loads(raw)
    except json.JSONDecodeError:
        return raw
    if isinstance(value, str):
        try:
            return json.loads(value)
        except json.JSONDecodeError:
            return value
    return value


# 抽正文。优先用微信的 #js_content —— 那是公众号文章的正文容器；拿不到再退回
# 通用的 article / main，最后才是 body。返回 JSON 字符串，避免 CLI 层做转义。
EXTRACT_JS = """(() => {
  const pick = () =>
    document.querySelector('#js_content') ||
    document.querySelector('article') ||
    document.querySelector('main') ||
    document.body;
  const node = pick();
  const title =
    (document.querySelector('#activity-name') || {}).textContent ||
    document.title ||
    '';
  return JSON.stringify({
    url: location.href,
    title: title.trim(),
    text: (node ? node.innerText : '').trim(),
    html: node ? node.innerHTML : '',
  });
})()"""


def looks_like_interstitial(url: str, title: str, text: str) -> bool:
    """与后端同判据：URL 命中即可定案，否则要求「短」且命中一整组关键词。"""
    lowered_url = (url or "").lower()
    if any(marker in lowered_url for marker in INTERSTITIAL_URL_MARKERS):
        return True
    if len(text or "") > INTERSTITIAL_MAX_CHARS:
        return False
    haystack = f"{title}\n{text}".lower()
    return any(
        all(phrase in haystack for phrase in phrases)
        for phrases in INTERSTITIAL_PHRASE_SETS
    )


# ---------------------------------------------------------------------- cairn API


def api_request(base: str, token: str, path: str, payload: dict | None = None) -> dict:
    url = f"{base.rstrip('/')}{path}"
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(
        url,
        data=data,
        method="POST" if data else "GET",
        headers={
            "Authorization": f"Bearer {token}",
            **({"Content-Type": "application/json"} if data else {}),
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode(errors="replace")[:400]
        raise RescueError(f"{path} 返回 {exc.code}: {body}") from exc
    except urllib.error.URLError as exc:
        raise RescueError(f"连不上 {url}: {exc.reason}") from exc


def find_blocked(base: str, token: str, limit: int = 200) -> list[dict]:
    """列出需要救援的条目，覆盖阅读库与收件箱两侧。

    三类来源：
      - 阅读库里 blocked_by_origin 的失败（修复后新产生的）
      - 阅读库里已被验证页污染、却以 status=done 存进去的历史记录
      - 收件箱里提案失败的条目（收藏默认进收件箱，所以新的拦截多半落在这边）

    每条都带上 destination，标记它原本在哪。回填时按原位置送，才是就地修复而不是
    在另一侧另起一条 —— 实测同一 URL 走 /api/ingest 会命中既有记录并原地更新。
    """
    blocked: list[dict] = []

    links = api_request(base, token, f"/api/links?limit={limit}")
    for item in links.get("items", []):
        error_msg = item.get("error_msg") or ""
        reason = item.get("classification_reason") or ""
        if error_msg.startswith(BLOCKED_ERROR_PREFIX):
            blocked.append({**item, "why": "阅读库：抓取被拦截", "destination": "library"})
        elif reason == CAPTCHA_CLASSIFICATION:
            blocked.append({**item, "why": "阅读库：正文其实是验证页", "destination": "library"})

    # 收件箱条目只暴露 proposal_status，没有错误原因字段。failed 就是信号；
    # 如果它其实是别的原因失败的，抽取后的守卫会拦下来，不会污染库。
    inbox = api_request(base, token, f"/api/inbox?limit={limit}")
    for item in inbox.get("items", []):
        if item.get("proposal_status") == "failed":
            blocked.append({**item, "why": "收件箱：提案失败", "destination": "inbox"})

    return blocked


def push_capture(base: str, token: str, captured: dict, source_url: str, destination: str) -> dict:
    """把浏览器抓到的正文按 browser_capture 送回后端。

    destination 显式传入而不是省略：省略会落到服务端缺省（收件箱），对一条本来
    就在阅读库里、只是正文被验证页顶替掉的记录来说，那等于在收件箱另起一条，
    而坏记录还留在库里。按原位置送才能就地修好。
    """
    return api_request(
        base,
        token,
        "/api/ingest",
        {
            "destination": destination,
            "sources": [
                {
                    "kind": "browser_capture",
                    "url": source_url,
                    "title": captured.get("title", "")[:512],
                    "text": captured.get("text", "")[:512 * 1024],
                    "metadata": {
                        "rescued_by": "rescue_blocked_links.py",
                        "captured_url": captured.get("url", ""),
                    },
                }
            ],
        },
    )


# ------------------------------------------------------------------- rescue flow


def rescue_one(base: str, token: str, url: str, session: str, wait_seconds: int, destination: str) -> str:
    """打开一条链接、必要时等人过验证、抽正文、回填。返回一行结果描述。"""
    browser("open", url, "--headed", "--session", session, timeout=180)

    deadline = time.time() + wait_seconds
    prompted = False
    while True:
        captured = browser_eval(session, EXTRACT_JS)
        if not isinstance(captured, dict):
            raise RescueError(f"抽取脚本返回了意外结果: {captured!r}")

        if not looks_like_interstitial(
            captured.get("url", ""), captured.get("title", ""), captured.get("text", "")
        ):
            break

        if not prompted:
            print(
                f"    ⚠ 停在验证页，请在弹出的浏览器窗口里完成验证"
                f"（最多等 {wait_seconds} 秒）…",
                flush=True,
            )
            prompted = True
        if time.time() > deadline:
            raise RescueError("等待验证超时，仍停在验证页；本条跳过，未回填")
        time.sleep(VERIFY_POLL_SECONDS)

    text = captured.get("text", "")
    if len(text) < 200:
        raise RescueError(f"抽到的正文只有 {len(text)} 字，判为无效，未回填")

    result = push_capture(base, token, captured, url, destination)
    target = result.get("inbox_id") or result.get("link_id") or "?"
    return f"已回填 {len(text)} 字 → {result.get('destination', '?')} {target}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base-url", default=os.environ.get("CAIRN_BASE_URL", ""))
    parser.add_argument("--token", default=os.environ.get("CAIRN_API_TOKEN", ""))
    parser.add_argument("--session", default=DEFAULT_SESSION, help="agent-browser 会话名，cookie 存在这里")
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("list", help="列出被风控拦下、需要救援的链接")

    rescue = sub.add_parser("rescue", help="重抓并回填")
    rescue.add_argument("urls", nargs="*", help="要救的 URL；不给则配合 --all")
    rescue.add_argument("--all", action="store_true", help="救所有被拦下的链接")
    rescue.add_argument("--wait", type=int, default=180, help="每条等待人工过验证的秒数上限")

    args = parser.parse_args()
    if not args.base_url or not args.token:
        print("需要 CAIRN_BASE_URL 与 CAIRN_API_TOKEN（或 --base-url / --token）", file=sys.stderr)
        return 2

    try:
        if args.command == "list":
            blocked = find_blocked(args.base_url, args.token)
            if not blocked:
                print("没有需要救援的链接。")
                return 0
            print(f"{len(blocked)} 条待救：")
            for item in blocked:
                print(f"  [{item['why']}] {item.get('url', '')}")
            return 0

        # 已知条目按它原本所在的位置回填（就地修复）。命令行显式给的 URL 若不在
        # 已知列表里，就是一次全新的收藏，按服务端缺省进收件箱。
        known = {i.get("url", ""): i.get("destination", "library") for i in find_blocked(args.base_url, args.token)}
        targets: dict[str, str] = {}
        if args.all:
            targets.update(known)
        for url in args.urls:
            targets[url] = known.get(url, "inbox")
        targets.pop("", None)
        if not targets:
            print("没有指定要救的链接（给 URL，或加 --all）。")
            return 0

        failures = 0
        for index, (url, destination) in enumerate(targets.items(), 1):
            print(f"[{index}/{len(targets)}] {url} → {destination}")
            try:
                print(f"    ✓ {rescue_one(args.base_url, args.token, url, args.session, args.wait, destination)}")
            except (RescueError, subprocess.TimeoutExpired) as exc:
                failures += 1
                print(f"    ✗ {exc}")
        return 1 if failures else 0
    except RescueError as exc:
        print(f"错误: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
