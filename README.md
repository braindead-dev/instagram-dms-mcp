# Instagram DMs MCP

A Model Context Protocol (MCP) server that lets AI assistants read and send Instagram DMs.

## Quick Start

### 1. Get Your Instagram Cookies

1. Go to [instagram.com](https://www.instagram.com) and log in
2. Open DevTools (F12) → **Application** tab → **Cookies** → `https://www.instagram.com`
3. Copy these values into your `.env`:

| Cookie | Env Variable |
|--------|--------------|
| `sessionid` | `IG_SESSION_ID` |
| `ds_user_id` | `IG_USER_ID` |
| `csrftoken` | `IG_CSRF_TOKEN` |
| `datr` | `IG_DATR` |
| `ig_did` | `IG_DID` |
| `mid` | `IG_MID` |

### 2. Setup

```bash
git clone https://github.com/braindead-dev/instagram-dms-mcp.git
cd instagram-dms-mcp

# Build the gateway (requires Go 1.22+)
cd gateway && ./build.sh && cd ..

# Configure
cp env.example .env
# Edit .env with your cookies

# Run
pip install -r requirements.txt
python src/server.py
```

## Tools

| Tool | Description |
|------|-------------|
| `get_inbox()` | See all your conversations |
| `get_conversation(user)` | Read messages with someone |
| `send_message(user, message)` | Send a message |
| `react(user, emoji)` | React to their last message |

### Example Flow

```
User: "Check my Instagram DMs"
→ get_inbox()

User: "What did @johndoe say?"
→ get_conversation("johndoe")

User: "Reply with 'sounds good!'"
→ send_message("johndoe", "sounds good!")

User: "React with a heart"
→ react("johndoe", "❤️")
```

## Incoming DM Notifications (Poke)

To get notified when you receive new DMs:

1. Get your API key from [poke.com/settings/advanced](https://poke.com/settings/advanced)
2. Add to `.env`:
   ```
   POKE_API_KEY=your_key_here
   ```

New DMs will be forwarded to Poke as: `📩 Instagram DM from @username: message`

## Behavior Settings

The MCP simulates natural behavior by default:

| Setting | Default | Description |
|---------|---------|-------------|
| `IG_SIMULATE_TYPING` | `true` | Show typing indicator before sending |
| `IG_AUTO_MARK_SEEN` | `true` | Mark as read when opening conversation |
| `IG_TYPING_DELAY` | `1.5` | Seconds to "type" before sending |
