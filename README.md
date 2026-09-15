# mailinfo

指定した Maildir を監視し、対象メーリングリストの件名だけを Discord webhook に通知する小さな Go アプリです。

## 仕様

- 平日 6:00 以上 22:00 未満にだけ動作します。
- 1分おきに `MAILDIR_PATH` で指定した Maildir を確認します。
- Discord へ送る本文は件名のみです。
- 件名が空の場合は `None` を送ります。
- 処理済み Maildir key を `STATE_FILE` に保存し、再起動後の二重通知を避けます。
- 初回起動時は既存メールを通知せず、その時点で存在するメールを処理済みにします。
- `REQUIRE_STAFF_ADDRESS=true` の場合、`STAFF_ADDRESS` に関係するメールだけを通知します。
- `EXCLUDE_FILE` に改行区切りで書いた正規表現に From アドレスが一致した場合は通知しません。
- 件名中のメールアドレスは、`@` より前にある小文字アルファベットを削除してから通知します。

例:

```text
contact abc123@example.com
```

は次のように通知されます。

```text
contact 123@example.com
```

## 設定

`.env.example` を参考に、systemd の `EnvironmentFile` などで環境変数を渡してください。

必須:

```sh
DISCORD_WEBHOOK_URL="https://discord.com/api/webhooks/..."
MAILDIR_PATH="/home/notify-user/Maildir"
STAFF_ADDRESS="staff@example.test"
```

任意:

```sh
REQUIRE_STAFF_ADDRESS="true"
TIMEZONE="Asia/Tokyo"
WORK_START_HOUR="6"
WORK_END_HOUR="22"
POLL_INTERVAL="1m"
STATE_FILE="private/state.json"
EXCLUDE_FILE="private/exclude_senders.txt"
```

メールサーバ上で Maildir を直接読むため、IMAP のユーザー名やパスワードは不要です。

## 除外フィルタ

`private/exclude_senders.txt` に、通知しない送信者アドレスの正規表現を1行ずつ書きます。

```text
# コメントと空行は無視されます
^noreply@example\.com$
@example\.invalid$
```

## ビルド

```sh
go build -o mailinfo .
```

## systemd 例

`/etc/systemd/system/mailinfo.service`:

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

[Install]
WantedBy=multi-user.target
```

反映:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now mailinfo.service
```
