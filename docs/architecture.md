# How it works

One process holds one WhatsApp linked-device session and long-polls every bot.

## WhatsApp to Telegram

1. Status broadcasts and channels are skipped. A message the bridge itself sent comes back as an echo and is dropped by its ID.
2. The chat and sender are reduced to phone-number JIDs: WhatsApp may address people by LID, and the session store maps LIDs to numbers.
3. Routing:
   - a direct chat with a configured person goes to the owner through that person's bot;
   - a configured group goes to its Telegram group, through the sender's bot when they have one and through the service bot with their name in bold otherwise;
   - any other chat goes to its overflow topic, created on first use and named after the contact or group; ignored chats are dropped.
4. Locations and venues, contact cards and polls become native Telegram messages; they take no caption, so a sender's name or a caption goes in a message just before them. A live location shows its first position only; later positions of the same share (within eight hours) are not posted again. vCards over 2048 bytes are sent without the vCard. Polls with more than 12 options list the rest in the message before them. Telegram polls take 2 to 12 options of up to 100 characters.
5. Media is downloaded and re-uploaded. Files over the Bot API limit of 50 MB arrive as a note, checked against the announced size before downloading. Animated stickers arrive as files.
6. A reply in WhatsApp becomes a reply to the mirrored Telegram message when it is known.
7. Delivery recovers where it can:
   - when Telegram limits the rate (429), the call waits as long as Telegram asks and is repeated, which keeps messages in order;
   - when a person's bot may not write (it is not in the group, or you never started it), the service bot posts the message with the sender's name, and in the private case tells you once which bot to start;
   - when an overflow topic was deleted, it is created again.

Each message gets five minutes in total, so a stuck request cannot stall the events behind it.

Messages you send from another device (your phone) are mirrored with a 📱 header.

Events on mirrored messages, found through the message links:

- **Edits** replace the text or caption of the Telegram copy, marked "✏️ edited", through the bot that posted it.
- **Deletions for everyone** delete the Telegram copy; after Telegram's 48 hours, when a bot may no longer delete, the copy is replaced by "🗑 deleted in WhatsApp".
- **Reactions** are set by the bot of the person who reacted (in a private chat only that chat's bot can react). Telegram offers bots a fixed set of emoji; skin tones are dropped, and an emoji outside the set arrives as a short reply instead. A bot holds one reaction per message, so reactions of people without a bot arrive as replies from the service bot that name them. Your own reactions from the phone are not shown.
- **Poll votes** are decrypted and arrive as replies to the poll naming the choice ("🗳 Sat"), from the voter's bot or from the service bot with the voter's name: bots cannot vote for others in Telegram.
- **Calls** are noted in the caller's chat; **group changes** (renamed, description, members joining or leaving) are noted by the service bot. In overflow chats this happens only once the chat has a topic.

Links stored before the bridge recorded which bot posted a message are resolved from the chat and the sender.

Typing is mirrored one way, while you are active. WhatsApp sends typing only to clients that show themselves online, and while one is online the phone gets no notifications. So the bridge marks you online for three minutes after each message, edit, reaction, vote or deletion you send through the mirror, and marks you unavailable again after that. In this window, someone typing or recording a voice message shows as the bot typing or recording, in the chat where their message would appear. Typing is not shown in a forum chat that has no topic yet. Telegram does not tell bots when you type, so your typing does not reach WhatsApp.

## History

`backfill` links a new device with a full history sync, collects the messages of the last days from the history sync events (and live messages that arrive meanwhile), sorts them by time and passes each through the same routing as live messages. History messages carry a time stamp, each Telegram chat gets a day marker when the day changes, already mirrored messages are skipped and no read receipts are sent.

## Telegram to WhatsApp

Only messages from `owner_id` are handled.

- A person's bot accepts messages in its private chat and sends them to that person. In groups it stays silent; the service bot handles group messages.
- The service bot sends what you write in a mirrored group to that WhatsApp group, and what you write in an overflow topic to that topic's chat.
- A reply to a mirrored message becomes a WhatsApp reply that quotes it.
- Files over 20 MB cannot be downloaded by bots and are refused. Animated and video stickers are sent as their emoji.
- Telegram service messages (members added, title changed, topics edited) are not sent on.
- An edit of your text message is sent as a WhatsApp edit (WhatsApp accepts them for 15 minutes); caption edits are not, and the updates of a live location are not edits.
- Your reaction to a mirrored message becomes a WhatsApp reaction; custom-emoji and paid reactions are not sent. Telegram reports reactions to administrators in groups, so the service bot handles them there.
- Locations, venues, contact cards and polls you send become native WhatsApp messages; your vote in a mirrored poll (the bridge sends polls as non-anonymous) becomes your WhatsApp vote.
- `/del` in reply to your own message deletes it in WhatsApp for everyone and removes both messages in Telegram. Other `/` commands are not sent.
- When a message cannot be sent, the bot replies with the reason.

With `mark_read`, sending to a chat marks the messages received in it since the last send as read.

## State

The SQLite database holds the whatsmeow session tables and the bridge tables:

| Table | Contents |
| --- | --- |
| `mirror_links` | Telegram message ↔ WhatsApp message, with a short copy of the text for quotes |
| `mirror_topics` | WhatsApp chat ↔ overflow topic |
| `mirror_polls` | WhatsApp poll ↔ Telegram poll ID, with the option names to decode votes |
| `mirror_avatars` | WhatsApp picture ID last set as each bot's photo |

Bot names are set at every connection when they differ. Bot photos are set when the person's WhatsApp picture ID changes, at connection and on picture-change events.
