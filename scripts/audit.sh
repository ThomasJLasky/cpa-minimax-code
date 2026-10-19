#!/usr/bin/env bash
# 推送前自检：确认仓库里没有凭据、本机路径、个人标识。
#
#   ./scripts/audit.sh
#   AUDIT_EXTRA='terminus|wtt250249' ./scripts/audit.sh   # 追加本机专属词
#
# 退出码 0 = 干净，1 = 有命中。
set -uo pipefail
cd "$(dirname "$0")/.."

SELF=':!scripts/audit.sh'
FAIL=0
say() { printf '\n\033[1m%s\033[0m\n' "$1"; }

check() {
  local label="$1" pattern="$2" hits
  hits=$(git grep -nEI "$pattern" -- . "$SELF" 2>/dev/null | grep -vE '^README\.md:.*<mmoat_' || true)
  if [[ -n "$hits" ]]; then
    printf '  x %s\n' "$label"
    printf '%s\n' "$hits" | sed 's/^/      /' | head -8
    FAIL=1
  else
    printf '  ok %s\n' "$label"
  fi
}

say "1. 凭据与密钥"
check "无 MiniMax access/refresh token"  'mmoat_[A-Za-z0-9]{10,}|mmort_[A-Za-z0-9]{10,}'
check "无 OpenAI/Anthropic 风格密钥"      'sk-[A-Za-z0-9]{16,}'
check "无 GitHub token"                   'ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}'
check "无私钥块"                          'BEGIN (RSA |EC |OPENSSH |PGP )?PRIVATE KEY'
check "无云厂商密钥"                      'AKIA[0-9A-Z]{16}|LTAI[A-Za-z0-9]{12,}'

say "2. 本机与个人标识"
check "无本机绝对路径"                    '/Users/[A-Za-z][^/ ]*/|/home/[a-z]+/'
check "无真实邮箱"                        '[A-Za-z0-9._%+-]+@(qq|gmail|163|outlook|alibaba|bytedance|tencent)\.[a-z.]+'
check "无手机号"                          '(^|[^0-9])1[3-9][0-9]{9}([^0-9]|$)'
if [[ -n "${AUDIT_EXTRA:-}" ]]; then
  check "无本机专属词（AUDIT_EXTRA）" "$AUDIT_EXTRA"
else
  printf '  -- 跳过本机专属词（可用 AUDIT_EXTRA 指定）\n'
fi

say "3. 凭据文件"
LEAK=$(git ls-files | grep -E '(^|/)(auth|auths)/|\.env|config\.json$|\.pem$|\.key$|\.p12$' || true)
if [[ -n "$LEAK" ]]; then printf '  x 跟踪了凭据类文件:\n%s\n' "$LEAK"; FAIL=1; else printf '  ok 未跟踪任何凭据文件\n'; fi

say "4. 提交元信息"
BAD_ID=$(git log --all --format='%an <%ae>' | sort -u | grep -E '@(qq|gmail|163|outlook|alibaba|bytedance|tencent)\.' || true)
if [[ -n "$BAD_ID" ]]; then
  printf '  x 提交里含真实邮箱:\n%s\n' "$BAD_ID"; FAIL=1
else
  git log --all --format='  ok %an <%ae>' | sort -u
fi

say "5. 构建产物"
ART=$(git ls-files | grep -E '\.(dylib|so|dll|h)$|^dist/' || true)
if [[ -n "$ART" ]]; then printf '  x 跟踪了构建产物:\n%s\n' "$ART"; FAIL=1; else printf '  ok 无构建产物入库\n'; fi

say "6. 历史对象"
HIST=0
for obj in $(git rev-list --objects --all | awk '{print $1}'); do
  if git cat-file -p "$obj" 2>/dev/null | grep -qE 'mmoat_[A-Za-z0-9]{10,}|mmort_[A-Za-z0-9]{10,}'; then
    printf '  x 历史对象 %s 含令牌\n' "$obj"; FAIL=1; HIST=1
  fi
done
[[ $HIST -eq 0 ]] && printf '  ok 历史对象无令牌\n'

say "结论"
if [[ $FAIL -eq 0 ]]; then printf '  PASS 可以推送\n'; else printf '  FAIL 先修复再推\n'; fi
exit $FAIL
