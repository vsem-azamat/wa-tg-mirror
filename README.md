# wa-tg-mirror

Read and answer WhatsApp chats from Telegram. Every person you choose gets their own Telegram bot, so their WhatsApp chat looks like an ordinary Telegram chat with their name and photo. WhatsApp groups become Telegram supergroups where each member speaks through their own bot. Every other chat can land in a forum supergroup, one topic per chat.

The bridge links to your WhatsApp account as a linked device (like WhatsApp Web) through [whatsmeow](https://github.com/tulir/whatsmeow), and talks to Telegram through the Bot API with long polling. It opens no ports.

> WhatsApp does not offer an official API for personal accounts. The bridge uses the same protocol as WhatsApp Web; use it with your own account at your own risk.

## What is mirrored

| WhatsApp | Telegram |
| --- | --- |
| A direct chat with a configured person | The private chat with that person's bot |
| A configured group | A Telegram group; members with a bot speak through it, others through the service bot with their name |
| Any other chat | A topic of the overflow forum, unless ignored |

Text, photos, videos, GIFs, voice messages, audio, files, stickers, locations and replies go both ways. Polls and contacts arrive as text. Messages you send from your phone are shown with a 📱 mark. Each person's bot takes their configured name and their WhatsApp profile photo.

## Quick start

```sh
make build
./bin/wa-tg-mirror discover -db wa-tg-mirror.db   # scan the QR code, then list groups and chats
cp config.example.yaml config.yaml                 # fill in bots, people and groups
./bin/wa-tg-mirror serve -config config.yaml
```

See [docs](docs/README.md) for setup of the bots and groups, configuration and operation.

## License

[Apache-2.0](LICENSE)
