# mailinfo

<p style="display: inline">
	<img src="https://img.shields.io/badge/-Go-00ADD8.svg?logo=go&style=for-the-badge&logoColor=white">
	<img src="https://img.shields.io/badge/-Discord-5865F2.svg?logo=discord&style=for-the-badge&logoColor=white">
</p>

このプロジェクトは，Maildirを定期的に監視し，条件に一致する新着メールの件名を外部へ通知する常駐プログラムです．Go標準ライブラリのみで実装しており，メールサーバーやデータベースを別途用意せずに動作します．

内部は「Maildir監視 → メールイベント → 通知先プラグイン」という構造になっており，通知先はDiscordに限らず，Slack，汎用Webhook，メール（SMTP），ログファイル，デスクトップ通知（notify-send）のいずれか，またはそれらの組み合わせを選べます．

## 概要

`MAILDIR_PATH`で指定したMaildirの`new/`と`cur/`を定期的に走査し，未処理のメールを検出します．平日かつ指定した時間帯に，宛先と送信元の条件を満たしたメールの件名を，ルーティングルールで振り分けた通知先へ送ります．

主な機能として，以下を扱います．

- Maildirの`new/`および`cur/`にあるメールの定期監視
- `To`，`Cc`，`Delivered-To`などのヘッダーによる宛先判定
- メーリングリストの`local.domain`，`local-request@domain`，`local-owner@domain`形式への対応
- 正規表現ファイルによる送信元の除外
- 平日および業務時間帯による通知制限
- 通知済みメールキーのJSONファイル管理
- 件名内メールアドレスのサニタイズ
- 件名のMIMEデコード（RFC 2047のencoded-word）．ISO-2022-JPやShift_JISなど，Go標準ライブラリが扱わない文字コードは`iconv`コマンド経由でUTF-8へ変換
- 差出人・件名・本文の条件による通知先の振り分け（送信スキップを含む）．通知先の種類（Discord/Slack/Webhook/ログ/デスクトップ通知/メール）を問わず，target名で共通のルールファイルを使う
- Discord Webhook，Slack Incoming Webhook，任意のWebhook URL，ログファイル，デスクトップ通知，メール（SMTP）への新着メール件名の通知

## 制約

- 監視対象はMaildir形式で，`new/`または`cur/`ディレクトリが必要です．
- 初回起動時は，既存メールを通知済みとして状態ファイルへ登録します．既存メールを遡って通知する機能はありません．
- 通知済み判定はMaildirファイル名から生成したキーに依存します．状態ファイルを削除すると，状態が失われます．
- 通知は平日の指定時間帯だけ行われ，土曜日と日曜日は通知しません．
- 複数プロセスによる同一状態ファイルの同時更新は想定していません．
- 同じtarget名を，Discord・Slack・Webhook・ログ・デスクトップ通知・メールのうち複数の種類にまたがって定義することはできません（起動時にエラーとなります）．

## 要件

- Go `1.26`系
- 読み取り可能なMaildir
- 状態ファイルを書き込めるディレクトリ
- 少なくとも1つの通知先設定（Discord Webhook URL，Slack Webhook URL，汎用Webhook URL，ログ出力先，デスクトップ通知，メールのいずれか）
- `iconv`コマンド（ISO-2022-JPやShift_JISなど，UTF-8以外の文字コードで書かれた件名をデコードする場合に必要．多くのLinuxディストリビューションに標準で含まれる）
- `notify-send`コマンド（デスクトップ通知を使う場合に必要．GUI環境を持たないサーバーでは利用できない）

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

起動後は標準ログを確認し，テスト用メールを対象Maildirへ送信して，設定した通知先へ通知されることを確認します．確認が終わったら`Ctrl+C`で停止します．`.env`にはWebhook URLやSMTP認証情報などの秘密情報が含まれるため，ファイルの権限を適切に設定してください．

## systemdでの運用

本番環境で常駐運用する場合は，systemdのサービスとして登録します．次の内容を`/etc/systemd/system/mailinfo.service`に保存してください．

```ini
[Unit]
Description=Notify configured targets when staff mailing list receives mail
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
| `DISCORD_WEBHOOK_URLS` | 条件付き | なし | Discord通知先target名とWebhook URLの対応表（`name1=url1,name2=url2`形式） |
| `SLACK_WEBHOOK_URLS` | 条件付き | なし | Slack通知先target名とIncoming Webhook URLの対応表（`name1=url1,name2=url2`形式） |
| `WEBHOOK_URLS` | 条件付き | なし | 汎用Webhook通知先target名とURLの対応表（`name1=url1,name2=url2`形式）．メールイベントをJSONのままPOSTする |
| `LOG_TARGETS` | 条件付き | なし | ログ通知先target名と出力先ファイルパスの対応表（`name1=path1,name2=path2`形式） |
| `DESKTOP_TARGETS` | 条件付き | なし | デスクトップ通知（`notify-send`）を使うtarget名のカンマ区切りリスト（`name1,name2`形式） |
| `EMAIL_TARGETS` | 条件付き | なし | メール通知先target名と宛先メールアドレスの対応表（`name1=to1@example.com,name2=to2@example.com`形式） |
| `SMTP_HOST` | 条件付き | なし | `EMAIL_TARGETS`使用時に必須．送信に使うSMTPサーバーのホスト名 |
| `SMTP_PORT` | いいえ | `587` | SMTPサーバーのポート番号 |
| `SMTP_USER` | いいえ | なし | SMTP認証のユーザー名．未指定の場合は認証なしで接続する |
| `SMTP_PASS` | いいえ | なし | SMTP認証のパスワード |
| `SMTP_FROM` | 条件付き | なし | `EMAIL_TARGETS`使用時に必須．送信元メールアドレス |
| `DISCORD_ROUTE_FILE` | いいえ | `private/discord_routes.txt` | 通知先振り分けルールファイル．通知先の種類によらず共通で使う（歴史的経緯でDiscordという名前が残っている） |
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

`REQUIRE_STAFF_ADDRESS`が有効な場合，`STAFF_ADDRESS`は必須です．`DISCORD_WEBHOOK_URL(S)`，`SLACK_WEBHOOK_URLS`，`WEBHOOK_URLS`，`LOG_TARGETS`，`DESKTOP_TARGETS`，`EMAIL_TARGETS`のうち，少なくとも1つを指定する必要があります．同じtarget名を複数の種類にまたがって定義した場合は起動時にエラーとなります．`.env`やWebhook URL，SMTP認証情報は公開しないでください．

### 送信元除外ファイル

`EXCLUDE_FILE`には，1行につき1つの正規表現を記述します．空行と`#`で始まる行は無視されます．

```text
# example: automated senders
no-reply@example\.com$
mailer-daemon@.*
```

### 通知先の仕組みとルーティング

このプログラムの内部は，「Maildir監視 → メールイベント → 通知先プラグイン」という3層構造になっています．Maildirの走査とメールヘッダーの解析は，通知先の種類を一切知りません．解析結果は`MailEvent`（メールのキー・件名・差出人・ルーティング先target名・時刻）としてまとめられ，`Notifier`インターフェース（`Notify(event MailEvent) error`）を満たす通知先プラグインへ渡されるだけです．そのため，Discord・Slack・汎用Webhook・ログ・デスクトップ通知・メールのどれを使う場合も，Maildir監視やルーティングのロジックには手を入れる必要がありません．

通知先は，種類ごとの環境変数（`DISCORD_WEBHOOK_URLS`，`SLACK_WEBHOOK_URLS`，`WEBHOOK_URLS`，`LOG_TARGETS`，`DESKTOP_TARGETS`，`EMAIL_TARGETS`）でtarget名を登録し，`DISCORD_ROUTE_FILE`（既定は`private/discord_routes.txt`）に，差出人・件名・本文の条件でtargetを振り分けるルールを記述します．通知先の種類が異なっていても，target名とルールファイルの書き方は共通です．target名の追加・削除は各環境変数の値を変更するだけで行え，振り分け条件の追加・削除は`DISCORD_ROUTE_FILE`の行を増減するだけで行えます．同じtarget名を複数の種類にまたがって登録することはできません．

`DISCORD_WEBHOOK_URLS`・`SLACK_WEBHOOK_URLS`・`WEBHOOK_URLS`・`LOG_TARGETS`・`EMAIL_TARGETS`は，`name1=value1,name2=value2`のようにカンマ区切りで指定します．`DESKTOP_TARGETS`だけは付随する値を持たないため，`name1,name2`のようにtarget名だけをカンマ区切りで指定します．

```dotenv
DISCORD_WEBHOOK_URLS="A=https://discord.com/api/webhooks/AAA/TOKEN,B=https://discord.com/api/webhooks/BBB/TOKEN"
SLACK_WEBHOOK_URLS="C=https://hooks.slack.com/services/T000/B000/XXXX"
LOG_TARGETS="D=private/mail-alert.log"
DESKTOP_TARGETS="E"
DISCORD_DEFAULT_TARGET="B"
```

`DISCORD_ROUTE_FILE`には，1行につき1ルールを記述します．タブ区切りで複数の条件を並べ，最後の要素をtargetとします．同じ行内の条件はすべてを満たした場合にのみマッチします（AND）．条件は`field:regex`（一致すればマッチ）または`!field:regex`（一致しなければマッチ）の形式で指定し，`field`には`from`（差出人），`subject`（件名），`body`（本文，MIMEデコード前の生テキスト），`text`（件名と本文を結合したもの），`header:<ヘッダー名>`（任意のメールヘッダーの値）を指定できます．ルールは上から順に評価し，最初にマッチした行のtargetを採用します．targetに`skip`を指定すると，そのメールはどの通知先へも通知しません．どの行にもマッチしない場合は，`DISCORD_DEFAULT_TARGET`に対応するtargetへ通知します．target名に対応する通知先が見つからない場合は，通知をスキップします．target名は，登録した環境変数のキー名と1文字も違わず一致させる必要があります．

`header:<ヘッダー名>`は，`sympa`や`fml`などのメーリングリストソフトを経由すると`From`が配信用アドレスに書き換えられ，元の送信元アドレスが`X-Original-From`など別のヘッダーに残るケースに対応するためのものです．`header:X-Original-From:^managers@example\.com$`のように指定すると，該当ヘッダーの値（`textproto.MIMEHeader.Get`と同様，ヘッダー名の大文字小文字は区別しません）に対して正規表現マッチを行います．

次の例は，架空のドメイン・アドレスを用いた設定イメージです．自組織のメールドメイン（`example.com`）およびメーリングリスト中継ドメイン（`list.example.com`）以外の差出人を通知対象から除外し，差出人ごと・キーワードごとに通知先を振り分けます．メーリングリスト経由で`From`が書き換わる場合に備えて，`header:X-Original-From`による判定と，直接送信された場合の`from`による判定の両方を用意しています．

```text
# field:regex[<TAB>field:regex...]<TAB>target

# メールゲートウェイがspamと判定したメールは，送信元ドメインを詐称していても通知しない
header:X-KSMG-AntiSpam-Status:^spam$	skip

!from:@(?:[^@]*\.)?example\.com$	!from:@list\.	skip

header:X-Original-From:^managers@example\.com$	B
from:^managers@example\.com$	B

header:X-Original-From:^nagios@.*list\.example\.com$	C
from:^nagios@.*list\.example\.com$	C
header:X-Original-From:^staff@	C
from:^staff@	C
header:X-Original-From:^ipdb-admin@example\.com$	C
from:^ipdb-admin@example\.com$	C

text:(お願い|トラブル|していただ)	A
```

ブラックリストによる除外は，既存の`EXCLUDE_FILE`（送信元除外ファイル）で判定されるため，このファイルには含めていません．空行と`#`で始まる行は無視されます．

### 通知先プラグインの一覧

- Discord（`DISCORD_WEBHOOK_URLS`）
	Discord WebhookへJSON（`{"content": "<件名>"}`）をPOSTします．
- Slack（`SLACK_WEBHOOK_URLS`）
	Slack Incoming WebhookへJSON（`{"text": "<件名>"}`）をPOSTします．
- 汎用Webhook（`WEBHOOK_URLS`）
	指定したURLへ，メールのキー・件名・差出人・target名・時刻を含む`MailEvent`をそのままJSONでPOSTします．Discord/Slack向けの固定フォーマットに合わない，自作の受信側や他のシステムと連携する場合に使います．
- ログ（`LOG_TARGETS`）
	指定したファイルへ，時刻・target名・差出人・件名をタブ区切りで1行追記します．常駐プログラム自体の標準ログとは別に，他のログ収集基盤で監視したい場合に使います．
- デスクトップ通知（`DESKTOP_TARGETS`）
	`notify-send`コマンド経由でデスクトップ通知を表示します．GUI環境を持たないサーバー上での常駐運用には向きません．
- メール（`EMAIL_TARGETS`）
	`SMTP_HOST`以下で設定したSMTPサーバー経由で，target毎に指定した宛先へメールを送信します．

### 件名の文字コード（MIMEデコード）

件名がRFC 2047のencoded-word（`=?charset?B?...?=`など）でエンコードされている場合，UTF-8とUS-ASCIIはGo標準ライブラリでデコードします．ISO-2022-JPやShift_JISなど，それ以外の文字コードは`golang.org/x/text`に頼らず，`iconv`コマンドへ生バイト列をパイプしてUTF-8へ変換します．そのため，動作環境に`iconv`コマンドが必要です．`iconv`が使えない，または対象の文字コードを変換できない場合，件名はデコードされずヘッダーの生の値（`=?ISO-2022-JP?B?...?=`のような文字列）のままDiscordへ通知されます．

## テスト

```bash
go test ./...
```

`main_test.go`で，件名のサニタイズ，通知時間帯，宛先判定，通知先ルーティング，各通知先プラグイン（ログ出力・デスクトップ通知の引数組み立て・メール本文組み立て・複数種類のtarget統合）のテストを実施しています．

## プロジェクト構成

- `main.go`
	設定読み込み，Maildir走査，メールヘッダー解析，フィルタ，通知先ルーティング，状態管理，各通知先プラグイン（Discord/Slack/汎用Webhook/ログ/デスクトップ通知/メール）
- `main_test.go`
	主要な判定ロジックのテスト
- `.env.example`
	環境変数の設定例
- `private/`
	状態ファイルや送信元除外設定など，公開しない運用データの配置先

## ライセンス

このプロジェクトはMITライセンスのもとで公開されています．詳細は[LICENSE](./LICENSE)を参照してください．
