#!/usr/bin/env python3
"""判定上游是否真的抓取并「看见」https 图片（vs 内联 data: URL 被 11133 拒）。

用带唯一标记的测试图（大号数字 + 独特配色）分别以三种方式提问，比较回答：
  A. https 图片 URL
  B. 同图 data: base64 URL
  C. 不传图（基线，看模型是否会凭空编造）
"""
import base64, hashlib, json, os, random, subprocess, urllib.error, urllib.request
from PIL import Image, ImageDraw, ImageFont

DIGITS = "".join(random.choice("0123456789") for _ in range(4))
BG = (18, 24, 40)
FG = (0, 255, 170)
IMG_LOCAL = f"/root/rholin-blog/public/img/vision-probe-{DIGITS}.png"
IMG_PUB = f"https://blog.915802575.xyz/img/vision-probe-{DIGITS}.png"

# 1) 生成图片：深底 + 荧光绿大号数字，模拟"只有看图才能答对"
img = Image.new("RGB", (640, 320), BG)
d = ImageDraw.Draw(img)
font = None
for path in ("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
             "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"):
    if os.path.exists(path):
        try:
            font = ImageFont.truetype(path, 240)
            break
        except Exception:
            pass
if font is None:
    font = ImageFont.load_default()
d.text((60, 30), DIGITS, fill=FG, font=font)
d.rectangle([0, 0, 639, 319], outline=(255, 64, 64), width=8)
img.save(IMG_LOCAL)
print(f"测试图已落盘: {IMG_LOCAL}  (标记数字 {DIGITS})")

# 2) 校验公网可达（CF）
try:
    r = subprocess.run(["curl", "-s", "-o", "/dev/null", "-w", "%{http_code} %{size_download}",
                        "--max-time", "20", IMG_PUB], capture_output=True, text=True)
    print(f"公网可达性: HTTP {r.stdout.strip()}  {IMG_PUB}")
except Exception as e:
    print("curl 失败:", e)

CFG = json.load(open("/data/workbuddy2api/config.json"))
def api_key():
    for src in (CFG, CFG.get("server") or {}):
        for k in ("api_key", "apikey", "auth_key"):
            if isinstance(src.get(k), str) and src[k]:
                return src[k]
    raise SystemExit("找不到 api key")

def ask(model, content, maxtok=48):
    data = json.dumps({"model": model, "stream": False, "max_tokens": maxtok,
                       "messages": [{"role": "user", "content": content}]}).encode()
    req = urllib.request.Request("http://127.0.0.1:7863/v1/chat/completions", data=data,
                                 headers={"Authorization": "Bearer " + api_key(),
                                          "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            j = json.loads(resp.read())
        m = j["choices"][0]["message"]
        txt = (m.get("content") or "") + " | reasoning:" + str(m.get("reasoning_content") or "")[:80]
        return resp.getcode(), txt.strip()[:160], j.get("usage", {})
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:200], {}
    except Exception as e:
        return -1, str(e)[:200], {}

b64 = base64.b64encode(open(IMG_LOCAL, "rb").read()).decode()
Q = f"图片里的大号数字是多少？只回答那 4 个数字，不要其他内容。"
cases = {
    "A_https": [{"type": "text", "text": Q}, {"type": "image_url", "image_url": {"url": IMG_PUB}}],
    "B_data":  [{"type": "text", "text": Q}, {"type": "image_url", "image_url": {"url": "data:image/png;base64," + b64}}],
    "C_none":  Q,
}
for model in ("glm-5.1", "deepseek-v4.1-flash"):
    print(f"\n=== {model} （正确答案 {DIGITS}）===")
    for name, content in cases.items():
        code, txt, usage = ask(model, content)
        hit = "✓看见" if DIGITS in txt else ("✗没看见" if code == 200 else "—")
        print(f" {name:8} code={code} {hit:8} prompt_tokens={usage.get('prompt_tokens','?')} :: {txt[:110]}")
