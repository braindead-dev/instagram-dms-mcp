# Instagram DMs MCP

A Model Context Protocol (MCP) server that lets AI assistants read and send Instagram DMs.

## Quick Start

### 1. Get Your Instagram Cookies

1. Go to [instagram.com](https://www.instagram.com) and log in
2. Open DevTools (F12) → **Application** tab → **Cookies** → `https://www.instagram.com`
3. Copy these cookie values:

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

# Copy and fill in your cookies
cp env.example .env
# Edit .env with your cookie values
```

### 3. Run

```bash
pip install -r requirements.txt
python src/server.py
```

The MCP server will be available at `http://localhost:8000/mcp`

## Connecting to Poke

1. Go to [poke.com/settings/connections](https://poke.com/settings/connections)
2. Add a new MCP integration  
3. Enter your MCP server URL

## Available Tools

| Tool | Description |
|------|-------------|
| `get_inbox` | List all DM conversations |
| `get_messages` | Get messages from a thread |
| `send_message` | Send a message |
| `send_dm` | DM a user by username |
| `react_to_message` | React with an emoji |