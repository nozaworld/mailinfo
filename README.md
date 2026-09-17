# mailinfo

<p style="display: inline">
	<img src="https://img.shields.io/badge/-Go-00ADD8.svg?logo=go&style=for-the-badge&logoColor=white">
	<img src="https://img.shields.io/badge/-Discord-5865F2.svg?logo=discord&style=for-the-badge&logoColor=white">
</p>

このプロジェクトは，Maildirを定期的に監視し，条件に一致する新着メールの件名をDiscord Webhookへ通知する常駐プログラムです．Go標準ライブラリのみで実装しており，メールサーバーやデータベースを別途用意せずに動作します．

## 概要

`MAILDIR_PATH`で指定したMaildirの`new/`と`cur/`を定期的に走査し，未処理のメールを検出します．平日かつ指定した時間帯に，宛先と送信元の条件を満たしたメールの件名をDiscordへ投稿します．

主な機能として，以下を扱います．

- Maildirの`new/`および`cur/`にあるメールの定期監視
- `To`，`Cc`，`Delivered-To`などのヘッダーによる宛先判定
- メーリングリストの`local.domain`，`local-request@domain`，`local-owner@domain`形式への対応
- 正規表現ファイルによる送信元の除外
- 平日および業務時間帯による通知制限
- 通知済みメールキーのJSONファイル管理
- 件名内メールアドレスのサニタイズ
- 差出人・件名・本文の条件によるDiscord通知先の振り分け（送信スキップを含む）
- Discord Webhookへの新着メール件名の通知

## 制約

- 監視対象はMaildir形式で，`new/`または`cur/`ディレクトリが必要です．
- 初回起動時は，既存メールを通知済みとして状態ファイルへ登録します．既存メールを遡って通知する機能はありません．
- 通知済み判定はMaildirファイル名から生成したキーに依存します．状態ファイルを削除すると，状態が失われます．
- 通知は平日の指定時間帯だけ行われ，土曜日と日曜日は通知しません．
- 複数プロセスによる同一状態ファイルの同時更新は想定していません．
- 通知先はDiscord Webhookに限定しており，Slackなど他の通知先には対応していません．

## 要件

- Go `1.26`系
- 読み取り可能なMaildir
- 状態ファイルを書き込めるディレクトリ
- 通知先のDiscord Webhook URL

## 使い方

1. リポジトリをクローンします．

```bash
git clone https://github.com/nozaworld/mailinfo.git
cd mailinfo
```

2. `.env.example`を参考に環境変数を設定します．`.env`を使用する場合は，シェルから読み込んでください．

```bash
cp .env.example .env
# .envの値を環境に合わせて編集
set -a
. ./.env
set +a
```

3. ビルドして起動します．

```bash
go build -o mailinfo .
./mailinfo
```

プログラムは終了するまで常駐し，処理状況やスキップ理由を標準ログへ出力します．設定値が不足している場合や，タイムゾーン・間隔・時刻の形式が不正な場合は起動時に終了します．

### SSHでの本番環境テスト

本番サーバーへSSH接続し，まずフォアグラウンドで起動して設定と通知を確認します．`/opt/mailinfo`に配置した場合の例です．

```bash
ssh user@example.com
cd /opt/mailinfo
set -a
. ./.env
set +a
./mailinfo
```

起動後は標準ログを確認し，テスト用メールを対象Maildirへ送信してDiscordへ通知されることを確認します．確認が終わったら`Ctrl+C`で停止します．`.env`にはDiscord Webhook URLなどの秘密情報が含まれるため，ファイルの権限を適切に設定してください．

## systemdでの運用

本番環境で常駐運用する場合は，systemdのサービスとして登録します．次の内容を`/etc/systemd/system/mailinfo.service`に保存してください．

```ini
[Unit]
Description=Notify Discord when staff mailing list receives mail
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/mailinfo
EnvironmentFile=/opt/mailinfo/.env
ExecStart=/opt/mailinfo/mailinfo
Restart=always
RestartSec=10
User=root
Group=root

[Install]
WantedBy=multi-user.target
```

サービスを登録して起動します．

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mailinfo.service
```

稼働状態とログは次のコマンドで確認できます．

```bash
sudo systemctl status mailinfo.service
sudo journalctl -u mailinfo.service -f
```

設定やバイナリを更新した場合は，サービスを再起動します．

```bash
sudo systemctl restart mailinfo.service
```

停止または自動起動の無効化を行う場合は，次のコマンドを使用します．

```bash
sudo systemctl disable --now mailinfo.service
```

### 環境変数

| 環境変数 | 必須 | 既定値 | 説明 |
| --- | --- | --- | --- |
| `DISCORD_WEBHOOK_URL` | 条件付き | なし | 後方互換用の単一Webhook URL．指定すると`default`という名前で`DISCORD_WEBHOOK_URLS`に登録される |
| `DISCORD_WEBHOOK_URLS` | 条件付き | なし | 通知先target名とWebhook URLの対応表（`name1=url1,name2=url2`形式） |
| `DISCORD_ROUTE_FILE` | いいえ | `private/discord_routes.txt` | 通知先振り分けルールファイル |
| `DISCORD_DEFAULT_TARGET` | いいえ | `default` | どのルールにも一致しなかった場合に使うtarget名 |
| `MAILDIR_PATH` | はい | なし | 監視するMaildirのパス |
| `STAFF_ADDRESS` | 条件付き | なし | 宛先判定に使うメールアドレス |
| `REQUIRE_STAFF_ADDRESS` | いいえ | `true` | `false`などを指定すると宛先判定を無効化 |
| `TIMEZONE` | いいえ | `Asia/Tokyo` | 業務時間と曜日の判定に使うタイムゾーン |
| `WORK_START_HOUR` | いいえ | `6` | 通知を開始する時刻 |
| `WORK_END_HOUR` | いいえ | `22` | 通知を終了する時刻（終了時刻は含まない） |
| `POLL_INTERVAL` | いいえ | `1m` | Maildirを確認する間隔（Goのduration形式） |
| `STATE_FILE` | いいえ | `private/state.json` | 通知済みメールキーの保存先 |
| `EXCLUDE_FILE` | いいえ | `private/exclude_senders.txt` | 送信元除外正規表現ファイル |

`REQUIRE_STAFF_ADDRESS`が有効な場合，`STAFF_ADDRESS`は必須です．`DISCORD_WEBHOOK_URL`と`DISCORD_WEBHOOK_URLS`は，どちらか一方，または両方を指定する必要があります．`.env`やDiscord Webhook URLは公開しないでください．

### 送信元除外ファイル

`EXCLUDE_FILE`には，1行につき1つの正規表現を記述します．空行と`#`で始まる行は無視されます．

```text
# example: automated senders
no-reply@example\.com$
mailer-daemon@.*
```

### Discord通知先のルーティング

`DISCORD_WEBHOOK_URLS`で複数のDiscord Webhook URLをtarget名付きで登録しておき，`DISCORD_ROUTE_FILE`（既定は`private/discord_routes.txt`）に，差出人・件名・本文の条件で通知先を振り分けるルールを記述します．target名の追加・削除は`DISCORD_WEBHOOK_URLS`の値を変更するだけで行え，振り分け条件の追加・削除は`DISCORD_ROUTE_FILE`の行を増減するだけで行えます．

`DISCORD_WEBHOOK_URLS`は，`name1=url1,name2=url2`のようにカンマ区切りで指定します．

```dotenv
DISCORD_WEBHOOK_URLS="A=https://discord.com/api/webhooks/AAA/TOKEN,B=https://discord.com/api/webhooks/BBB/TOKEN,C=https://discord.com/api/webhooks/CCC/TOKEN,D=https://discord.com/api/webhooks/DDD/TOKEN"
DISCORD_DEFAULT_TARGET="D"
```

`DISCORD_ROUTE_FILE`には，1行につき1ルールを記述します．タブ区切りで複数の条件を並べ，最後の要素をtargetとします．同じ行内の条件はすべてを満たした場合にのみマッチします（AND）．条件は`field:regex`（一致すればマッチ）または`!field:regex`（一致しなければマッチ）の形式で指定し，`field`には`from`（差出人），`subject`（件名），`body`（本文，MIMEデコード前の生テキスト），`text`（件名と本文を結合したもの）を指定できます．ルールは上から順に評価し，最初にマッチした行のtargetを採用します．targetに`skip`を指定すると，そのメールはどのDiscord Webhookへも通知しません．どの行にもマッチしない場合は，`DISCORD_DEFAULT_TARGET`に対応するURLへ通知します．target名に対応するURLが`DISCORD_WEBHOOK_URLS`に見つからない場合は，通知をスキップします．target名は`DISCORD_WEBHOOK_URLS`のキー名と1文字も違わず一致させる必要があります．

次の例は，学内（`.nagoya-u.ac.jp`）・生協（`coop.`）ドメイン以外の差出人を通知対象から除外し，差出人ごと・キーワードごとにDiscordの通知先を振り分けます（`private/discord_routes.txt`の実際の内容）．

```text
# field:regex[<TAB>field:regex...]<TAB>target
!from:@(?:[^@]*\.)?nagoya-u\.ac\.jp$	!from:@coop\.	skip

from:^managers@nagoya-u\.ac\.jp$	B

from:^nagios@.*coop\.nagoya-u\.ac\.jp$	C
from:^staff@	C
from:^ipdb-admin@icts\.nagoya-u\.ac\.jp$	C

text:(お願い|トラブル|していただ)	A
```

ブラックリストによる除外は，既存の`EXCLUDE_FILE`（送信元除外ファイル）で判定されるため，このファイルには含めていません．空行と`#`で始まる行は無視されます．

## テスト

```bash
go test ./...
```

`main_test.go`で，件名のサニタイズ，通知時間帯，宛先判定のテストを実施しています．

## プロジェクト構成

- `main.go`
	設定読み込み，Maildir走査，メールヘッダー解析，フィルタ，通知先ルーティング，状態管理，Discord通知
- `main_test.go`
	主要な判定ロジックのテスト
- `.env.example`
	環境変数の設定例
- `private/`
	状態ファイルや送信元除外設定など，公開しない運用データの配置先

## ライセンス

このプロジェクトはMITライセンスのもとで公開されています．詳細は[LICENSE](./LICENSE)を参照してください．
