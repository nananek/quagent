#!/usr/bin/env bash
# レシピに同梱した署名鍵 (internal/image/recipes/*/*.asc) のうち、DAYS 日以内に
# 署名に使える鍵が無くなるものを 1 行ずつ出す。
set -euo pipefail
days=${DAYS:-60}
limit=$(( $(date +%s) + days * 86400 ))
for key in internal/image/recipes/*/*.asc; do
	ok=0
	latest=""
	while IFS=: read -r type validity _ _ _ _ expires _ _ _ _ caps _; do
		case $type in pub|sub) ;; *) continue ;; esac
		[[ $caps == *s* ]] || continue # 自分で署名できる鍵だけ
		[[ $validity == [re] ]] && continue # 失効・期限切れ
		if [[ -z $expires ]] || (( expires > limit )); then
			ok=1
		fi
		[[ -n $expires ]] && latest=$(date -u -d "@$expires" +%Y-%m-%d)
	done < <(gpg --batch --with-colons --show-keys "$key" 2>/dev/null)
	if (( ! ok )); then
		echo "$key ${latest:-(期限不明)}"
	fi
done
