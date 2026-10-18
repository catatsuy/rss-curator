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

`digest`では、リンク周辺の紹介文も興味判定に使います。本体と紹介先は別々のファイルとして届けます。広告、購読、ナビゲーション、共有リンクを除外し、紹介先の記事にあるリンクは追跡しません。紹介先のハッシュ一覧と完了状態を保存し、本体と全紹介先が処理済み（不採用・除外・停止を含む）なら本体を再取得しません。未完了の紹介先がすべて待機中、Crawlbase未設定、利用上限到達などの場合も本体を取得しません。紹介先の再試行が必要な場合だけ、URLを再発見するために本体を取得します。

PDFはタイトルとURLだけを通知します。本文のダウンロード、解析、AIへの送信は行いません。論文メタデータがあるHTMLのランディングページも同じ扱いです。メタデータのない論文ページを完全に識別することはできません。

### 初回と重複の扱い

初回は各フィードの最新2記事だけを対象とし、それより古い記事は対象外として記録します。最新順は公開日時、なければ更新日時を使い、日時がない記事同士はRSSの掲載順を使います。初回の対象記事で処理に失敗したものは、次回の再試行対象に残します。

2回目以降は件数を制限せず、RSS内の未処理記事を取得します。日時だけで既読を判断せず、記事URLとRSSのGUIDも照合するため、古い公開日で追加された記事も対象になります。

複数のフィードやFeisty Duckで同じ記事を見つけても、重複投稿を避けます。記事の識別には、妥当性を確認したcanonical URLを優先し、利用できなければリダイレクト後のURLを使います。フラグメントと明らかな追跡パラメータを除き、記事の識別に必要なクエリは保持します。canonicalが別ホストやトップページを指す場合、複数の宣言が矛盾する場合、`og:url`と一致しない場合は採用しません。同一ホスト上の誤ったcanonicalを完全には判別できません。

## 構成と状態管理

日常の実行はGitHub Actionsで行います。Goが記事の取得と要約を担当し、`scripts/run.sh`が記事ごとのSlack投稿と状態の永続化を担当します。Slack投稿には[notify_slack](https://github.com/catatsuy/notify_slack)を使います。Actionsでは最新ReleaseのLinux向けバイナリを取得して配置します。

ソースコードと設定は`main`、処理状態は`rss-state`ブランチに置きます。個人利用の規模に合わせ、状態はJSONファイルで管理します。

### 投稿成功後に処理を確定する

要約ができた時点では、通知対象の記事を処理済みにしません。通常実行ではSlackへの投稿に成功した直後に確定し、状態を永続化してから次の記事へ進みます。手動投稿では`ack`で確定します。興味なしと判定した記事は、通知せず処理済みにします。

Goはフィード初期化時と記事の処理後に状態を保存します。ShellはGoの終了後と各投稿の確定後に状態をcommit / pushし、途中終了時にも保存を試みます。状態保存やpushに失敗した場合はジョブを失敗させます。

SlackとGitの更新は同時には確定できないため、投稿成功から状態保存までの間に中断すると再投稿される場合があります。また、RSSから既に消えた記事は発見・再試行できません。実行間隔の間にRSSの保持件数を超える記事が追加された場合も同様です。

### 公開する情報を絞る

`rss-state`に保存するのは、記事や識別子のSHA-256、処理日時、フィード初期化状態、再確認に必要なハッシュ同士の対応、記事取得の方法・失敗分類・試行回数・次回試行日時・最終試行日時・最終発見日時、ホストの待機期限、ニュースレターの紹介先ハッシュ一覧と完了状態です。興味判定のスコア・採否・理由、記事URL、要約、個人用プロンプトは保存しません。

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

標準Ubuntu runnerで1日4回、日本時間07:17 / 10:17 / 13:17 / 19:17に実行します。朝の2回で米国東海岸・西海岸の前日の更新を拾い、13時と19時に遅い更新や他地域の記事を確認する想定です。日本時間を固定し、米国の夏時間・冬時間の両方を考慮しています。RSSは毎回確認しますが、処理対象がなければ記事本文の取得やOpenAIへのリクエストは行いません。同じワークフローは直列で実行し、ジョブのタイムアウトは90分です。PRからSecretsにアクセスするワークフローはありません。

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

### 記事取得のCrawlbaseフォールバック

通常HTTPで記事を取得し、403・429の場合だけ、**次回の実行**から[Crawlbase Crawling API](https://crawlbase.com/docs/crawling-api)に切り替えます。同じ実行内では切り替え・再試行を行いません。RSS取得には使用しません。HTMLは従来と同じ本文抽出、canonical判定、興味判定、要約、逐次Slack投稿へ渡します。SDKは追加していません。

| 環境変数 | 初期値 | 用途 |
| --- | --- | --- |
| `CRAWLBASE_TOKEN` | 未設定 | **Normal Token**。Actionsでは同名のRepository Secretに登録 |
| `CRAWLBASE_MAX_REQUESTS` | `5` | 1実行のCrawlbaseリクエスト上限（失敗も含む）。`0`で停止。Actionsでは同名のRepository variableで変更 |
| `ARTICLE_MAX_FETCH_FAILURES` | `3` | 1記事の連続取得失敗の上限。通常HTTP・Crawlbaseを合算し、実行間で維持。1以上の整数。Actionsでは同名のRepository variableで変更 |
| `ARTICLE_USER_AGENT` | `rss-curator (+https://github.com/catatsuy/rss-curator)` | 通常HTTPの記事取得（HEAD/GET）で送るUA。未設定・空文字・空白の場合も既定値を送る。Actionsでは同名のRepository variableで変更 |

サイトによってはcrawler風のUAを拒否するため、記事取得のUAを指定できます。ブラウザ固有のヘッダー追加やサイトごとの例外は設けていません。RSS・OpenAI・Crawlbase APIのUAは変更しません。UAを変更しても、既に停止・処理済みになった記事を自動的に再試行することはありません。

YouTube（短縮URL・サブドメインを含む）、求人サイト/求人・採用ページ、寄付・購読ページは取得前に除外します。これらには通常HTTP・Crawlbase・OpenAIのリクエストを送りません。Crawlbaseへの切り替え予定だったURLにも適用します。

JavaScriptレンダリングやJavaScript Tokenへの自動切り替えは行いません。トークン未設定でも通常HTTPの記事は処理できます。Crawlbaseへ切り替え済みの記事は、通常HTTPを再試行せず、トークンの設定まで保留します。上限到達も失敗回数に加えず、残りを次回へ繰り越します。

| 通常HTTPの結果 | 次の処理 |
| --- | --- |
| 2xx | 従来どおり処理 |
| 403 | 次回からCrawlbase |
| 429 | 最低1時間と`Retry-After`（秒数・HTTP日時）の遅い方まで待ち、次回以降にCrawlbase。同じホストの別記事も期限までは取得しない |
| 401・404・410 | 恒久失敗として自動再試行を停止 |
| 5xx・タイムアウト・接続エラー | 次の実行も通常HTTP |
| 本文抽出失敗 | 次の実行も通常HTTP（Crawlbaseに切り替えない） |

HTTP 200でも本文を抽出できない場合や、5xx・接続障害も取得失敗に数えます。通常HTTPとCrawlbaseを合わせて既定3回の連続失敗で停止し、以後は自動再試行しません。取得処理1回（通常HTTPのHEAD/GETを含む）を1試行として数え、本文取得の成功で連続失敗回数をリセットします。待機・Crawlbaseの利用上限・トークン未設定による保留は失敗回数に加えません。旧状態ファイルは、保存済みの失敗中の試行回数を新しいカウンターへ引き継ぎます。

`HEAD`が429を返した場合は、その後の`GET`も行いません。待機は長い`sleep`ではなく、状態ファイルの日時で制御します。

CrawlbaseはAPIのHTTPステータス・`cb_status`・`original_status`のすべてを確認します。同期取得でHTTP 200・`cb_status` 200・対象2xxの場合だけ本文を処理します。対象の403・429・401・404・410、ブロック/CAPTCHAや空本文などの恒久失敗は停止します。Crawlbase側の5xx、接続障害、対象5xx、不正な応答などは最低1時間あけて、**連続3回失敗まで**実行間で再試行します。ただし、記事全体の`ARTICLE_MAX_FETCH_FAILURES`に先に達した場合はそこで停止します。API自体の429は他のCrawlbase取得にも待機期限を適用し、APIの認証・残高エラーではその実行の後続Crawlbase呼び出しも抑止します。

取得状態は記事URLのハッシュをキーに、既存の`rss-state`ブランチの`state.json`へ保存します。待機中の記事は処理済みにしません。停止した記事のハッシュは処理済みとして残し、URLや認証情報は保存しません。新しいフィールドは省略可能で、既存の状態ファイルをそのまま読めます。紹介先一覧のない旧形式の投稿済みニュースレターは、一覧の移行のため最初の1回だけ本体を取得します。全紹介先の完了後は、実行間や取得状態の30日整理後も本体を再取得しません。成功して処理が完了した取得状態、期限が切れたホスト待機、30日間再発見されなかった取得状態は削除します。RSSや紹介元から消えたURLは従来どおり再発見できないため、別の永続キューは作りません。

スケジュール・手動・再実行で同じ状態ブランチを読み、既存の同時実行制御とcommit / pushを使います。Crawlbase取得後にOpenAIやSlackで失敗した場合、本文は永続化しないため、次回の本文取得が再度必要になります。

構造化ログには、URL（認証情報を含み得るクエリ等は省略）、取得方法、各ステータス、結果/失敗分類・停止理由、試行回数・連続失敗回数・失敗上限、次回取得方法・待機期限、実行内のCrawlbase回数を出します。Crawlbaseのトークンを含むAPI URL、レスポンス本文、HTTPクライアントの生のエラーは出しません。

## ローカルで使う

Go 1.27.2以上を用意し、[notify_slackの最新Release](https://github.com/catatsuy/notify_slack/releases/latest)からOS・CPUに対応するバイナリをダウンロードしてPATHへ配置します。

ローカル実行でも`OPENAI_API_KEY`を設定します。dry-runでは認証は不要です。

```bash
go build -o rss-curator .

export OPENAI_API_KEY='...'
# 任意：403・429の記事を次回から代替取得する場合
export CRAWLBASE_TOKEN='...'
export CRAWLBASE_MAX_REQUESTS=5
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

RSS取得、記事取得・本文抽出、APIの失敗は他の記事を止めません。再試行できる失敗は未処理のまま残し、取得状態に従って次の実行で再試行します。401・404・410や再試行上限に達した取得失敗は停止します。OpenAI SDK内での再試行は無効にし、APIの失敗も次の定期実行で再試行します。要約が返った記事から順に、シェルから`notify_slack`で投稿します。投稿と状態の永続化が成功したら1秒待ち、次の記事へ進みます。Slack投稿または投稿成功後の状態保存に失敗したら即座に終了し、後続記事のAPIは呼びません。未投稿の要約は一時ファイルのため、次回の定期実行では再生成します。

設定不備、壊れた状態ファイル、状態保存・pushの失敗は全体のエラーとして扱います。状態を削除すると初回動作に戻るため、復旧時はまずActionsのログと`rss-state`を確認してください。

## 開発時の確認

```bash
go test ./...
go vet ./...
staticcheck -checks="all,-ST1000" ./...
```

通常のテストは外部APIを呼びません。Crawlbaseは模擬HTTPでステータス判定、次回切り替え、Retry-Afterとホスト待機、再試行上限、実行内の利用上限、秘密情報の非出力を検証し、一時Gitリポジトリで取得状態の保存・復元も確認します。RSSの新着検出、本文抽出、重複排除、モードごとの分岐に加え、模擬Slack CLIと一時Gitリポジトリで、投稿成功後だけ状態を確定・保存することを確認します。
