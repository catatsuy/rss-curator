package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinkDeliveryStream(t *testing.T) {
	for _, reply := range []string{"posted\ncontinue\n", "failed\n"} {
		var out strings.Builder
		post, checkpoint := deliveryStream(strings.NewReader(reply), &out)
		payload := "https://example.com/article/" + "\n"
		err := post("article-id", "link\n"+payload)
		if (err != nil) != (reply == "failed\n") {
			t.Fatal("リンク投稿の応答が不正", err)
		}
		if err == nil {
			if err := checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		want := "link article-id " + base64.StdEncoding.EncodeToString([]byte(payload)) + "\n"
		if !strings.HasPrefix(out.String(), want) {
			t.Fatalf("unexpected event: %q", out.String())
		}
	}
}

func TestShellLinkPayload(t *testing.T) {
	script, err := os.ReadFile("scripts/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "post_article() {")
	end := strings.Index(string(script)[start:], "\nwhile IFS=") + start
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dir := t.TempDir()
			mock := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$TEST_ARGS\"\ncat > \"$TEST_BODY\"\n"
			if fail {
				mock += "exit 1\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "notify_slack"), []byte(mock), 0700); err != nil {
				t.Fatal(err)
			}
			payload := "https://example.com/article/" + "\n"
			cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+string(script)[start:end]+"\npost_article")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "event=link test", "id=test", "link_payload="+base64.StdEncoding.EncodeToString([]byte(payload)), "TEST_ARGS="+filepath.Join(dir, "args"), "TEST_BODY="+filepath.Join(dir, "body"))
			if b, err := cmd.CombinedOutput(); (err != nil) != fail {
				t.Fatalf("投稿結果が不正: %s %v", b, err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "body"))
			if err != nil || string(body) != payload {
				t.Fatal("リンク本文が不正", string(body), err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil || !strings.Contains(string(args), "-snippet-type\ntext\n-filename\ntest.txt") {
				t.Fatal("プレーンテキスト投稿ではありません", string(args), err)
			}
		})
	}
}
