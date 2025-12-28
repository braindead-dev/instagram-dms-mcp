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

# Gateway configuration
GATEWAY_PORT = 29391
GATEWAY_URL = f"http://127.0.0.1:{GATEWAY_PORT}"

# Global gateway process
_gateway_process: Optional[subprocess.Popen] = None
_cookies_tempfile: Optional[str] = None

mcp = FastMCP("instagram-dms-mcp")


def get_cookies_json() -> Optional[str]:
    """Get cookies JSON from environment variables."""
    # Try individual env vars first (preferred method)
    session_id = os.getenv("IG_SESSION_ID", "")
    user_id = os.getenv("IG_USER_ID", "")
    csrf_token = os.getenv("IG_CSRF_TOKEN", "")
    
    if session_id and user_id and csrf_token:
        cookies = {
            "sessionid": session_id,
            "ds_user_id": user_id,
            "csrftoken": csrf_token,
        }
        # Add optional cookies if present
        if os.getenv("IG_DATR"):
            cookies["datr"] = os.getenv("IG_DATR")
        if os.getenv("IG_DID"):
            cookies["ig_did"] = os.getenv("IG_DID")
        if os.getenv("IG_MID"):
            cookies["mid"] = os.getenv("IG_MID")
        return json.dumps(cookies)
    
    # Fallback: try IG_COOKIES as JSON or base64
    cookies_raw = os.getenv("IG_COOKIES", "")
    if cookies_raw:
        # Try to decode as base64 first
        try:
            decoded = base64.b64decode(cookies_raw).decode("utf-8")
            json.loads(decoded)  # Verify it's valid JSON
            return decoded
        except Exception:
            pass
        
        # Try as raw JSON
        try:
            json.loads(cookies_raw)
            return cookies_raw
        except Exception:
            pass
    
    return None


def find_gateway_binary() -> Optional[Path]:
    """Find the gateway binary relative to this script."""
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
    global _gateway_process, _cookies_tempfile
    
    # Check if already running
    try:
        resp = httpx.get(f"{GATEWAY_URL}/health", timeout=2)
        if resp.status_code == 200:
            print("Gateway already running")
            return True
    except Exception:
        pass
    
    # Get cookies
    cookies_json = get_cookies_json()
    if not cookies_json:
        print("ERROR: Instagram cookies not set")
        print("Set these environment variables in your .env file:")
        print("  IG_SESSION_ID=...")
        print("  IG_USER_ID=...")
        print("  IG_CSRF_TOKEN=...")
        print("\nSee env.example for the full list")
        return False
    
    # Write cookies to temp file
    fd, _cookies_tempfile = tempfile.mkstemp(suffix=".json", prefix="ig_cookies_")
    with os.fdopen(fd, "w") as f:
        f.write(cookies_json)
    
    # Find gateway binary
    gateway_bin = find_gateway_binary()
    if not gateway_bin:
        print("ERROR: Gateway binary not found. Run 'cd gateway && ./build.sh' first")
        return False
    
    # Start gateway
    print(f"Starting Instagram gateway...")
    env = os.environ.copy()
    env["IG_COOKIES_FILE"] = _cookies_tempfile
    
    _gateway_process = subprocess.Popen(
        [str(gateway_bin)],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    
    # Wait for gateway to be ready
    for i in range(30):
        try:
            resp = httpx.get(f"{GATEWAY_URL}/health", timeout=2)
            if resp.status_code == 200:
                data = resp.json()
                print(f"Gateway ready - logged in as @{data.get('username', 'unknown')}")
                return True
        except Exception:
            pass
        
        # Check if process died
        if _gateway_process.poll() is not None:
            stdout, _ = _gateway_process.communicate()
            print(f"Gateway failed to start:\n{stdout.decode()}")
            return False
        
        time.sleep(1)
    
    print("Gateway timed out waiting for ready")
    return False


def stop_gateway():
    """Stop the gateway subprocess."""
    global _gateway_process, _cookies_tempfile
    
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


# Register cleanup
atexit.register(stop_gateway)


async def gateway_get(path: str, params: dict | None = None, timeout: float = 30.0) -> dict:
    """Make a GET request to the Instagram gateway."""
    url = f"{GATEWAY_URL}{path}"
    async with httpx.AsyncClient() as client:
        try:
            resp = await client.get(url, params=params, timeout=timeout)
            if resp.status_code >= 400:
                return {
                    "ok": False,
                    "status": resp.status_code,
                    "error": resp.text,
                }
            return {"ok": True, "data": resp.json()}
        except httpx.TimeoutException:
            return {"ok": False, "error": "Gateway request timed out"}
        except httpx.ConnectError:
            return {"ok": False, "error": f"Could not connect to gateway at {GATEWAY_URL}"}
        except Exception as e:
            return {"ok": False, "error": str(e)}


async def gateway_post(path: str, json_data: dict, timeout: float = 30.0) -> dict:
    """Make a POST request to the Instagram gateway."""
    url = f"{GATEWAY_URL}{path}"
    async with httpx.AsyncClient() as client:
        try:
            resp = await client.post(url, json=json_data, timeout=timeout)
            if resp.status_code >= 400:
                return {
                    "ok": False,
                    "status": resp.status_code,
                    "error": resp.text,
                }
            # Handle 204 No Content
            if resp.status_code == 204:
                return {"ok": True, "data": None}
            try:
                return {"ok": True, "data": resp.json()}
            except Exception:
                return {"ok": True, "data": None}
        except httpx.TimeoutException:
            return {"ok": False, "error": "Gateway request timed out"}
        except httpx.ConnectError:
            return {"ok": False, "error": f"Could not connect to gateway at {GATEWAY_URL}"}
        except Exception as e:
            return {"ok": False, "error": str(e)}


def format_timestamp(ts_ms: int) -> str:
    """Convert millisecond timestamp to ISO format string."""
    if not ts_ms:
        return ""
    try:
        dt = datetime.fromtimestamp(ts_ms / 1000, tz=timezone.utc)
        return dt.isoformat()
    except Exception:
        return ""


# =============================================================================
# MCP Tools
# =============================================================================


@mcp.tool(description="Get the current Instagram account info and connection status")
async def get_account_info() -> dict:
    """
    Check if the Instagram gateway is connected and get account info.
    
    Returns the logged-in username and user ID.
    """
    result = await gateway_get("/health")
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to connect to gateway")}
    
    data = result.get("data", {})
    return {
        "status": data.get("status", "unknown"),
        "username": data.get("username"),
        "user_id": data.get("user_id"),
    }


@mcp.tool(description="Get your Instagram DM inbox - lists all conversations with recent messages")
async def get_inbox(limit: int = 20) -> dict:
    """
    Get all Instagram DM threads/conversations.
    
    Returns a list of conversations with participant info and last message preview.
    
    Args:
        limit: Maximum number of threads to return (default 20)
    """
    result = await gateway_get("/threads")
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to fetch inbox")}
    
    data = result.get("data", {})
    threads = data.get("threads", [])
    
    # Format the threads for easier reading
    formatted = []
    for thread in threads[:limit]:
        formatted.append({
            "thread_id": thread.get("thread_id"),
            "participant": {
                "username": thread.get("participant_username"),
                "name": thread.get("participant_name"),
            },
            "last_message": thread.get("last_message_preview"),
            "last_message_time": format_timestamp(thread.get("last_message_time", 0)),
            "message_count": thread.get("message_count", 0),
        })
    
    return {
        "count": len(formatted),
        "conversations": formatted,
    }


@mcp.tool(description="Get messages from a specific Instagram DM thread")
async def get_messages(thread_id: str, limit: int = 30) -> dict:
    """
    Get message history from a specific DM thread.
    
    Args:
        thread_id: The thread ID to fetch messages from
        limit: Maximum number of messages to return (default 30, max 100)
    """
    if not thread_id:
        return {"error": "thread_id is required"}
    
    result = await gateway_get("/history", params={"thread_id": thread_id, "limit": str(min(limit, 100))})
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to fetch messages")}
    
    data = result.get("data", {})
    messages = data.get("messages", [])
    
    # Format messages
    formatted = []
    for msg in messages:
        formatted.append({
            "message_id": msg.get("message_id"),
            "sender_id": msg.get("sender_id"),
            "text": msg.get("text"),
            "timestamp": format_timestamp(msg.get("timestamp_ms", 0)),
            "attachments": [
                {
                    "type": att.get("type"),
                    "url": att.get("url"),
                    "filename": att.get("filename"),
                }
                for att in (msg.get("attachments") or [])
            ],
        })
    
    return {
        "thread_id": thread_id,
        "count": len(formatted),
        "has_more": data.get("has_more", False),
        "messages": formatted,
    }


@mcp.tool(description="Send a text message to an Instagram DM thread")
async def send_message(thread_id: str, text: str, reply_to: Optional[str] = None) -> dict:
    """
    Send a text message to a DM thread.
    
    Args:
        thread_id: The thread ID to send the message to
        text: The message text to send
        reply_to: Optional message ID to reply to
    """
    if not thread_id:
        return {"error": "thread_id is required"}
    if not text:
        return {"error": "text is required"}
    
    payload = {"thread_id": thread_id, "text": text}
    if reply_to:
        payload["reply_to"] = reply_to
    
    result = await gateway_post("/send", payload)
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to send message")}
    
    return {"success": True, "message": "Message sent successfully"}


@mcp.tool(description="React to a message with an emoji")
async def react_to_message(thread_id: str, message_id: str, emoji: str) -> dict:
    """
    Add a reaction to a message.
    
    Args:
        thread_id: The thread containing the message
        message_id: The message ID to react to
        emoji: The emoji to react with (e.g., "❤️", "😂", "👍")
    """
    if not thread_id:
        return {"error": "thread_id is required"}
    if not message_id:
        return {"error": "message_id is required"}
    if not emoji:
        return {"error": "emoji is required"}
    
    result = await gateway_post("/react", {
        "thread_id": thread_id,
        "message_id": message_id,
        "emoji": emoji,
    })
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to add reaction")}
    
    return {"success": True, "message": f"Reacted with {emoji}"}


@mcp.tool(description="Search for an Instagram user by username")
async def search_user(username: str) -> dict:
    """
    Look up an Instagram user by their username.
    
    Returns the user's ID which can be used to start a DM.
    
    Args:
        username: The Instagram username to search for (without @)
    """
    if not username:
        return {"error": "username is required"}
    
    # Strip @ if present
    username = username.lstrip("@")
    
    result = await gateway_get("/lookup_user", params={"username": username})
    if not result.get("ok"):
        return {"error": result.get("error", f"User @{username} not found")}
    
    data = result.get("data", {})
    return {
        "username": data.get("username"),
        "user_id": data.get("user_id"),
        "thread_id": data.get("thread_id"),  # Same as user_id for 1:1 DMs
    }


@mcp.tool(description="Send a DM to a user by their username (starts new conversation if needed)")
async def send_dm(username: str, text: str) -> dict:
    """
    Send a direct message to a user by their username.
    
    This will create a new conversation thread if one doesn't exist.
    
    Args:
        username: The Instagram username to message (without @)
        text: The message text to send
    """
    if not username:
        return {"error": "username is required"}
    if not text:
        return {"error": "text is required"}
    
    # Strip @ if present
    username = username.lstrip("@")
    
    result = await gateway_post("/dm_username", {
        "username": username,
        "text": text,
    }, timeout=60.0)  # Longer timeout since it needs to search first
    
    if not result.get("ok"):
        return {"error": result.get("error", f"Failed to send DM to @{username}")}
    
    data = result.get("data", {})
    return {
        "success": True,
        "message": f"Message sent to @{username}",
        "thread_id": data.get("thread_id"),
        "user_id": data.get("user_id"),
    }


@mcp.tool(description="Mark a conversation as read/seen")
async def mark_as_read(thread_id: str) -> dict:
    """
    Mark a DM thread as read.
    
    Args:
        thread_id: The thread ID to mark as read
    """
    if not thread_id:
        return {"error": "thread_id is required"}
    
    result = await gateway_post("/seen", {"thread_id": thread_id})
    if not result.get("ok"):
        return {"error": result.get("error", "Failed to mark as read")}
    
    return {"success": True, "message": "Marked as read"}


@mcp.tool(description="Get user info by their user ID")
async def get_user_info(user_id: str) -> dict:
    """
    Get information about a user by their ID.
    
    Args:
        user_id: The Instagram user ID
    """
    if not user_id:
        return {"error": "user_id is required"}
    
    result = await gateway_get("/user", params={"id": user_id})
    if not result.get("ok"):
        return {"error": result.get("error", "User not found")}
    
    data = result.get("data", {})
    return {
        "user_id": data.get("id"),
        "username": data.get("username"),
        "name": data.get("name"),
        "profile_pic_url": data.get("profile_pic_url"),
    }


# =============================================================================
# Main Entry Point
# =============================================================================

if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8000))
    host = "0.0.0.0"
    
    print("=" * 50)
    print("Instagram DMs MCP Server")
    print("=" * 50)
    
    # Start the gateway
    if not start_gateway():
        print("\nFailed to start gateway. Exiting.")
        sys.exit(1)
    
    print(f"\nStarting MCP server on {host}:{port}")
    print(f"MCP endpoint: http://{host}:{port}/mcp")
    print("=" * 50)
    
    # Handle signals for clean shutdown
    def handle_signal(signum, frame):
        print("\nShutting down...")
        stop_gateway()
        sys.exit(0)
    
    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)
    
    mcp.run(
        transport="http",
        host=host,
        port=port,
        stateless_http=True,
    )
