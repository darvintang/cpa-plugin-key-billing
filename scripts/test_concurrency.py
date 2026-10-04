#!/usr/bin/env python3
# Created by Darvin.
"""同时发送流式请求，验证插件并发拒绝。API Key 仅从环境变量读取。

示例：CPA_API_KEY 已设置后，运行：
  python3 scripts/test_concurrency.py --base-url http://127.0.0.1:8317 \
      --model gpt-6-luna --concurrency 10 --rounds 3 --expect-limit credential

凭证上限作用于每个凭证，多个可用凭证会分摊请求；要验证单个上限，先将
测试 API Key 的路由限定到一个凭证。API Key 上限用 --expect-limit key。
脚本不会修改配置。实际请求消耗额度；并发数应大于设置的上限。
"""

import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPException
import json
import os
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlparse
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

LIMIT_CODES = {"credential": "credential_concurrency_limit", "key": "api_key_concurrency_exceeded"}


class NoRedirect(HTTPRedirectHandler):
    # Never forward an API Key to a redirected origin.
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def send(args, key, barrier):
    if args.endpoint == "responses":
        payload = {"model": args.model, "input": args.prompt, "max_output_tokens": args.max_tokens, "stream": True}
    else:
        payload = {"model": args.model, "messages": [{"role": "user", "content": args.prompt}], "max_tokens": args.max_tokens, "stream": True}
    request = Request(args.base_url.rstrip("/") + "/v1/" + args.endpoint,
                      json.dumps(payload).encode(),
                      {"Authorization": "Bearer " + key, "Content-Type": "application/json"})
    # A shared explicit identity exercises the plugin-owned sticky reservation.
    if args.session_id:
        request.add_header("Session-Id", args.session_id)
    barrier.wait()
    start = time.monotonic()
    status, outcome = 0, "network_error"
    try:
        # Disable proxy inheritance so localhost load does not go through a system proxy.
        with build_opener(ProxyHandler({}), NoRedirect()).open(request, timeout=args.timeout) as response:
            status = response.status
            completed, failed = False, False
            for raw in response:
                line = raw.decode("utf-8", errors="replace").strip()
                if line == "event: error":
                    failed = True
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    completed = completed or args.endpoint == "chat/completions"
                    continue
                try:
                    event = json.loads(data)
                    if not isinstance(event, dict):
                        failed = True
                        continue
                    kind = event.get("type", "")
                    result = event.get("response") or {}
                    failed = failed or bool(event.get("error")) or kind in ("error", "response.failed", "response.incomplete")
                    if kind == "response.completed":
                        completed = result.get("status", "completed") == "completed" and not result.get("error")
                except (ValueError, AttributeError):
                    failed = True
            outcome = "success" if completed and not failed else "stream_error"
    except HTTPError as error:
        status = error.code
        # Print only recognized codes, never raw error bodies, prompts, or credentials.
        outcome = "http_error"
        try:
            body = json.loads(error.read(65536))
            detail = body.get("error", {})
            code = detail.get("code")
            # CPA wraps scheduler refusals; distinguish both concurrency messages from quota 429s.
            if code == "rate_limit_exceeded":
                message = str(detail.get("message", ""))
                if message == "All eligible credentials are at their concurrency limit":
                    code = LIMIT_CODES["credential"]
                elif message.startswith("API key concurrency limit reached:"):
                    code = LIMIT_CODES["key"]
            if status == 429 and code in LIMIT_CODES.values():
                outcome = code
            elif status == 429:
                outcome = "other_429"
        except (ValueError, AttributeError):
            pass
        finally:
            error.close()
    except (URLError, OSError, ValueError, HTTPException):
        pass
    return outcome, status, time.monotonic() - start


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base-url", default="http://127.0.0.1:8317", help="CPA 地址，不带 /v1")
    parser.add_argument("--model", required=True)
    parser.add_argument("--session-id", default="", help="所有请求共用会话标识，用于验证粘性预留槽")
    parser.add_argument("--concurrency", type=int, default=10, help="每轮同时发送的请求数")
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--expect-limit", choices=LIMIT_CODES, default="credential", help="验证凭证或 API Key 上限")
    parser.add_argument("--api-key-env", default="CPA_API_KEY", help="包含下游 API Key 的环境变量名称")
    parser.add_argument("--endpoint", choices=("responses", "chat/completions"), default="responses")
    parser.add_argument("--timeout", type=float, default=120)
    parser.add_argument("--max-tokens", type=int, default=512)
    parser.add_argument("--prompt", default="Write a numbered list of 100 short facts about computers.", help="使用较长回答保持请求重叠")
    args = parser.parse_args()
    url = urlparse(args.base_url)
    if url.scheme not in ("http", "https") or not url.netloc or url.username or url.password or url.query or url.fragment or url.path not in ("", "/"):
        parser.error("--base-url 必须为 HTTP(S) origin，例如 http://127.0.0.1:8317")
    if min(args.concurrency, args.rounds, args.max_tokens, args.timeout) <= 0:
        parser.error("并发数、轮数、token 上限和超时必须大于 0")
    if any(ord(c) < 32 or ord(c) > 126 for c in args.session_id):
        parser.error("--session-id 只能包含可打印 ASCII 字符")
    key = os.environ.get(args.api_key_env, "").strip()
    if not key:
        parser.error("请先设置环境变量 " + args.api_key_env)
    counts = Counter()
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        for round_number in range(1, args.rounds + 1):
            barrier = threading.Barrier(args.concurrency)
            futures = [pool.submit(send, args, key, barrier) for _ in range(args.concurrency)]
            batch = Counter()
            for future in futures:
                outcome, status, elapsed = future.result()
                batch[outcome] += 1
                print(f"第 {round_number} 轮 HTTP={status} {outcome} {elapsed:.2f}s")
            counts.update(batch)
    print("汇总：" + json.dumps(dict(counts), ensure_ascii=False, sort_keys=True))
    expected = LIMIT_CODES[args.expect_limit]
    if counts[expected] and counts["success"] and sum(counts.values()) == counts[expected] + counts["success"]:
        print("已验证：出现插件目标并发拒绝，且有请求正常完成。请结合凭证并发显示确认数值上限。")
        return 0
    print("未验证：没有同时得到正常完成和目标并发拒绝，或存在其他失败。检查路由、上限、请求时长和汇总。")
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
