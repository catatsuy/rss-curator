# rss-curator

技術系RSSの新着記事から、深く読んでおきたい記事を選び、技術的な内容を日本語で理解できる形にしてSlackへ届ける個人用ツールです。

追いかけたい情報源が増えると、記事の発見と選別だけでも時間がかかります。rss-curatorはその部分を自動化し、実装の仕組み、設計判断、測定結果、制約まで読める要約を手元に残すことを目指しています。

## 届けたいもの

重視するのは、記事から得られる技術的な学びです。既に関心のある分野との一致に加え、知らなかった技術や設計上の工夫を学べそうな記事も選びます。興味判定の基準はプロンプトで設定します。

要約には、API名や関数名、プロトコルの動作、アルゴリズム、処理フロー、設計の理由、ベンチマークの条件と数値、トレードオフを残します。宣伝や冗長な導入は圧縮し、技術的な説明に紙幅を使います。長い記事は本文を分けて処理し、各部分の詳細要約を1つのファイルにまとめます。

Slackには1記事につき1つのMarkdownファイルを投稿します。長文を複数のメッセージに分割せず、タイトルと原文URLを添えて、あとから記事単位で読み返せる形にします。原文にない情報は補わず、画像を解釈していない場合は図の内容を推測しません。

## 処理の流れ

1. RSS / Atomから記事を取得し、過去の状態と照合して新着を見つける。
2. 記事本文を取得し、見出し、コード、表、リンクなどを保ったMarkdownにする。
3. タイトルと本文を使って興味を判定する。
4. 採用した記事の日本語詳細要約を生成し、投稿用ファイルにする。
5. Slackへファイルを投稿し、成功した記事を処理済みとして記録する。

記事の発見元はRSS / Atomに限定します。Feisty Duckの紹介先をたどる場合だけ、ニュースレター内のリンクから記事を取得します。

### 情報源ごとの読み方

`feeds.json`で情報源と処理モードを管理します。初期設定にはCloudflare、Meta Engineering、Go Blog、Feisty Duck、GFW Reportなど16フィードを登録しています。

| モード | 読み方 |
| --- | --- |
| `select` | 通常の情報源向け。本文を読んで興味を判定し、採用記事を詳細要約する |
| `all` | すべて読みたい情報源向け。興味判定を省略して詳細要約する |
| `digest` | Feisty Duck向け。本体を必ず要約し、紹介先も個別に選別・要約する |

`digest`では、リンク周辺の紹介文も興味判定に使います。本体と紹介先は別々のファイルとして届けます。広告、購読、ナビゲーション、共有リンクを除外し、紹介先の記事にあるリンクは追跡しません。投稿済み本体もRSSに残っている間は紹介先を再確認し、失敗した紹介先を再試行します。

PDFはタイトルとURLだけを通知します。本文のダウンロード、解析、AIへの送信は行いません。論文メタデータがあるHTMLのランディングページも同じ扱いです。メタデータのない論文ページを完全に識別することはできません。

### 初回と重複の扱い

初回は各フィードの最新2記事だけを対象とし、それより古い記事は対象外として記録します。最新順は公開日時、なければ更新日時を使い、日時がない記事同士はRSSの掲載順を使います。初回の対象記事で処理に失敗したものは、次回の再試行対象に残します。

2回目以降は件数を制限せず、RSS内の未処理記事を取得します。日時だけで既読を判断せず、記事URLとRSSのGUIDも照合するため、古い公開日で追加された記事も対象になります。

複数のフィードやFeisty Duckで同じ記事を見つけても、重複投稿を避けます。記事の識別には、妥当性を確認したcanonical URLを優先し、利用できなければリダイレクト後のURLを使います。フラグメントと明らかな追跡パラメータを除き、記事の識別に必要なクエリは保持します。canonicalが別ホストやトップページを指す場合、複数の宣言が矛盾する場合、`og:url`と一致しない場合は採用しません。同一ホスト上の誤ったcanonicalを完全には判別できません。

## 構成と状態管理

日常の実行はGitHub Actionsで行います。Goが記事の取得から投稿用ファイルの生成までを担当し、`scripts/run.sh`がSlack投稿と状態の永続化を担当します。Slack投稿には[notify_slack](https://github.com/catatsuy/notify_slack)を使います。Actionsでは最新ReleaseのLinux向けバイナリを取得して配置します。

ソースコードと設定は`main`、処理状態は`rss-state`ブランチに置きます。個人利用の規模に合わせ、状態はJSONファイルで管理します。

### 投稿成功後に処理を確定する

要約ができた時点では、通知対象の記事を処理済みにしません。Slackへの投稿に成功してから`ack`で確定します。興味なしと判定した記事は、通知せず処理済みにします。

Goはフィード初期化時と記事の処理後に状態を保存します。ShellはGoの終了後と各投稿の確定後に状態をcommit / pushし、途中終了時にも保存を試みます。状態保存やpushに失敗した場合はジョブを失敗させます。

SlackとGitの更新は同時には確定できないため、投稿成功から状態保存までの間に中断すると再投稿される場合があります。また、RSSから既に消えた記事は発見・再試行できません。実行間隔の間にRSSの保持件数を超える記事が追加された場合も同様です。

### 公開する情報を絞る

`rss-state`に保存するのは、記事や識別子のSHA-256、処理日時、フィード初期化状態、再確認に必要なハッシュ同士の対応です。興味判定のスコア・採否・理由、記事URL、要約、個人用プロンプトは保存しません。

投稿用ファイルは一時ディレクトリに生成し、実行終了時に削除します。GitへのcommitやActionsの成果物としての公開は行いません。APIキー、Slack Token、個人用プロンプト、要約全文も公開ログに出しません。

## GitHub Actionsで使う

OpenAIへの認証にはAPIキーを使います。GitHubの **Settings → Secrets and variables → Actions → Secrets** に、`OPENAI_API_KEY`という名前でAPIキーを登録してください。ワークフローはこのSecretを読み込みます。

Workload Identity用のVariablesや`id-token: write`は不要です。以前登録したVariablesが残っていても、APIキーを優先します。

### プロンプトとSlack

プロンプトはリポジトリの次のファイルに含めています。Actionsはcheckoutしたファイルをそのまま読み込むため、プロンプト用のSecret登録やファイル転送は不要です。内容を変更する場合はファイルを編集してcommitしてください。

- `prompts/interest.md`: 記事の興味判定基準
- `prompts/summary.md`: 日本語詳細要約の指示

**Settings → Secrets and variables → Actions → Secrets** に次の2項目を登録します。

| Secret | 内容 |
| --- | --- |
| `NOTIFY_SLACK_TOKEN` | Slack Bot Token |
| `NOTIFY_SLACK_CHANNEL_ID` | 投稿先の非公開チャンネルID |

プロンプトを非公開にしたい場合は、任意で`INTEREST_PROMPT`と`SUMMARY_PROMPT`をSecretsに登録すると、リポジトリのファイルより優先されます。

Slack AppのBot Tokenに`files:write`を付与し、アプリをワークスペースへインストールして非公開チャンネルへ招待してください。投稿先にはチャンネル名ではなく、チャンネル詳細から取得したIDを設定します。

Actionsを有効にし、`GITHUB_TOKEN`による`rss-state`へのpushを許可してください。ワークフローには`contents: write`を設定しています。PATは不要です。ブランチ保護規則でpushが拒否される場合は規則を調整してください。

標準Ubuntu runnerで1日4回、日本時間09:17 / 15:17 / 21:17 / 翌03:17に実行します。同じワークフローは直列で実行し、ジョブのタイムアウトは90分です。PRからSecretsにアクセスするワークフローはありません。

手動実行はActionsタブの「RSSの選別とSlack投稿」から **Run workflow** を選びます。`dry_run`を有効にすると対象候補の列挙だけを行います。定期実行はGitHub側の都合で遅延・停止する場合があるため、必要に応じて実行履歴を確認してください。

## 設定を変える

### RSSの追加

`feeds.json`に次の形式で追加します。

```json
{
  "name": "情報源の名前",
  "url": "https://example.com/feed.xml",
  "mode": "select"
}
```

新しく追加したフィードも、初回は最新2記事から始めます。`digest`はFeisty Duck用です。GFW Reportは`https://gfw.report/index.xml`を`all`モードで登録し、興味判定を省略して処理します。`path_suffix: "/en/"`で英語版の記事だけを対象にし、中国語版との重複を避けます。

### 興味判定と要約

`prompts/interest.md`に選びたい記事の基準、`prompts/summary.md`に要約の指示を記述します。初期プロンプトには個人名や所属を含めていません。これらのファイルはPublicリポジトリで公開される設定です。

`INTEREST_PROMPT_FILE`と`SUMMARY_PROMPT_FILE`で読み込むファイルを変更できます。環境変数`INTEREST_PROMPT`と`SUMMARY_PROMPT`に本文を直接指定した場合は、そちらを優先します。

興味判定にはDecisions API、詳細要約にはResponses APIを使います。モデルと採用基準は次の環境変数で変更できます。

| 環境変数 | 初期値 | 用途 |
| --- | --- | --- |
| `INTEREST_MODEL` | `gpt-6-luna` | 興味判定のモデル |
| `SUMMARY_MODEL` | `gpt-6.1-sol` | 詳細要約のモデル |
| `INTEREST_THRESHOLD` | `0.8` | 採用するスコアの下限 |
| `INTEREST_LEVELS` | 学びが少ない／微妙だが学びが見込める／技術的な深さや新発見が多い | 順序付き評価ラベル。カンマ区切りで2〜10段階 |

スコアは0始まりのレベル番号の確率加重平均です。初期設定は0〜2で、0.8以上を採用します。閾値はレベルの範囲内に設定してください。拒否や不正な応答は不採用として確定せず、再試行対象にします。

Actionsでは同名のRepository variablesでモデルと閾値を変更できます。評価ラベルを変える場合は、ワークフローの`env`にも`INTEREST_LEVELS`を追加してください。API使用量は[OpenAI Usage](https://platform.openai.com/usage)でプロジェクト・モデル別に確認できます。

## ローカルで使う

Go 1.27.2以上を用意し、[notify_slackの最新Release](https://github.com/catatsuy/notify_slack/releases/latest)からOS・CPUに対応するバイナリをダウンロードしてPATHへ配置します。

ローカル実行でも`OPENAI_API_KEY`を設定します。dry-runでは認証は不要です。

```bash
go build -o rss-curator .

export OPENAI_API_KEY='...'
export INTEREST_PROMPT_FILE="$PWD/prompts/interest.md"
export SUMMARY_PROMPT_FILE="$PWD/prompts/summary.md"
export NOTIFY_SLACK_TOKEN='...'
export NOTIFY_SLACK_CHANNEL_ID='...'

scripts/run.sh
```

通常実行には`origin`の読み書き権限が必要です。`rss-state`がなければ自動作成します。ローカルでも同じ状態ブランチへpushするため、Actionsとの同時実行は避けてください。

### dry-run

```bash
./rss-curator run -config feeds.json -state state.json \
  -output /tmp/rss-curator-preview -dry-run
# または
scripts/run.sh --dry-run
```

APIキーとプロンプトは不要です。RSSだけを取得して対象候補のタイトル・URLを列挙し、記事ページの取得、AI呼び出し、Slack投稿、処理済み状態の更新は行いません。canonicalによる重複やFeisty Duckの紹介先、興味判定後の投稿対象までは確認できません。

出力先は存在しないディレクトリを指定してください。Actionsのdry-runは既存の状態ブランチを読み込みます。ローカルのShell dry-runは`RSS_STATE_FILE`（初期値`state.json`）を使います。

### 生成と投稿を個別に実行する

```bash
./rss-curator run -config feeds.json -state state.json -output output/

notify_slack -c /dev/null -snippet -snippet-type markdown \
  -filename ARTICLE_ID.md < output/pending/ARTICLE_ID.md
# 投稿の終了コードが0だった場合だけ実行する
./rss-curator ack -state state.json -id ARTICLE_ID
```

`run`は`pending/ID.md`と`manifest.json`、Shell用の`upload-ids`を出力します。投稿失敗時はackしないでください。通常はこの手順を自動化した`scripts/run.sh`を使います。

## エラーが起きたとき

RSS取得、記事取得・本文抽出、API、Slackの失敗は他の記事を止めません。失敗した記事は未処理のままにし、次の定期実行で再試行します。OpenAI SDK内での再試行は無効にし、APIの失敗も次の定期実行で再試行します。Slack投稿の失敗時は、他の投稿も続けたうえでジョブを失敗させます。

設定不備、壊れた状態ファイル、状態保存・pushの失敗は全体のエラーとして扱います。状態を削除すると初回動作に戻るため、復旧時はまずActionsのログと`rss-state`を確認してください。

## 開発時の確認

```bash
go test ./...
go vet ./...
staticcheck -checks="all,-ST1000" ./...
```

通常のテストは外部APIを呼びません。RSSの新着検出、本文抽出、重複排除、モードごとの分岐に加え、模擬Slack CLIと一時Gitリポジトリで、投稿成功後だけ状態を確定・保存することを確認します。
