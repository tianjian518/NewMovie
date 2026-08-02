#!/bin/sh
# NewMovie 2.0 容器入口：准备内置 OpenList（139cas）的账号，然后交给 supervisor 跑双进程。
#
# 零配置的关键在这里：首次启动时给内置 OpenList 设一个随机管理员密码并写进
# /data/openlist_admin_pass，NewMovie 启动后读它自动登录换 Token。
# 用户从头到尾不需要碰任何 Token。
set -e

DATA_DIR="${VIDRIVE_DATA:-/data}"
OL_DATA="$DATA_DIR/openlist"
OL_BIN="/app/openlist/openlist"
PASS_FILE="$DATA_DIR/openlist_admin_pass"

mkdir -p "$OL_DATA" "$DATA_DIR/cache"

# ---- 端口适配：兼容只给一个端口的面板（HF Space / Railway 等）----
if [ -n "$PORT" ] && [ -z "$VIDRIVE_ADDR" ]; then
  export VIDRIVE_ADDR=":$PORT"
  echo "[entrypoint] PORT=$PORT -> VIDRIVE_ADDR=$VIDRIVE_ADDR"
fi

# ---- 内置 OpenList 管理员密码 ----
# 用户可以用 NEWMOVIE_BUNDLED_PASS 显式指定；否则首次启动生成随机密码。
if [ -n "$NEWMOVIE_BUNDLED_PASS" ]; then
  echo "[entrypoint] 使用环境变量指定的内置网盘管理员密码"
  printf '%s' "$NEWMOVIE_BUNDLED_PASS" > "$PASS_FILE"
  chmod 600 "$PASS_FILE"
  NEED_SET_PASS=1
elif [ -f "$PASS_FILE" ] && [ -s "$PASS_FILE" ]; then
  echo "[entrypoint] 复用已保存的内置网盘管理员密码"
  NEED_SET_PASS=0
else
  echo "[entrypoint] 首次启动：为内置网盘生成随机管理员密码"
  # 用 openlist 自己的随机源不方便，这里用 /dev/urandom，避免引入额外依赖。
  head -c 18 /dev/urandom | base64 | tr -d '/+=' | head -c 20 > "$PASS_FILE"
  chmod 600 "$PASS_FILE"
  NEED_SET_PASS=1
fi

# 把密码写进 OpenList 的数据库（该命令会自行初始化 DB 与 config.json，幂等）。
if [ "$NEED_SET_PASS" = "1" ]; then
  echo "[entrypoint] 正在初始化内置网盘管理员账号…"
  (cd /app/openlist && "$OL_BIN" admin set "$(cat "$PASS_FILE")" --data "$OL_DATA" >/dev/null 2>&1) \
    || echo "[entrypoint] 警告：设置内置网盘密码失败，NewMovie 将无法自动接管（可在 /openlist/ 手动配置）"
fi

# ---- 端口：config.json 一旦生成就以文件为准，OPENLIST_HTTP_PORT 环境变量不再生效 ----
# 这是 OpenList 的既定行为，容易踩坑：设了环境变量却发现它还在 5244。
# 内置模式下我们要求它固定监听 5244（仅回环），所以直接把配置写死。
CONF="$OL_DATA/config.json"
if [ -f "$CONF" ]; then
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$CONF" <<'PY' || echo "[entrypoint] 警告：调整内置网盘端口失败"
import json, sys
p = sys.argv[1]
with open(p) as f:
    c = json.load(f)
s = c.setdefault("scheme", {})
s["address"] = "127.0.0.1"   # 只监听回环，不对外暴露 5244
s["http_port"] = 5244
s["https_port"] = -1
with open(p, "w") as f:
    json.dump(c, f, indent=2)
PY
  else
    # 没有 python3 时退化为 sed，只改端口不动监听地址。
    sed -i 's/"http_port"[[:space:]]*:[[:space:]]*[0-9-]*/"http_port": 5244/; s/"https_port"[[:space:]]*:[[:space:]]*[0-9-]*/"https_port": -1/' "$CONF" \
      || echo "[entrypoint] 警告：调整内置网盘端口失败"
  fi
fi

# 告诉 NewMovie：内置模式开启。
export NEWMOVIE_BUNDLED="${NEWMOVIE_BUNDLED:-1}"
export NEWMOVIE_BUNDLED_URL="${NEWMOVIE_BUNDLED_URL:-http://127.0.0.1:5244}"
export NEWMOVIE_BUNDLED_USER="${NEWMOVIE_BUNDLED_USER:-admin}"

echo "[entrypoint] 启动 NewMovie 2.0（内置 139cas 后端）"
echo "[entrypoint]   媒体库入口 → http://0.0.0.0${VIDRIVE_ADDR:-:8096}/"
echo "[entrypoint]   网盘挂载管理 → http://0.0.0.0${VIDRIVE_ADDR:-:8096}/openlist/"

exec /usr/bin/supervisord -c /app/docker/supervisord.conf
