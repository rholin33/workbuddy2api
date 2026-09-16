#!/usr/bin/env python3
"""隔离探针：确认上游 11133 model_param_invalid 的诱因，并顺带探真实上下文余量。

用例（同一模型逐一打）：
  A. 小文本 + 图片 content part   → 看是否图片不被支持
  B. 小文本，无图片               → 基线
  C. 1.6MB 纯文本                 → 估算 ~360k token，验证大上下文是否真能过
"""
import base64, json, os, time, urllib.error, urllib.request

GATEWAY = "http://127.0.0.1:7863/v1/chat/completions"
CFG = json.load(open("/data/workbuddy2api/config.json"))
PNG = "/root/rholin-blog/static/img/covers/expect-与-systemctl-自动化脚本.png"


def api_key():
    for src in (CFG, CFG.get("server") or {}):
        for k in ("api_key", "apikey", "auth_key"):
            if isinstance(src.get(k), str) and src[k]:
                return src[k]
    return os.environ["WB2_API_KEY"]


def post(model, messages, key, maxtok=16, timeout=300):
    payload = {"model": model, "stream": False, "max_tokens": maxtok, "messages": messages}
    data = json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(GATEWAY, data=data, headers={
        "Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            code, text = r.getcode(), r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        code, text = e.code, e.read().decode("utf-8", "replace")
    except Exception as e:
        code, text = -1, f"{type(e).__name__}: {e}"
    dt = time.time() - t0
    usage = ""
    try:
        u = json.loads(text).get("usage") or {}
        if u:
            usage = f" prompt_tokens={u.get('prompt_tokens')}"
    except Exception:
        pass
    err = ""
    if code != 200:
        try:
            e = json.loads(text)["error"]
            err = f" {e.get('code')}: {str(e.get('message'))[:200]}"
        except Exception:
            err = " " + text[:200].replace("\n", " ")
    print(f"  code={code} {dt:5.1f}s {len(data)/1048576:5.2f}MB{usage}{err}")
    return code, text


def b64png(target=200_000):
    raw = open(PNG, "rb").read()[:target]
    return "data:image/png;base64," + base64.b64encode(raw).decode()


if __name__ == "__main__":
    key = api_key()
    img = b64png()
    txt = "用一句话说明这个函数做什么。"

    for model in ["deepseek-v4.1-flash", "gpt-5.3-codex", "glm-5.1"]:
        print(f"\n=== {model} ===")
        print(" A) 文本+图片:", end="", flush=True)
        post(model, [{"role": "user", "content": [
            {"type": "text", "text": txt},
            {"type": "image_url", "image_url": {"url": img}}]}], key)
        time.sleep(1)
        print(" B) 纯文本基线:", end="", flush=True)
        post(model, [{"role": "user", "content": txt}], key)
        time.sleep(1)
        print(" C) 1.6MB 纯文本:", end="", flush=True)
        big = ("def f(x):\n    return x * 2  # 长上下文探针\n" * 40_000)
        post(model, [{"role": "user", "content": big + "\n上面重复了多少次？"}], key)
        time.sleep(2)
