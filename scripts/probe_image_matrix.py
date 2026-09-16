#!/usr/bin/env python3
"""受控矩阵：把「URL 形式（https/data）」与「图片是否有效」彻底分开，定位 11133 的真正诱因。

变体：
 V1 有效小图 + data URL
 V2 有效小图 + https URL
 V3 截断图（前 200KB 字节，非完整 PNG）+ data URL   ← 上一轮误判的元凶
 V4 有效图 + 尾部填充垃圾字节（解码后仍含完整 PNG）+ data URL
 V5 纯随机 base64 垃圾（根本不是图片）+ data URL
"""
import base64, json, os, random, subprocess, urllib.error, urllib.request

PNG_SMALL = "/root/rholin-blog/public/img/vision-probe-9883.png"   # 有效完整 PNG(~14KB)
PNG_BIG = "/root/rholin-blog/static/img/covers/expect-与-systemctl-自动化脚本.png"

def b64(path, truncate=None, pad=0, garbage=False):
    if garbage:
        raw = bytes(random.getrandbits(8) for _ in range(300_000))
    else:
        raw = open(path, "rb").read()
        if truncate:
            raw = raw[:truncate]
        if pad:
            raw = raw + bytes(random.getrandbits(8) for _ in range(pad))
    return "data:image/png;base64," + base64.b64encode(raw).decode()

URL_SMALL = "https://blog.915802575.xyz/img/vision-probe-9883.png"

CFG = json.load(open("/data/workbuddy2api/config.json"))
KEY = next(v for src in (CFG, CFG.get("server") or {})
           for k in ("api_key", "apikey", "auth_key")
           if isinstance((v := src.get(k)), str) and v)

def ask(model, url):
    data = json.dumps({"model": model, "stream": False, "max_tokens": 24, "messages": [
        {"role": "user", "content": [{"type": "text", "text": "描述这张图。"},
                                     {"type": "image_url", "image_url": {"url": url}}]}]}).encode()
    req = urllib.request.Request("http://127.0.0.1:7863/v1/chat/completions", data=data,
                                 headers={"Authorization": "Bearer " + KEY,
                                          "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            return r.getcode(), f"ok size={len(data)}", json.loads(r.read()).get("usage", {})
    except urllib.error.HTTPError as e:
        txt = e.read().decode()[:300]
        code = ""
        try:
            code = json.loads(txt)["error"].get("message", "")[:150]
        except Exception:
            code = txt.replace("\n", " ")[:150]
        return e.code, f"size={len(data)} {code}", {}
    except Exception as e:
        return -1, str(e)[:150], {}

variants = [
    ("V1 有效小图 data:URL", b64(PNG_SMALL)),
    ("V2 有效小图 https:", URL_SMALL),
    ("V3 截断图(200KB) data:URL", b64(PNG_BIG, truncate=200_000)),
    ("V4 有效图+尾部垃圾 data:URL", b64(PNG_SMALL, pad=1_500_000)),
    ("V5 纯随机垃圾 data:URL", b64(None, garbage=True)),
]
for model in ("deepseek-v4.1-flash", "gpt-5.3-codex"):
    print(f"\n=== {model} ===")
    for name, url in variants:
        code, info, usage = ask(model, url)
        print(f" {name:28} code={code} pt={usage.get('prompt_tokens', '?')} :: {info[:150]}")
