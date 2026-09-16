#!/usr/bin/env python3
"""端到端复验：与用户被拒请求同量级（~1.7MB）的 payload 是否还会被本地预检误杀。

设计上故意比用户真实请求**更严苛**：
  - 用户真实 1.68MB 里绝大部分是 base64 图片（上游按图计，估算器给 12 bytes/token 的宽松系数）；
  - 这里把图片压到 ~1.2MB，其余 ~0.5MB 用纯英文/代码文本填充（估算器对文本最严，4.5 bytes/token）。
所以只要这个能过，用户那种图片占大头的请求必然能过。

判定：不得出现网关预检的 context_length_exceeded(估算约...) 400。
"""
import base64, json, os, sys, time, urllib.error, urllib.request

GATEWAY = "http://127.0.0.1:7863/v1/chat/completions"
CFG = json.load(open("/data/workbuddy2api/config.json"))


def api_key() -> str:
    for src in (CFG, CFG.get("server") or {}):
        for k in ("api_key", "apikey", "auth_key"):
            if isinstance(src.get(k), str) and src[k]:
                return src[k]
    k = os.environ.get("WB2_API_KEY")
    if not k:
        raise SystemExit("找不到 api_key")
    return k


def png_b64(path: str, target_bytes: int) -> str:
    """读真实 PNG → base64；不足 target_bytes 时用随机字节补足（模拟大图）。"""
    raw = open(path, "rb").read()
    if len(raw) < target_bytes:
        raw = raw + os.urandom(target_bytes - len(raw))
    else:
        raw = raw[:target_bytes]
    return "data:image/png;base64," + base64.b64encode(raw).decode()


PNG = "/root/rholin-blog/static/img/covers/expect-与-systemctl-自动化脚本.png"


def build(model: str) -> tuple[str, dict]:
    img = png_b64(PNG, 900_000)          # → base64 ≈1.2MB
    text = ("def handler(req):\n    # 复刻用户长上下文：工具说明 + 代码 + 分析\n"
            "    return process(req)\n") * 4_500   # ≈0.5MB 文本
    payload = {
        "model": model,
        "stream": False,
        "max_tokens": 24,
        "messages": [
            {"role": "system", "content": "You are a coding agent. 复验上下文预算预检。"},
            {"role": "user", "content": [
                {"type": "text", "text": text},
                {"type": "image_url", "image_url": {"url": img}},
            ]},
        ],
    }
    body = json.dumps(payload, ensure_ascii=False).encode()
    return body.decode(), payload


def send(name: str, model: str, key: str):
    _, payload = build(model)
    data = json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(GATEWAY, data=data, headers={
        "Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    t0 = time.time()
    code, text = -1, ""
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            code, text = r.getcode(), r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        code, text = e.code, e.read().decode("utf-8", "replace")
    except Exception as e:
        code, text = -1, f"{type(e).__name__}: {e}"
    dt = time.time() - t0
    guard = ("估算约" in text) or ("context_length_exceeded" in text and "上限" in text)
    ok = "✅ 放行" if not guard else "❌ 仍被本地预检误杀"
    print(f"[{name}] model={model}")
    print(f"   bytes={len(data)}  ({len(data)/1048576:.2f}MB)  code={code}  {dt:.1f}s")
    print(f"   {ok}  guard_reject={guard}")
    print(f"   resp: {text[:280].replace(chr(10),' ')}")
    print()
    return code, guard


if __name__ == "__main__":
    key = api_key()
    results = []
    for model in ["deepseek-v4.1-flash", "glm-5.1", "gpt-5.3-codex"]:
        results.append(send(model, model, key))
        time.sleep(2)
    bad = [r for r in results if r[1]]
    print("=" * 60)
    print("结论：" + ("全部放行，无本地误杀 ✅" if not bad else f"{len(bad)} 个仍被误杀 ❌"))
    sys.exit(1 if bad else 0)
