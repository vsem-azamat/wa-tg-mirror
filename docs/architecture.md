# How it works

One process holds one WhatsApp linked-device session and long-polls every bot.

## WhatsApp to Telegram

1. Status broadcasts and channels are skipped. A message the bridge itself sent comes back as an echo and is dropped by its ID.
2. The chat and sender are reduced to phone-number JIDs: WhatsApp may address people by LID, and the session store maps LIDs to numbers.
3. Routing:
   - a direct chat with a configured person goes to the owner through that person's bot;
   - a configured group goes to its Telegram group, through the sender's bot when they have one and through the service bot with their name in bold otherwise;
   - any other chat goes to its overflow topic, created on first use and named after the contact or group; ignored chats are dropped.
4. Media is downloaded and re-uploaded. Files over the Bot API limit of 50 MB arrive as a note, checked against the announced size before downloading. Animated stickers arrive as files.
5. A reply in WhatsApp becomes a reply to the mirrored Telegram message when it is known.
6. Delivery recovers where it can:
   - when Telegram limits the rate (429), the call waits as long as Telegram asks and is repeated, which keeps messages in order;
   - when a person's bot may not write (it is not in the group, or you never started it), the service bot posts the message with the sender's name, and in the private case tells you once which bot to start;
   - when an overflow topic was deleted, it is created again.

Each message gets five minutes in total, so a stuck request cannot stall the events behind it.

Messages you send from another device (your phone) are mirrored with a 📱 header.

Events on mirrored messages, found through the message links:

- **Edits** replace the text or caption of the Telegram copy, marked "✏️ edited", through the bot that posted it.
- **Deletions for everyone** delete the Telegram copy; after Telegram's 48 hours, when a bot may no longer delete, the copy is replaced by "🗑 deleted in WhatsApp".
- **Reactions** are set by the bot of the person who reacted (in a private chat only that chat's bot can react). Telegram offers bots a fixed set of emoji; skin tones are dropped, and an emoji outside the set, or a reaction by someone without a bot, arrives as a short reply instead. Your own reactions from the phone are not shown.
- **Calls** are noted in the caller's chat; **group changes** (renamed, description, members joining or leaving) are noted by the service bot.

Typing indicators are not mirrored: WhatsApp sends them only to clients that show themselves online, which would keep you online all the time.

## History

`backfill` links a new device with a full history sync, collects the messages of the last days from the history sync events (and live messages that arrive meanwhile), sorts them by time and passes each through the same routing as live messages. History messages carry a time stamp, each Telegram chat gets a day marker when the day changes, already mirrored messages are skipped and no read receipts are sent.

## Telegram to WhatsApp

Only messages from `owner_id` are handled.

- A person's bot accepts messages in its private chat and sends them to that person. In groups it stays silent; the service bot handles group messages.
- The service bot sends what you write in a mirrored group to that WhatsApp group, and what you write in an overflow topic to that topic's chat.
- A reply to a mirrored message becomes a WhatsApp reply that quotes it.
- Files over 20 MB cannot be downloaded by bots and are refused. Animated and video stickers are sent as their emoji.
- Telegram service messages (members added, title changed, topics edited) are not sent on.
- An edit of your text message is sent as a WhatsApp edit (WhatsApp accepts them for 15 minutes); caption edits are not.
- Your reaction to a mirrored message becomes a WhatsApp reaction. Telegram reports reactions to administrators in groups, so the service bot handles them there.
- `/del` in reply to your own message deletes it in WhatsApp for everyone and removes both messages in Telegram. Other `/` commands are not sent.
- When a message cannot be sent, the bot replies with the reason.

With `mark_read`, sending to a chat marks the messages received in it since the last send as read.

## State

The SQLite database holds the whatsmeow session tables and three bridge tables:

| Table | Contents |
| --- | --- |
| `mirror_links` | Telegram message ↔ WhatsApp message, with a short copy of the text for quotes |
| `mirror_topics` | WhatsApp chat ↔ overflow topic |
| `mirror_avatars` | WhatsApp picture ID last set as each bot's photo |

Bot names are set at every connection when they differ. Bot photos are set when the person's WhatsApp picture ID changes, at connection and on picture-change events.
