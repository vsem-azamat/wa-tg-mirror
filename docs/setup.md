# Setup

## Bots

Create the bots with [@BotFather](https://t.me/BotFather):

- one **service bot**: it writes your messages into WhatsApp groups, speaks for group members who have no bot of their own, runs the overflow forum and sends you the pairing QR code;
- one bot **per person** you want as a separate chat.

The bridge sets each person's bot name from `bot_name` and `people[].name` and its photo from the person's WhatsApp profile picture, so the names given in BotFather do not matter.

Open every bot once and press **Start**: a bot may not write to you before that. Until you do, the bridge delivers that person's messages through the service bot and tells you which bot to start.

## Telegram groups

For every mirrored WhatsApp group, create a Telegram **supergroup** with you, the service bot and the bots of the people in that group, and make the service bot an administrator so it receives every message. Basic groups are refused by the configuration: in them every bot numbers messages on its own, so a reply could not be matched to the message it answers. A new group becomes a supergroup when, for example, topics are switched on and off again, or its history is made visible to new members; it then gets a new `-100…` ID.

A member whose bot is not in the Telegram group still gets through: the service bot posts for them with their name.

For the overflow, create a supergroup with topics enabled and add the service bot as an administrator with the right to manage topics. The bridge creates one topic per chat, and creates it again if it was deleted.

## Pairing WhatsApp

`wa-tg-mirror discover -db <file> -qr-png qr.png` shows a QR code (in the terminal, or in a PNG that is rewritten as the code rotates). Scan it in WhatsApp → Settings → Linked devices → Link a device. The command then waits for the chat list and prints the groups with their members, the recent chats and the saved contacts; use it to fill in the configuration. The database file then holds the session and is used by `serve`.

When `serve` starts without a session, it sends the QR code to the owner through the service bot instead.

The phone must come online at least every 14 days, otherwise WhatsApp unlinks the device.

## Configuration

See [`config.example.yaml`](../config.example.yaml). The file is decoded strictly: unknown keys are errors. Tokens written as `${NAME}` are read from the environment.

| Key | Meaning |
| --- | --- |
| `owner_id` | Your Telegram user ID. Bots ignore everyone else. |
| `database` | SQLite file with the WhatsApp session and the bridge state. |
| `bot_name` | Display name of each person's bot, `{name}` stands for `people[].name`; default `{name}`. |
| `mark_read` | When you write to a chat, its received messages are marked read in WhatsApp. |
| `service_bot.token` | Token of the service bot. |
| `people[]` | `name`, `phone` (international, digits only), `bot.token`. |
| `groups[]` | `name`, `wa_jid` (ends in `@g.us`), `tg_chat_id` (a supergroup, `-100…`). |
| `overflow` | Optional: `tg_chat_id` of the forum supergroup, `ignore` list of WhatsApp chat JIDs not mirrored at all. |
