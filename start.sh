#!/bin/bash
# Start both the Instagram gateway and the MCP server

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}Instagram DMs MCP - Starting...${NC}"
echo ""

# Check if cookies file exists
COOKIES_FILE="${IG_COOKIES_FILE:-$HOME/.instagram-dms-mcp/cookies.json}"
if [[ ! -f "$COOKIES_FILE" ]]; then
    echo -e "${RED}Error: Cookies file not found at $COOKIES_FILE${NC}"
    echo ""
    echo "Please create your cookies file first. See README.md for instructions."
    echo ""
    echo "Quick setup:"
    echo "  1. Go to instagram.com and log in"
    echo "  2. Open browser console (F12)"
    echo "  3. Run the cookie extraction script from README.md"
    echo "  4. Save the output to $COOKIES_FILE"
    exit 1
fi

# Check if gateway binary exists
if [[ ! -f "$SCRIPT_DIR/gateway/ig-gateway" ]]; then
    echo -e "${YELLOW}Gateway not built. Building now...${NC}"
    cd "$SCRIPT_DIR/gateway"
    ./build.sh
    cd "$SCRIPT_DIR"
fi

# Function to cleanup on exit
cleanup() {
    echo ""
    echo -e "${YELLOW}Shutting down...${NC}"
    kill $GATEWAY_PID 2>/dev/null || true
    kill $MCP_PID 2>/dev/null || true
    exit 0
}
trap cleanup SIGINT SIGTERM

# Start the gateway
echo -e "${GREEN}Starting Instagram gateway...${NC}"
export IG_COOKIES_FILE="$COOKIES_FILE"
"$SCRIPT_DIR/gateway/ig-gateway" &
GATEWAY_PID=$!

# Wait for gateway to be ready
echo "Waiting for gateway to start..."
for i in {1..30}; do
    if curl -s http://127.0.0.1:29391/health > /dev/null 2>&1; then
        echo -e "${GREEN}Gateway is ready!${NC}"
        break
    fi
    if ! kill -0 $GATEWAY_PID 2>/dev/null; then
        echo -e "${RED}Gateway failed to start. Check the logs above.${NC}"
        exit 1
    fi
    sleep 1
done

# Start the MCP server
echo -e "${GREEN}Starting MCP server...${NC}"
cd "$SCRIPT_DIR"
python3 -m venv venv 2>/dev/null || true
source venv/bin/activate 2>/dev/null || true
pip install -q -r requirements.txt

python src/server.py &
MCP_PID=$!

echo ""
echo -e "${GREEN}===========================================${NC}"
echo -e "${GREEN}Instagram DMs MCP is running!${NC}"
echo -e "${GREEN}===========================================${NC}"
echo ""
echo "Gateway:     http://127.0.0.1:29391"
echo "MCP Server:  http://127.0.0.1:8000/mcp"
echo ""
echo "Press Ctrl+C to stop"
echo ""

# Wait for either process to exit
wait $GATEWAY_PID $MCP_PID
