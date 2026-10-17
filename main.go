package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("run または ack を指定してください")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	state := f.String("state", "state.json", "状態ファイル")
	switch args[0] {
	case "run":
		config := f.String("config", "feeds.json", "フィード設定")
		output := f.String("output", "output", "新規の出力ディレクトリ")
		dry := f.Bool("dry-run", false, "APIと状態更新を行わずRSSの処理予定を列挙")
		stream := f.Bool("stream", false, "記事IDを出力し、標準入力で投稿・永続化の完了を待つ")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return fmt.Errorf("余分な引数があります")
		}
		if *stream && !*dry {
			post, checkpoint := deliveryStream(os.Stdin, os.Stdout)
			return runWithDelivery(ctx, *config, *state, *output, *dry, post, checkpoint)
		}
		return run(ctx, *config, *state, *output, *dry)
	case "ack":
		id := f.String("id", "", "投稿成功した記事ID")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		return ack(*state, *id)
	default:
		return fmt.Errorf("不明なコマンドです: %s", args[0])
	}
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := execute(ctx, os.Args[1:]); e != nil {
		slog.Error("実行に失敗しました", "エラー", e)
		os.Exit(1)
	}
}

// シェルからの応答があるまで次の記事に進まない。要約本文は出力しない。
func deliveryStream(input io.Reader, output io.Writer) (func(string, string) error, func() error) {
	replies := bufio.NewScanner(input)
	exchange := func(event, expected string) error {
		if _, err := fmt.Fprintln(output, event); err != nil {
			return fmt.Errorf("シェルへ通知できません")
		}
		if !replies.Scan() || replies.Text() != expected {
			return fmt.Errorf("シェルでの投稿または状態保存が完了しませんでした")
		}
		return nil
	}
	return func(id, _ string) error { return exchange("ready "+id, "posted") },
		func() error { return exchange("saved", "continue") }
}
