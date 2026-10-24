#!/usr/bin/env bash
set -euo pipefail
umask 077
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
bin=${RSS_CURATOR_BIN:-"$root/rss-curator"}
if [[ ! -x "$bin" ]]; then go build -o "$bin" .; fi
work=$(mktemp -d)
state_dir="$work/state"
output="$work/output"
attached=0
initial_branch=""
worker=""
checkpoint() {
  [[ -f "$state_dir/state.json" ]] || return 0
  git -C "$state_dir" add -- state.json || return 1
  if ! git -C "$state_dir" diff --cached --quiet; then
    git -C "$state_dir" -c user.name=github-actions\[bot\] -c user.email=41898282+github-actions\[bot\]@users.noreply.github.com commit -q -m '処理状態を更新' || return 1
  fi
  git -C "$state_dir" push origin HEAD:refs/heads/rss-state || return 1
}
cleanup() {
  status=$?
  trap - EXIT
  if [[ -n "$worker" ]]; then
    exec 4>&-
    kill "$worker" 2>/dev/null || true
    wait "$worker" 2>/dev/null || true
  fi
  if [[ "$attached" == 1 ]]; then
    if ! checkpoint; then echo '状態の保存に失敗しました' >&2; status=1; fi
    git worktree remove --force "$state_dir" >/dev/null 2>&1 || true
  fi
  if [[ -n "$initial_branch" ]]; then git branch -D "$initial_branch" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ "${1:-}" == '--dry-run' ]]; then
  # リモートへの書き込みとSlack投稿をしない。ローカル状態は任意。
  dry_state=${RSS_STATE_FILE:-state.json}
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    remote_status=0
    git ls-remote --exit-code origin refs/heads/rss-state >"$work/remote" || remote_status=$?
    if [[ "$remote_status" == 0 ]]; then
      git fetch --no-tags origin refs/heads/rss-state
      git show FETCH_HEAD:state.json >"$work/state.json"
      dry_state="$work/state.json"
    elif [[ "$remote_status" != 2 ]]; then
      echo 'rss-stateブランチの確認に失敗しました' >&2; exit 1
    fi
  fi
  "$bin" run -config feeds.json -state "$dry_state" -output "$output" -dry-run
  exit 0
fi
: "${NOTIFY_SLACK_TOKEN:?NOTIFY_SLACK_TOKENを設定してください}"
: "${NOTIFY_SLACK_CHANNEL_ID:?NOTIFY_SLACK_CHANNEL_IDを設定してください}"
command -v notify_slack >/dev/null || { echo 'notify_slackがありません' >&2; exit 1; }
remote_status=0
git ls-remote --exit-code origin refs/heads/rss-state >"$work/remote" || remote_status=$?
if [[ "$remote_status" == 0 ]]; then
  git fetch --no-tags origin refs/heads/rss-state
  git worktree add --detach "$state_dir" FETCH_HEAD
elif [[ "$remote_status" == 2 ]]; then
  git worktree add --detach "$state_dir" HEAD
  attached=1
  initial_branch="rss-curator-initial-$$"
  git -C "$state_dir" switch --orphan "$initial_branch"
else
  echo 'rss-stateブランチの確認に失敗しました' >&2; exit 1
fi
attached=1
# 1記事ごとにGoを待機させ、シェルで投稿と永続化を完了する。
mkfifo "$work/events" "$work/replies"
"$bin" run -config feeds.json -state "$state_dir/state.json" -output "$output" -stream \
  <"$work/replies" >"$work/events" &
worker=$!
exec 4>"$work/replies"
exec 3<"$work/events"
post_article() {
  if [[ "$event" == link\ * ]]; then
    printf '%s' "$link_payload" | base64 --decode | notify_slack -c /dev/null -snippet -snippet-type text -filename "$id.txt"
  else
    notify_slack -c /dev/null -snippet -snippet-type markdown -filename "$id.md" <"$output/pending/$id.md"
  fi
}
while IFS= read -r event <&3; do
  case "$event" in
    'ready '*|'link '*)
      link_payload=""
      if [[ "$event" == link\ * ]]; then
        link_data=${event#link }
        id=${link_data%% *}
        link_payload=${link_data#* }
        [[ "$link_payload" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || { echo '投稿データが不正です' >&2; exit 1; }
      else
        id=${event#ready }
      fi
      [[ "$id" =~ ^[0-9a-f]{64}$ ]] || { echo '投稿IDが不正です' >&2; exit 1; }
      if post_article >"$work/slack.log" 2>&1; then
        echo posted >&4
      else
        slack_status=$?
        echo "Slackへの投稿に失敗しました: $id (notify_slack exit=$slack_status)" >&2
        # 出力全文にはトークンや要約が含まれ得るため、既知のエラーコードだけを出す。
        while IFS= read -r line || [[ -n "$line" ]]; do
          if [[ "$line" =~ (^|[^a-z_])(missing_scope|invalid_auth|not_authed|token_revoked|token_expired|account_inactive|not_in_channel|channel_not_found|no_permission|is_archived|invalid_arguments|invalid_snippet_type|file_uploads_disabled|file_upload_size_restricted|ratelimited|rate_limited|internal_error|fatal_error|request_timeout|service_unavailable|method_deprecated)([^a-z_]|$) ]]; then
            echo "Slack error: ${BASH_REMATCH[2]}" >&2
          fi
        done <"$work/slack.log"
        echo failed >&4
        wait "$worker" || true
        worker=""
        exit 1
      fi
      ;;
    saved)
      if ! checkpoint; then
        echo '投稿成功後の状態の永続化に失敗しました' >&2
        echo failed >&4
        wait "$worker" || true
        worker=""
        exit 1
      fi
      sleep 1
      echo continue >&4
      ;;
    *) echo 'Goからの通知が不正です' >&2; exit 1 ;;
  esac
done
status=0
wait "$worker" || status=$?
worker=""
exit "$status"
