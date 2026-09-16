#!/usr/bin/env python3
"""用被拒的真实请求体做端到端复验（本地预检 → 上游），不打印任何内容/密钥。

判断标准：**不得**出现网关侧的 context_length_exceeded 预检 400。
（上游自身报错属正常——说明预检已放行。）
"""
import json, os, re, time, urllib.request

GATEWAY = "http://127.0.0.1:7863/v1/chat/completions"
CFG = json.load(open("/data/workbuddy2api/config.json"))


def api_key() -> str:
    for k in ("api_key", "apikey", "auth_key"):
        if isinstance(CFG.get(k), str) and CFG[k]:
            return CFG[k]
    srv = CFG.get("server") or {}
    for k in ("api_key", "apikey"):
        if isinstance(srv.get(k), str) and srv[k]:
            return srv[k]
    env = os.environ.get("WB2_API_KEY", "")
    if env:
        return env
    raise SystemExit("找不到 api_key（检查 config.json 或导出 WB2_API_KEY）")


def extract_body(path: str) -> dict | None:
    """从 CPA error dump 里取出客户端请求体 JSON。"""
    raw = open(path, encoding="utf-8", errors="replace").read()
    m = re.search(r"=== REQUEST BODY ===\s*", raw)
    if not m:
        return None
    seg = raw[m.end():]
    nxt = re.search(r"\n=== ", seg)
    if nxt:
        seg = seg[: nxt.start()]
    try:
        return json.loads(seg)
    except Exception:
        return None


def to_chat_messages(body: dict) -> list:
    """把 /v1/responses 风格 input 或 chat messages 统一成 chat messages（保留文本与图片）。"""
    items = body.get("messages") or body.get("input") or []
    out = []
    for it in items if isinstance(items, list) else []:
        if not isinstance(it, dict):
            continue
        role = it.get("role") or "user"
        content = it.get("content")
        if isinstance(content, str):
            out.append({"role": role, "content": content})
            continue
        parts = []
        for c in content if isinstance(content, list) else []:
            if not isinstance(c, dict):
                continue
            t = c.get("type")
            if t in ("input_text", "text", "output_text", "summary_text"):
                parts.append({"type": "text", "text": c.get("text") or c.get("content") or ""})
            elif t in ("input_image", "image_url"):
                u = c.get("image_url") or c.get("url")
                if isinstance(u, dict):
                    u = u.get("url")
                if u:
                    parts.append({"type": "image_url", "image_url": {"url": u}})
            elif t == "reasoning":
                pass
            elif isinstance(c.get("text"), str):
                parts.append({"type": "text", "text": c["text"]})
        if parts:
            out.append({"role": role, "content": parts})
    return out


def probe(name: str, model: str, msgs: list, key: str):
    payload = {"model": model, "messages": msgs, "stream": False, "max_tokens": 16}
    data = json.dumps(payload).encode()
    req = urllib.request.Request(GATEWAY, data=data, headers={
        "Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            code, text = r.getcode(), r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        code, text = e.code, e.read().decode("utf-8", "replace")
    except Exception as e:  # 网络/超时
        code, text = -1, f"{type(e).__name__}: {e}"
    dt = time.time() - t0
    snippet = text[:400].replace("\n", " ")
    guard = "context_length_exceeded" in text and "估算约" in text
    print(f"[{name}] model={model} bytes={len(data)} code={code} {dt:.1f}s guard_reject={guard}")
    print(f"      ↑ {snippet}")
    return code, guard


def main():
    key = api_key()
    dumps = sorted(
        [os.path.join("/data/outdata/CPA/logs", f)
         for f in os.listdir("/data/outdata/CPA/logs") if f.startswith("error-v1-chat-completions")],
        key=os.path.getmtime)
    print(f"找到 {len(dumps)} 个 dump\n")
    for d in dumps[-3:]:
        body = extract_body(d)
        if not body:
            print(f"[跳过] 无法解析: {os.path.basename(d)}")
            continue
        msgs = to_chat_messages(body)
        if not msgs:
            print(f"[跳过] 无可用消息: {os.path.basename(d)}")
            continue
        model = body.get("model") or "deepseek-v4.1-flash"
        name = os.path.basename(d)[:34]
        # 估算一下本地预检会算成多少（与网关同口径：仅打印数量级）
        probe(name, model, msgs, key)
        print()
        time.sleep(1)


if __name__ == "__main__":
    main()
