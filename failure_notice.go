package main

import (
	"fmt"
	"path/filepath"
)

// 通知の再試行では記事取得・OpenAIを呼ばず、シェルの既存投稿経路だけを使う。
func (r *runner) notifyFetchFailure(raw string) error {
	id := hash(raw)
	f := r.state.Fetch[id]
	if !f.FailureNoticePending || r.dry || r.post == nil {
		return nil
	}
	noticeID := hash("fetch-failure\n" + id)
	path := filepath.Join(r.output, "pending", noticeID+".md")
	body := fmt.Sprintf("# 記事の取得を断念しました\n\nURL: %s\n\n取得方法: %s\n失敗理由: %s\n停止理由: %s\n試行回数: %d（連続失敗: %d）\n\n自動再取得は停止しています。\n", safeArticleURL(raw), f.Method, f.LastFailure, f.StopReason, f.Attempts, f.Failures)
	if err := atomicWrite(path, []byte(body)); err != nil {
		return fmt.Errorf("%w: 失敗通知ファイルを作成できません", errDelivery)
	}
	if err := r.post(noticeID, path); err != nil {
		return fmt.Errorf("%w: %v", errDelivery, err)
	}
	f.FailureNoticePending = false
	r.state.Fetch[id] = f
	if err := r.save(); err != nil {
		return fmt.Errorf("%w: %v", errDelivery, err)
	}
	if r.checkpoint != nil {
		if err := r.checkpoint(); err != nil {
			return fmt.Errorf("%w: 状態の永続化に失敗しました", errDelivery)
		}
	}
	return nil
}
