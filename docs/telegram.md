# Telegram Setup Guide

This guide walks you through connecting **cc-connect** to Telegram, so you can chat with your local Claude Code via a Telegram bot.

## Prerequisites

- A Telegram account
- A machine that can run cc-connect (no public IP needed)
- Claude Code installed and configured

> 💡 **Advantage**: Uses Long Polling mode — no public IP, no domain, no reverse proxy needed.

---

## Step 1: Create a Telegram Bot

### 1.1 Open BotFather

Search for **@BotFather** in Telegram (the official bot manager) and start a chat.

> ⚠️ Make sure it's the verified official BotFather — don't use third-party imitations.

### 1.2 Create a New Bot

Send the command `/newbot`. BotFather will ask you to provide a name and username.

### 1.3 Set the Bot Name

Enter a **display name** for your bot (e.g. `cc-connect`).

### 1.4 Set the Bot Username

Enter a **username** (must end with `bot`, e.g. `cc_connect_bot`).

> 💡 **Naming rules:**
> - Must end with `bot` (case-insensitive)
> - Only letters, numbers, and underscores
> - Must be globally unique

### 1.5 Get the Bot Token

After creation, BotFather will reply with something like:

```
Done! Congratulations on your new bot...
Use this token to access the HTTP API:
1234567890:ABCdefGHIjklMNOpqrsTUVwxyz-123456

Keep your token secure...
```

> ⚠️ Save this token immediately — it's only shown once! If lost, use `/mybots` → select bot → `API Token` → `Revoke current token` to regenerate.

---

## Step 2: Configure cc-connect

Add the token to your `config.toml`:

```toml
[[projects]]
name = "my-project"

# ── Project-level settings ──────────────────────────────────
# admin_from: who can run privileged commands (/shell, /restart, /upgrade).
#   Not set (default) → privileged commands are blocked for everyone.
#   "*" → all allowed users get admin access (only for personal single-user setups).
#   "id1,id2" → only these Telegram user IDs can run privileged commands.
admin_from = "*"

[projects.agent]
type = "claudecode"

[projects.agent.options]
work_dir = "/path/to/your/project"
mode = "default"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz-123456"

# ── Platform-level settings ─────────────────────────────────
# allow_from: who can use this bot.
#   Not set (default) → all users are permitted (a WARN will be logged).
#   "*" → same as not set, but explicit (no WARN).
#   "id1,id2" → only these Telegram user IDs can interact with the bot.
# allow_from = "123456789"

# Optional proxy for Telegram Bot API access.
# Supports URLs like:
#   http://127.0.0.1:7890
#   socks5://127.0.0.1:1080
# proxy = "http://127.0.0.1:7890"
# proxy_username = ""
# proxy_password = ""
```

> **Common mistake:** `admin_from` goes under `[[projects]]` (project level), NOT inside `[projects.platforms.options]`. If placed in the wrong section, it will be silently ignored.
>
> To find your Telegram user ID, send any message to **@userinfobot**.

### 2.1 Optional: Use a Proxy

If your machine cannot reach the Telegram Bot API directly, set a forward proxy in the Telegram platform options:

```toml
[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz-123456"
proxy = "socks5://127.0.0.1:1080"
proxy_username = ""
proxy_password = ""
```

Notes:

- `proxy` accepts HTTP or SOCKS5 proxy URLs.
- Leave `proxy_username` and `proxy_password` empty when the proxy does not require authentication.
- The proxy only affects Telegram Bot API requests for this Telegram platform instance.

---

## Step 3: Get Chat ID (Optional)

If you want to restrict the bot to specific users/groups, you'll need the Chat ID.

Before using `getUpdates` manually, **stop cc-connect and any other poller
using this bot token**. Telegram permits only one receive consumer per bot;
opening the URL while cc-connect runs can interrupt its polling with HTTP 409.
Restart cc-connect after the inspection. Never use a negative offset to inspect
the queue: Telegram forgets earlier pending updates.

### 3.1 Get Your Personal Chat ID

1. Send any message to your bot
2. Visit the following URL (replace `{{TOKEN}}` with your token):

```
https://api.telegram.org/bot{{TOKEN}}/getUpdates
```

3. Find the `chat.id` field in the returned JSON

### 3.2 Get a Group Chat ID

1. Add the bot to a group
2. Send a message mentioning @your_bot in the group
3. Check the `getUpdates` URL — group Chat IDs are usually negative numbers

> Note: Chat ID whitelisting is planned for a future release.

---

## Step 4: Set Bot Commands (Optional)

### 4.1 Set Command Menu

In BotFather, send:

```
/setcommands
```

Select your bot, then enter the command list:

```
help - Show available commands
new - Start a new session
list - List sessions
```

### 4.2 Set Bot Description

```
/setdescription
```

Enter a description — users will see this when they first open the bot.

---

## Step 5: Start cc-connect

### 5.1 Launch

```bash
cc-connect
# Or specify a config file
cc-connect -config /path/to/config.toml
```

### 5.2 Verify Connection

You should see logs like:

```
level=INFO msg="telegram: connected" bot=cc_connect_bot
level=INFO msg="platform started" project=my-project platform=telegram
level=INFO msg="cc-connect is running" projects=1
```

---

## Polling and recovery

The fork owns the receive loop; the Telegram SDK is used for outbound methods.
Startup authenticates with `getMe`, then performs a non-blocking `getUpdates`
request using the current positive offset. **Only a successfully decoded polling
response, including an empty response, marks the platform ready.** Authentication
or a healthy local API socket alone does not establish Telegram delivery health.

- There is one synchronous poller per platform. Long polls use a 59-second
  server timeout with a 90-second HTTP client timeout. `Stop` cancels both
  authentication and in-flight polling; duplicate `Start` calls are rejected.
- A request, body-read, decoding, or Bot API failure immediately marks the
  connection unavailable and returns to the recovery loop. The next attempt
  creates a fresh HTTP client and **independent transport connection pool**,
  preserving proxy, authentication, TLS, and HTTP/2 configuration. Recovery
  does not require a container restart or a DNS/model/credential change.
- Retries back off from 1 to 30 seconds. Completed successful polls over a stable
  interval reset backoff; merely spending time in a failed request does not.
  HTTP 429 `retry_after` can extend the retry delay beyond 30 seconds.
- Updates are dispatched synchronously before advancing the confirmation offset.
  Reconnects retain the last dispatched update ID; queued updates are not drained
  or acknowledged ahead of dispatch. HTTP 409 is reported and retried normally,
  **not** treated as permission to discard the queue. Check for another running
  bot instance or an existing webhook when conflicts persist.

The existing process-start replay policy still applies: message timestamps older
than startup (with the existing two-second grace period) are ignored. This change
preserves queued messages during **in-process reconnects**; it does not introduce
persistent update offsets or replay old messages across process restarts.

Receive recovery regressions run entirely against a loopback HTTP/2 Bot API:

```bash
go test ./platform/telegram -run TestTelegramRecovery_ -count=1
go test -race ./platform/telegram
```

They cover header/body timeouts, readiness, independent connection pools, queued
messages and offsets across reconnects, cancellation, duplicate starts, API
errors, rate limiting, proxy settings, and token-safe error reporting. They do
not create a second real Telegram consumer or prove human network delivery.

## Step 6: Start Chatting

### 6.1 Direct Message

1. Search for your bot's username in Telegram
2. Click "Start" to begin
3. Send a message

### 6.2 Group Chat

1. Create or open a group
2. Go to group settings → Add members
3. Search and add your bot
4. Send messages in the group

### 6.3 Topic Sessions

Telegram topics include a `message_thread_id`. cc-connect uses that thread ID
as part of the Telegram session key, so each topic has its own independent
conversation context. This applies to forum topics in groups and private chat
topics when Telegram includes `message_thread_id`.

---

## Usage Example

```
User: Help me analyze the current project structure

cc-connect: 🤔 Thinking...
cc-connect: 🔧 Tool: Bash(ls -la)
cc-connect: Here's the project structure...
```

---

## Starting and resuming conversations

`/new` starts a separate empty conversation; it does not erase the previous
conversation's UUID or history. Use `/list` or `/switch` without arguments to
open the session picker, then tap the desired row to resume it. You can also
send `/switch <number | id_prefix | name>` directly.

Picker callbacks use a compact identity derived from the native session ID,
not the displayed row number. They remain bound to that conversation if the
backend list is reordered, and stay within Telegram's 64-byte callback limit
for long opaque IDs. A removed or inaccessible target is not replaced by a
different conversation. Opening a picker does not prompt the agent or change
the active conversation. Native ACP loads the selected UUID on the next turn
or model-control request, preserving its saved model, effort and history.

## Streaming cards

Enable the Feishu-style rich-card layout in the global display settings:

```toml
[display]
mode = "quiet"
card_mode = "rich"
thinking_messages = true
tool_messages = true
reply_footer = true
```

Each turn has one expandable **Reasoning** blockquote and one expandable
**Tools** blockquote when those channels contain events. The answer streams
below both panels; completion adds the status footer. When the agent reports
an explicit current-context meter, the footer includes exact used/window token
counts, for example `ctx 15.6% (40960/262144)`, plus any reported turn input/output
and cache counters. ACP turn totals are not context occupancy; missing meters
are not guessed. `reply_footer` and `show_context_indicator` control visibility,
and metrics remain hidden during streaming. See [ACP context usage](acp.md#context-usage-in-reply-footers).
Telegram clients
supporting expandable blockquotes can unfold each panel independently.

ACP `agent_thought_chunk` notifications are mapped to the reasoning channel,
not to answer text. `agent_message_chunk` notifications remain answer text.
This follows protocol labels rather than guessing from a model's wording.
Continuous ACP thought deltas update a single reasoning block, preserving
word spacing, punctuation, paragraphs, and newlines. The reasoning panel uses
one heading icon, not a thinking emoji before each line. Tool calls remain
individually counted and status-marked. `thinking_max_len` caps the accumulated
reasoning preview rather than independently treating each token as a step.

### Long answers and final delivery

Replies are split before sending, using the decoded text's UTF-16 length and
Telegram's 4096-unit limit. HTML entities, Unicode emoji, nested formatting,
code blocks and expandable folds remain valid in every fragment. Reply targets,
forum topics and first-fragment buttons are preserved. HTML rejection retries
only that explicitly rejected fragment as decoded plain text; transport failures
do not replay an uncertain or partially accepted long response. Explicit 429
rejections honor `retry_after` with cancellable, bounded retries of only the
rejected fragment; accepted fragments are not sent again.

Completion is synchronous, unlike coalesced progress edits. It cancels queued
preview frames, waits for the in-flight edit, updates the original message with
the first final fragment, then sends continuations in order. A late progress
frame cannot overwrite the final answer. Delivery failure is reported rather
than being mistaken for successful timer scheduling; rich-card failures show a
short `/history` recovery notice when the platform is reachable. The generated
answer is persisted before delivery and is not regenerated to retry a send.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Telegram Cloud                          │
│                                                              │
│   User Message ──→ Telegram Bot API ◄── Long Polling         │
│                          ▲                                   │
└──────────────────────────┼───────────────────────────────────┘
                           │
                           │ HTTPS (no public IP needed)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                    Your Local Machine                         │
│                                                              │
│   cc-connect ◄──► Claude Code CLI ◄──► Your Project Code    │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

---

## Long Polling vs Webhook

| Feature | Long Polling | Webhook |
|---------|-------------|---------|
| Public IP | ❌ Not needed | ✅ Required |
| Domain | ❌ Not needed | ✅ Required |
| HTTPS cert | ❌ Not needed | ✅ Required |
| Complexity | Simple | More complex |
| Latency | Low (long poll) | Low |
| Best for | Local dev, private network | Production |

---

## FAQ

### Q: Bot doesn't respond to messages?

Check the following:
1. Is cc-connect running?
2. Is the bot token correct?
3. Have you sent a message after starting cc-connect? (The bot only receives messages after startup)

### Q: How to regenerate the token?

1. Send `/mybots` to BotFather
2. Select your bot
3. Click `API Token` → `Revoke current token`

### Q: Bot doesn't respond in groups?

Make sure Group Privacy mode is disabled. In BotFather: `/mybots` → select bot → `Bot Settings` → `Group Privacy` → `Turn off`.

---

## References

- [Telegram Bot API Documentation](https://core.telegram.org/bots/api)
- [BotFather Guide](https://core.telegram.org/bots#botfather)
- [Telegram Bot Tutorial](https://core.telegram.org/bots/tutorial)

---

## See Also

- [Feishu Setup](./feishu.md)
- [DingTalk Setup](./dingtalk.md)
- [Weibo Setup](./weibo.md)
- [Slack Setup](./slack.md)
- [Discord Setup](./discord.md)
- [Back to README](../README.md)
