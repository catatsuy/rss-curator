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
"$bin" run -config feeds.json -state "$state_dir/state.json" -output "$output"
checkpoint
failed=0
while IFS= read -r id; do
  [[ "$id" =~ ^[0-9a-f]{64}$ ]] || { echo '投稿IDが不正です' >&2; exit 1; }
  if notify_slack -c /dev/null -snippet -snippet-type markdown -filename "$id.md" <"$output/pending/$id.md" >"$work/slack.log" 2>&1; then
    "$bin" ack -state "$state_dir/state.json" -id "$id"
    checkpoint
  else
    echo "Slackへの投稿に失敗しました: $id" >&2
    failed=1
  fi
done <"$output/upload-ids"
exit "$failed"
