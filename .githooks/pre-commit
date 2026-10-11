#!/usr/bin/env bash
# Git Pre-commit High-Performance Secret Scanner
set -u

PRAGMA='git:allow-secret'
HITS="$(mktemp)"
RAW_TMP="$(mktemp)"
trap 'rm -f "$HITS" "$RAW_TMP"' EXIT

PATTERNS='OpenAI/Anthropic 风格 key|sk-[A-Za-z0-9]{20,}
AWS Access Key ID|AKIA[A-Z0-9]{16}
GitHub PAT (classic)|ghp_[A-Za-z0-9]{36}
GitHub PAT (fine-grained)|github_pat_[A-Za-z0-9_]{20,}
Google API Key|AIza[A-Za-z0-9_-]{30,}
Slack token|xox[baprs]-[A-Za-z0-9-]{10,}
JWT|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}
私钥 PEM 块|-----BEGIN [A-Z ]*PRIVATE KEY-----
Java keystore base64 (JKS)|/u3\+7Q
Java keystore base64 (JCEKS)|zs7[O]z'

GREP_ARGS=()
while IFS='|' read -r label regex; do
    [ -n "${regex:-}" ] || continue
    GREP_ARGS+=(-e "$regex")
done <<< "$PATTERNS"

while IFS= read -r -d '' path; do
    case "$path" in
        *.example|*.sample|*.template) ;;
        .env|*/.env|*.secret|*credentials*|*.pem|*.key|*id_rsa*|*id_ed25519*)
            printf 'NAME\t%s\t敏感配置文件不应入库\n' "$path" >> "$HITS"
            ;;
        *.jks|*.jks.b64|*.keystore|*.p12|*.pfx|*signing.properties|*keystore.properties)
            printf 'NAME\t%s\t签名 keystore 不应入库\n' "$path" >> "$HITS"
            ;;
    esac

    if ! git show ":$path" > "$RAW_TMP" 2>/dev/null; then
        continue
    fi

    # 二进制 Magic 探测
    magic="$(head -c 7 "$RAW_TMP" | od -An -tx1 | tr -d ' \n')"
    case "$magic" in
        feedfeed*|cececece*|3082????020103)
            printf 'NAME\t%s\tkeystore（按文件头 Magic 认出）不应入库\n' "$path" >> "$HITS"
            ;;
    esac

    # 快路径单次扫描
    matched_lines="$(grep -nIE "${GREP_ARGS[@]}" "$RAW_TMP" 2>/dev/null \
        | grep -v -- "$PRAGMA" \
        | cut -d: -f1)"

    # 慢路径标签归因
    if [ -n "$matched_lines" ]; then
        while IFS='|' read -r label regex; do
            [ -n "${regex:-}" ] || continue
            printf '%s\n' "$matched_lines" | while IFS= read -r lineno; do
                if sed -n "${lineno}p" "$RAW_TMP" | grep -qE -e "$regex" 2>/dev/null; then
                    printf 'LINE\t%s:%s\t%s\n' "$path" "$lineno" "$label" >> "$HITS"
                fi
            done
        done <<< "$PATTERNS"
    fi
done < <(git diff --cached --name-only -z --diff-filter=ACMR)

if [ -s "$HITS" ]; then
    echo "❌ [SECURITY ALERT] 发现潜在敏感凭据或密钥，提交已被拦截："
    while IFS=$'\t' read -r kind loc label; do
        printf '   • [%s] %s (%s)\n' "$kind" "$loc" "$label"
    done < "$HITS"
    echo ""
    echo "请撤出该文件或将密钥迁移至环境变量；若为误报可在行内添加注释: # $PRAGMA"
    exit 1
fi
exit 0
