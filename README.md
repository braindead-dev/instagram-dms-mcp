# Instagram DMs MCP

A Model Context Protocol (MCP) server that lets AI assistants read and send Instagram DMs.

## Features

- 📥 **Get Inbox** - List all DM conversations with previews
- 💬 **Read Messages** - Get full message history from any thread
- ✉️ **Send Messages** - Send text messages to existing threads
- 🆕 **Start Conversations** - DM any user by username
- ❤️ **React to Messages** - Add emoji reactions
- 👤 **User Lookup** - Search for users by username

## Quick Start

### 1. Get Your Instagram Cookies

Go to [instagram.com](https://www.instagram.com) and log in, then open the browser console (F12 or Cmd+Option+I) and run:

```javascript
// Copy this entire script and paste it into the browser console
(function() {
    const cookies = {};
    document.cookie.split(';').forEach(c => {
        const [key, val] = c.trim().split('=');
        if (key && val) cookies[key] = decodeURIComponent(val);
    });
    
    // These are the essential cookies needed
    const needed = ['sessionid', 'ds_user_id', 'csrftoken', 'ig_did', 'mid', 'datr', 'ig_nrcb'];
    const result = {};
    
    needed.forEach(k => {
        if (cookies[k]) result[k] = cookies[k];
    });
    
    // Check if we have the essential ones
    if (!result.sessionid || !result.ds_user_id) {
        console.error('❌ Missing essential cookies. Make sure you are logged in!');
        return;
    }
    
    const json = JSON.stringify(result, null, 2);
    console.log('✅ Copy this JSON to ~/.instagram-dms-mcp/cookies.json:\n\n' + json);
    
    // Also copy to clipboard if possible
    try {
        navigator.clipboard.writeText(json);
        console.log('\n📋 (Already copied to clipboard!)');
    } catch(e) {}
})();
```

### 2. Save Your Cookies

Create the config directory and save your cookies:

```bash
mkdir -p ~/.instagram-dms-mcp
# Paste the JSON from step 1 into this file:
nano ~/.instagram-dms-mcp/cookies.json
```

Your `cookies.json` should look like:

```json
{
  "sessionid": "your_session_id_here",
  "ds_user_id": "your_user_id_here",
  "csrftoken": "your_csrf_token",
  "ig_did": "...",
  "mid": "...",
  "datr": "...",
  "ig_nrcb": "..."
}
```

### 3. Build & Run

```bash
# Clone the repository
git clone https://github.com/anthropics/instagram-dms-mcp.git
cd instagram-dms-mcp

# Build the gateway (requires Go 1.22+)
cd gateway
./build.sh
cd ..

# Start everything
./start.sh
```

The MCP server will be available at `http://localhost:8000/mcp`

## Connecting to Poke

1. Go to [poke.com/settings/connections](https://poke.com/settings/connections)
2. Add a new MCP integration
3. Enter your MCP server URL: `http://your-server:8000/mcp`

## Available Tools

| Tool | Description |
|------|-------------|
| `get_account_info` | Check connection status and get your username |
| `get_inbox` | List all DM conversations |
| `get_messages` | Get messages from a specific thread |
| `send_message` | Send a message to a thread |
| `send_dm` | Send a DM to a user by username |
| `react_to_message` | React to a message with an emoji |
| `search_user` | Look up a user by username |
| `get_user_info` | Get info about a user by ID |
| `mark_as_read` | Mark a conversation as read |

## Example Usage

Once connected to Poke, you can ask things like:

- "Check my Instagram DMs"
- "What are my recent Instagram messages?"
- "Send a message to @username saying hello"
- "React to that last message with ❤️"

## Architecture

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Poke / AI     │────▶│   MCP Server    │────▶│  IG Gateway     │
│   Assistant     │     │   (Python)      │     │   (Go)          │
└─────────────────┘     └─────────────────┘     └─────────────────┘
                              :8000                  :29391
                                                        │
                                                        ▼
                                               ┌─────────────────┐
                                               │   Instagram     │
                                               │   Web API       │
                                               └─────────────────┘
```

- **MCP Server** (Python/FastMCP): Handles the MCP protocol and exposes tools
- **IG Gateway** (Go/messagix): Handles Instagram's web API using session cookies

## Development

### Running Components Separately

**Gateway only:**
```bash
cd gateway
./ig-gateway --cookies ~/.instagram-dms-mcp/cookies.json
```

**MCP Server only:**
```bash
pip install -r requirements.txt
python src/server.py
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `IG_GATEWAY_ADDR` | `http://127.0.0.1:29391` | Gateway address for MCP server |
| `IG_COOKIES_FILE` | `~/.instagram-dms-mcp/cookies.json` | Path to cookies file |
| `PORT` | `8000` | MCP server port |

## Deployment

### Using Render

1. Fork this repository
2. Create a new web service on [Render](https://render.com)
3. Connect your forked repository
4. Set the environment variables:
   - `IG_COOKIES_FILE`: Path to your cookies (you'll need to handle this securely)
5. Deploy

Note: For production deployments, you'll need to run the Go gateway separately or include it in your build process.

## Troubleshooting

### "Could not connect to gateway"
- Make sure the gateway is running: `./gateway/ig-gateway`
- Check that port 29391 is available

### "No cookies file found"
- Create `~/.instagram-dms-mcp/cookies.json` with your Instagram cookies
- Or set the `IG_COOKIES_FILE` environment variable

### "Gateway failed to start"
- Check if your cookies are valid (try logging into Instagram web again)
- Instagram session cookies expire periodically - you may need to refresh them

### Session expired
If your session expires, simply:
1. Log into Instagram web again
2. Run the cookie extraction script
3. Update your `cookies.json`
4. Restart the gateway

## Security Notes

⚠️ **Important**: Your `cookies.json` contains your Instagram session. Keep it secure:

- Never commit it to version control
- Don't share it with anyone
- Consider the security implications of self-hosting

## Credits

This project uses:
- [FastMCP](https://github.com/jlowin/fastmcp) - Python MCP framework
- [messagix](https://github.com/mautrix/meta) - Instagram web client library (from mautrix-meta)

## License

MIT
