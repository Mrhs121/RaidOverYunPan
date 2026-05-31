#!/usr/bin/env bash
# 一键创建 4 个 local 模拟盘，挂到 alist 上，方便端到端联调 cloudraid。
#
# 前置条件：alist 已经在 ALIST_URL 监听（默认 http://127.0.0.1:5244），
# 且 admin 用户密码与 ALIST_PASSWORD 相符。
#
# 用法：
#   ALIST_PASSWORD='你的密码' bash setup-local-test.sh
#
# 之后你 cloudraid 的 config.yaml 里 mounts 应填：
#   - "/local1"
#   - "/local2"
#   - "/local3"
#   - "/local4"

set -euo pipefail

ALIST_URL="${ALIST_URL:-http://127.0.0.1:5244}"
ALIST_USER="${ALIST_USER:-admin}"
ALIST_PASSWORD="${ALIST_PASSWORD:?需要设置 ALIST_PASSWORD 环境变量}"

DISK_ROOT="/Users/huangsheng/codes/test/pure_test/local_disk"
DISKS=(disk1 disk2 disk3 disk4)
MOUNTS=("/local1" "/local2" "/local3" "/local4")

echo "==> 准备目录 $DISK_ROOT/{disk1..4}"
for d in "${DISKS[@]}"; do
  mkdir -p "$DISK_ROOT/$d"
done

echo "==> 登录 alist"
TOKEN=$(curl -fsS "$ALIST_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ALIST_USER\",\"password\":\"$ALIST_PASSWORD\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')

if [ -z "$TOKEN" ]; then
  echo "登录失败，检查用户名密码与 alist 是否在运行" >&2
  exit 1
fi
echo "  token=${TOKEN:0:12}..."

create_storage() {
  local mount="$1"
  local disk="$2"
  local addition
  addition=$(cat <<EOF
{"root_folder_path":"$DISK_ROOT/$disk","thumbnail":false,"use_ffmpeg":false,"show_hidden":true,"mkdir_perm":"777","recycle_bin_path":"delete permanently","thumb_concurrency":"16","thumb_pixel":"320","video_thumb_pos":"20%"}
EOF
)
  # alist 的 addition 字段是「字符串里塞 JSON」，所以要再 JSON-quote 一次
  local addition_quoted
  addition_quoted=$(printf '%s' "$addition" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')

  local body
  body=$(cat <<EOF
{
  "mount_path": "$mount",
  "driver": "Local",
  "order": 0,
  "remark": "cloudraid local test",
  "cache_expiration": 0,
  "status": "work",
  "addition": $addition_quoted,
  "enable_sign": false,
  "web_proxy": false,
  "webdav_policy": "native_proxy",
  "down_proxy_url": ""
}
EOF
)

  echo "==> 创建存储 $mount → $DISK_ROOT/$disk"
  resp=$(curl -fsS "$ALIST_URL/api/admin/storage/create" \
    -H "Authorization: $TOKEN" \
    -H 'Content-Type: application/json' \
    -d "$body")
  echo "    $resp"
}

for i in "${!MOUNTS[@]}"; do
  create_storage "${MOUNTS[$i]}" "${DISKS[$i]}"
done

echo
echo "完成。验证一下："
echo "  curl -H 'Authorization: $TOKEN' '$ALIST_URL/api/admin/storage/list'"
