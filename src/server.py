#!/usr/bin/env python3
"""
Instagram DMs MCP Server

A Model Context Protocol server that provides Instagram DM capabilities.
Automatically manages the Instagram gateway as a subprocess.
"""

import asyncio
import atexit
import base64
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Optional

import httpx
from dotenv import load_dotenv
from fastmcp import FastMCP

load_dotenv()


# Logging
def log(event_type: str, message: str, data: dict | None = None):
    """Simple event logger for observability."""
    timestamp = datetime.now().strftime("%H:%M:%S")
    
    # Color codes
    colors = {
        "incoming": "\033[94m",  # Blue - incoming DMs
        "outgoing": "\033[92m",  # Green - outgoing messages
        "poke": "\033[95m",      # Magenta - Poke webhook
        "tool": "\033[93m",      # Yellow - MCP tool calls
        "error": "\033[91m",     # Red - errors
    }
    reset = "\033[0m"
    color = colors.get(event_type, "")
    
    prefix = {
        "incoming": "[IN]",
        "outgoing": "[OUT]", 
        "poke": "🌴 POKE",
        "tool": "🔧 MCP",
        "error": "❌ ERR",
    }.get(event_type, "•")
    
    print(f"{color}[{timestamp}] {prefix}: {message}{reset}")
    if data:
        for k, v in data.items():
            print(f"{color}         {k}: {v}{reset}")


# Configuration
GATEWAY_PORT = 29391
GATEWAY_URL = f"http://127.0.0.1:{GATEWAY_PORT}"

# Behavior settings
SIMULATE_TYPING = os.getenv("IG_SIMULATE_TYPING", "true").lower() == "true"
AUTO_MARK_SEEN = os.getenv("IG_AUTO_MARK_SEEN", "true").lower() == "true"
TYPING_DELAY_SECONDS = float(os.getenv("IG_TYPING_DELAY", "1.5"))

# Poke webhook for incoming DM notifications
POKE_API_KEY = os.getenv("POKE_API_KEY", "")

# Global state
_gateway_process: Optional[subprocess.Popen] = None
_cookies_tempfile: Optional[str] = None
_poll_task: Optional[asyncio.Task] = None
_self_user_id: Optional[str] = None
_self_username: Optional[str] = None

# User cache: user_id -> {username, name}
_user_cache: dict[str, dict] = {}

mcp = FastMCP("instagram-dms-mcp")


# Gateway Management
def get_cookies_json() -> Optional[str]:
    """Get cookies JSON from environment variables."""
    session_id = os.getenv("IG_SESSION_ID", "")
    user_id = os.getenv("IG_USER_ID", "")
    csrf_token = os.getenv("IG_CSRF_TOKEN", "")
    
    if session_id and user_id and csrf_token:
        cookies = {
            "sessionid": session_id,
            "ds_user_id": user_id,
            "csrftoken": csrf_token,
        }
        if os.getenv("IG_DATR"):
            cookies["datr"] = os.getenv("IG_DATR")
        if os.getenv("IG_DID"):
            cookies["ig_did"] = os.getenv("IG_DID")
        if os.getenv("IG_MID"):
            cookies["mid"] = os.getenv("IG_MID")
        return json.dumps(cookies)
    
    # Fallback: IG_COOKIES as JSON or base64
    cookies_raw = os.getenv("IG_COOKIES", "")
    if cookies_raw:
        try:
            decoded = base64.b64decode(cookies_raw).decode("utf-8")
            json.loads(decoded)
            return decoded
        except Exception:
            pass
        try:
            json.loads(cookies_raw)
            return cookies_raw
        except Exception:
            pass
    
    return None


def find_gateway_binary() -> Optional[Path]:
    """Find the gateway binary."""
    script_dir = Path(__file__).parent.parent
    candidates = [
        script_dir / "gateway" / "ig-gateway",
        script_dir / "gateway" / "ig-gateway.exe",
        Path("gateway") / "ig-gateway",
    ]
    for candidate in candidates:
        if candidate.exists():
            return candidate
    return None


def start_gateway() -> bool:
    """Start the Instagram gateway as a subprocess."""
    global _gateway_process, _cookies_tempfile, _self_user_id, _self_username
    
    # Check if already running
    try:
        resp = httpx.get(f"{GATEWAY_URL}/health", timeout=2)
        if resp.status_code == 200:
            data = resp.json()
            _self_user_id = data.get("user_id")
            _self_username = data.get("username")
            print(f"Gateway already running - logged in as @{_self_username}")
            return True
    except Exception:
        pass
    
    cookies_json = get_cookies_json()
    if not cookies_json:
        print("ERROR: Instagram cookies not set")
        print("Set these in your .env file:")
        print("  IG_SESSION_ID=...")
        print("  IG_USER_ID=...")
        print("  IG_CSRF_TOKEN=...")
        return False
    
    fd, _cookies_tempfile = tempfile.mkstemp(suffix=".json", prefix="ig_cookies_")
    with os.fdopen(fd, "w") as f:
        f.write(cookies_json)
    
    gateway_bin = find_gateway_binary()
    if not gateway_bin:
        print("ERROR: Gateway not found. Run: cd gateway && ./build.sh")
        return False
    
    print("Starting Instagram gateway...")
    env = os.environ.copy()
    env["IG_COOKIES_FILE"] = _cookies_tempfile
    
    _gateway_process = subprocess.Popen(
        [str(gateway_bin)],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    
    for _ in range(30):
        try:
            resp = httpx.get(f"{GATEWAY_URL}/health", timeout=2)
            if resp.status_code == 200:
                data = resp.json()
                _self_user_id = data.get("user_id")
                _self_username = data.get("username")
                print(f"Gateway ready - logged in as @{_self_username}")
                return True
        except Exception:
            pass
        
        if _gateway_process.poll() is not None:
            stdout, _ = _gateway_process.communicate()
            print(f"Gateway failed:\n{stdout.decode()}")
            return False
        
        time.sleep(1)
    
    print("Gateway timed out")
    return False


def stop_gateway():
    """Stop the gateway subprocess."""
    global _gateway_process, _cookies_tempfile, _poll_task
    
    if _poll_task:
        _poll_task.cancel()
        _poll_task = None
    
    if _gateway_process:
        _gateway_process.terminate()
        try:
            _gateway_process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            _gateway_process.kill()
        _gateway_process = None
    
    if _cookies_tempfile and os.path.exists(_cookies_tempfile):
        os.unlink(_cookies_tempfile)
        _cookies_tempfile = None


atexit.register(stop_gateway)

# Gateway API Helpers
async def gateway_get(path: str, params: dict | None = None) -> dict:
    """GET request to gateway."""
    async with httpx.AsyncClient() as client:
        try:
            resp = await client.get(f"{GATEWAY_URL}{path}", params=params, timeout=30)
            if resp.status_code >= 400:
                return {"ok": False, "error": resp.text}
            return {"ok": True, "data": resp.json()}
        except Exception as e:
            return {"ok": False, "error": str(e)}


async def gateway_post(path: str, data: dict) -> dict:
    """POST request to gateway."""
    async with httpx.AsyncClient() as client:
        try:
            resp = await client.post(f"{GATEWAY_URL}{path}", json=data, timeout=30)
            if resp.status_code >= 400:
                return {"ok": False, "error": resp.text}
            if resp.status_code == 204:
                return {"ok": True}
            try:
                return {"ok": True, "data": resp.json()}
            except Exception:
                return {"ok": True}
        except Exception as e:
            return {"ok": False, "error": str(e)}


# User Resolution
async def get_user_info(user_id: str) -> dict:
    """Get username and name for a user ID, with caching."""
    if user_id in _user_cache:
        return _user_cache[user_id]
    
    result = await gateway_get("/user", {"id": user_id})
    if result.get("ok"):
        info = result.get("data", {})
        _user_cache[user_id] = {
            "username": info.get("username", ""),
            "name": info.get("name", ""),
        }
        return _user_cache[user_id]
    
    return {"username": "", "name": ""}


async def resolve_thread_id(identifier: str) -> Optional[str]:
    """Resolve a username or thread_id to a thread_id."""
    # If it's already a numeric thread ID, return it
    if identifier.isdigit():
        return identifier
    
    # Strip @ if present
    username = identifier.lstrip("@")
    
    # Look up user
    result = await gateway_get("/lookup_user", {"username": username})
    if result.get("ok"):
        return result["data"].get("thread_id")
    
    return None


def format_time_ago(timestamp_ms: int) -> str:
    """Format timestamp as human-readable 'time ago'."""
    if not timestamp_ms:
        return ""
    
    now = datetime.now(timezone.utc)
    then = datetime.fromtimestamp(timestamp_ms / 1000, tz=timezone.utc)
    diff = now - then
    
    if diff.days > 7:
        return then.strftime("%b %d")
    elif diff.days > 0:
        return f"{diff.days}d ago"
    elif diff.seconds >= 3600:
        return f"{diff.seconds // 3600}h ago"
    elif diff.seconds >= 60:
        return f"{diff.seconds // 60}m ago"
    else:
        return "just now"


# Internal Actions (not exposed as MCP tools)
async def _mark_seen(thread_id: str):
    """Mark thread as seen (internal use)."""
    if AUTO_MARK_SEEN:
        await gateway_post("/seen", {"thread_id": thread_id})


async def _send_typing(thread_id: str):
    """Send typing indicator and wait (internal use)."""
    if SIMULATE_TYPING:
        await gateway_post("/typing", {"thread_id": thread_id, "typing": True})
        await asyncio.sleep(TYPING_DELAY_SECONDS)
        await gateway_post("/typing", {"thread_id": thread_id, "typing": False})


async def notify_poke(sender_username: str, sender_name: str, thread_id: str, text: str, attachments: list):
    """Send incoming DM notification to Poke."""
    if not POKE_API_KEY:
        return
    
    # Format sender: @username (Display Name)
    if sender_username and sender_name:
        sender = f"@{sender_username} ({sender_name})"
    elif sender_username:
        sender = f"@{sender_username}"
    elif sender_name:
        sender = sender_name
    else:
        sender = "Someone"
    
    # Handle attachments
    if attachments and not text:
        att_types = []
        for a in attachments:
            att_type = a.get("type", "")
            if att_type in ("1", "2", "image"):
                att_types.append("photo")
            elif att_type in ("3", "4", "video"):
                att_types.append("video")
            elif att_type == "6" or "audio" in str(att_type).lower():
                att_types.append("voice message")
            else:
                att_types.append("attachment")
        text = f"[sent {', '.join(att_types)}]"
    
    message = f"Instagram DM from {sender} [thread:{thread_id}]: {text}"
    
    try:
        async with httpx.AsyncClient() as client:
            await client.post(
                "https://poke.com/api/v1/inbound-sms/webhook",
                headers={
                    "Authorization": f"Bearer {POKE_API_KEY}",
                    "Content-Type": "application/json",
                },
                json={"message": message},
                timeout=10,
            )
        log("poke", f"Forwarded DM to Poke", {"from": sender, "thread": thread_id})
    except Exception as e:
        log("error", f"Failed to notify Poke: {e}")


async def poll_incoming_messages():
    """Background task: poll for new DMs and notify Poke."""
    if not POKE_API_KEY:
        print("POKE_API_KEY not set - incoming DM notifications disabled")
        return
    
    print("Incoming DM notifications enabled - will forward to Poke")
    
    while True:
        try:
            result = await gateway_get("/poll", {"max": "50"})
            if result.get("ok"):
                data = result.get("data") or {}
                events = data.get("events") or []
                for event in events:
                    sender_id = event.get("sender_id", "")
                    thread_id = event.get("thread_id", "")
                    
                    # Skip our own messages
                    if sender_id == _self_user_id:
                        continue
                    
                    # Get sender info
                    user_info = await get_user_info(sender_id)
                    username = user_info.get("username", "")
                    
                    log("incoming", f"New DM from @{username or sender_id}", {
                        "thread": thread_id,
                        "message": event.get("text", "")[:50] + ("..." if len(event.get("text", "")) > 50 else "")
                    })
                    
                    await notify_poke(
                        sender_username=user_info.get("username", ""),
                        sender_name=user_info.get("name", ""),
                        thread_id=thread_id,
                        text=event.get("text", ""),
                        attachments=event.get("attachments", []),
                    )
        except asyncio.CancelledError:
            break
        except Exception as e:
            print(f"Poll error: {e}")
        
        await asyncio.sleep(2)


@mcp.tool(description="Check your Instagram DM inbox - see all conversations and recent messages")
async def get_inbox() -> dict:
    """
    View your Instagram inbox with all conversations.
    Shows who messaged you, the last message preview, and when.
    """
    log("tool", "get_inbox() called")
    result = await gateway_get("/threads")
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to load inbox")}
    
    threads = result.get("data", {}).get("threads", [])
    
    if not threads:
        return {"message": "Your inbox is empty"}
    
    conversations = []
    for thread in threads[:20]:
        username = thread.get("participant_username", "")
        name = thread.get("participant_name", "")
        
        # Cache user info
        thread_id = thread.get("thread_id", "")
        if thread_id and username:
            _user_cache[thread_id] = {"username": username, "name": name}
        
        display_name = f"@{username}" if username else name or "Unknown"
        if name and username:
            display_name = f"@{username} ({name})"
        
        conversations.append({
            "thread_id": thread_id,
            "user": display_name,
            "last_message": thread.get("last_message_preview", ""),
            "time": format_time_ago(thread.get("last_message_time", 0)),
        })
    
    return {
        "inbox_count": len(conversations),
        "conversations": conversations,
    }


@mcp.tool(description="Open a conversation to see message history")
async def get_conversation(user: str, limit: int = 20) -> dict:
    """
    View messages in a conversation.
    
    Args:
        user: Username (like "johndoe" or "@johndoe") or thread_id
        limit: Number of recent messages to show (default 20)
    """
    log("tool", f"get_conversation({user})")
    thread_id = await resolve_thread_id(user)
    if not thread_id:
        return {"error": f"Could not find conversation with '{user}'"}
    
    # Mark as seen when opening conversation
    await _mark_seen(thread_id)
    
    result = await gateway_get("/history", {"thread_id": thread_id, "limit": str(limit)})
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to load conversation")}
    
    data = result.get("data", {})
    raw_messages = data.get("messages", [])
    
    messages = []
    for msg in raw_messages:
        sender_id = msg.get("sender_id", "")
        
        # Determine who sent it
        if sender_id == _self_user_id:
            sender = "You"
        else:
            user_info = await get_user_info(sender_id)
            username = user_info.get("username", "")
            sender = f"@{username}" if username else user_info.get("name", "Them")
        
        text = msg.get("text", "")
        attachments = msg.get("attachments", [])
        
        # Describe attachments
        if attachments and not text:
            att_descs = []
            for att in attachments:
                att_type = att.get("type", "")
                if att_type in ("1", "2", "image"):
                    att_descs.append("photo")
                elif att_type in ("3", "4", "video"):
                    att_descs.append("video")
                elif att_type == "6" or "audio" in str(att_type).lower():
                    att_descs.append("voice message")
                else:
                    att_descs.append("attachment")
            text = f"[{', '.join(att_descs)}]"
        elif attachments:
            text += " [+attachments]"
        
        messages.append({
            "from": sender,
            "message": text,
            "time": format_time_ago(msg.get("timestamp_ms", 0)),
            "message_id": msg.get("message_id", ""),
        })
    
    # Get conversation partner name
    partner_info = _user_cache.get(thread_id, {})
    partner = f"@{partner_info.get('username', '')}" if partner_info.get("username") else user
    
    return {
        "conversation_with": partner,
        "thread_id": thread_id,
        "message_count": len(messages),
        "messages": messages,
        "has_more": data.get("has_more", False),
    }


@mcp.tool(description="Send a message in a conversation")
async def send_message(user: str, message: str) -> dict:
    """
    Send a message to someone on Instagram.
    
    Args:
        user: Username (like "johndoe" or "@johndoe") or thread_id
        message: The message to send
    """
    log("tool", f"send_message({user})", {"message": message[:50] + ("..." if len(message) > 50 else "")})
    
    if not message:
        return {"error": "Message cannot be empty"}
    
    thread_id = await resolve_thread_id(user)
    if not thread_id:
        # Try to start new conversation via dm_username
        username = user.lstrip("@")
        result = await gateway_post("/dm_username", {"username": username, "text": message})
        if result.get("ok"):
            log("outgoing", f"Sent DM to @{username} (new conversation)")
            return {"sent": True, "to": f"@{username}", "message": message}
        return {"error": f"Could not find or message '{user}'"}
    
    # Simulate natural behavior: seen -> typing -> send
    await _mark_seen(thread_id)
    await _send_typing(thread_id)
    
    result = await gateway_post("/send", {"thread_id": thread_id, "text": message})
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to send message")}
    
    # Get recipient name
    partner_info = _user_cache.get(thread_id, {})
    recipient = f"@{partner_info.get('username', '')}" if partner_info.get("username") else user
    
    log("outgoing", f"Sent message to {recipient}", {"thread": thread_id})
    return {"sent": True, "to": recipient, "message": message}


@mcp.tool(description="React to a message with an emoji")
async def react(user: str, emoji: str, message_id: Optional[str] = None) -> dict:
    """
    React to a message with an emoji.
    
    Args:
        user: Username or thread_id of the conversation
        emoji: The emoji to react with (like "❤️" or "😂")
        message_id: Specific message ID to react to (if not provided, reacts to the last message from them)
    """
    log("tool", f"react({user}, {emoji})")
    thread_id = await resolve_thread_id(user)
    if not thread_id:
        return {"error": f"Could not find conversation with '{user}'"}
    
    # If no message_id provided, get the last message from the other person
    if not message_id:
        result = await gateway_get("/history", {"thread_id": thread_id, "limit": "10"})
        if not result.get("ok"):
            return {"error": "Could not load conversation to find message"}
        
        messages = result.get("data", {}).get("messages", [])
        # Find last message NOT from us
        for msg in reversed(messages):
            if msg.get("sender_id") != _self_user_id:
                message_id = msg.get("message_id")
                break
        
        if not message_id:
            return {"error": "No message found to react to"}
    
    result = await gateway_post("/react", {
        "thread_id": thread_id,
        "message_id": message_id,
        "emoji": emoji,
    })
    
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to react")}
    
    log("outgoing", f"Reacted with {emoji} to message in {user}")
    return {"reacted": True, "emoji": emoji}


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8000))
    host = "0.0.0.0"
    
    print("=" * 50)
    print("Instagram DMs MCP Server")
    print("=" * 50)
    
    if not start_gateway():
        print("\nFailed to start. Check your cookies.")
        sys.exit(1)
    
    print(f"\nMCP server: http://{host}:{port}/mcp")
    print("=" * 50)
    
    def handle_signal(signum, frame):
        print("\nShutting down...")
        stop_gateway()
        sys.exit(0)
    
    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)
    
    # Start the Poke notification polling in background
    async def start_with_polling():
        global _poll_task
        loop = asyncio.get_event_loop()
        _poll_task = loop.create_task(poll_incoming_messages())
    
    # Note: FastMCP will run its own event loop, so we start polling there
    import threading
    def run_polling():
        asyncio.run(poll_incoming_messages())
    
    if POKE_API_KEY:
        polling_thread = threading.Thread(target=run_polling, daemon=True)
        polling_thread.start()
    
    mcp.run(
        transport="http",
        host=host,
        port=port,
        stateless_http=True,
    )
