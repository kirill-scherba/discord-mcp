# discord-mcp

[![Perl](https://img.shields.io/badge/perl-5.40+-blue.svg)](https://www.perl.org/)
[![MCP](https://img.shields.io/badge/MCP-2024--11--05-green.svg)](https://modelcontextprotocol.io)
[![License](https://img.shields.io/badge/license-MIT-purple.svg)](LICENSE)

> **Discord MCP server — 3 tools for sending messages and embeds via webhook.**

Zero external dependencies — `perl`, `JSON` (core), `curl`, and a Discord webhook URL.

## Tools

| Tool | Description |
|------|-------------|
| `discord_send` | Send a simple text message |
| `discord_send_embed` | Send a rich embed (title, description, color, fields) |
| `discord_webhook_info` | Validate webhook connectivity and return info |

## Setup

### 1. Create a Discord webhook

Open your Discord server → Channel Settings → Integrations → Webhooks → New Webhook. Copy the URL.

### 2. Add to opencode config

```json
"discord-mcp": {
  "type": "local",
  "command": ["perl", "/path/to/discord-mcp/discord-mcp.pl"],
  "environment": {
    "DISCORD_WEBHOOK_URL": "https://discord.com/api/webhooks/..."
  }
}
```

### 3. Usage

```json
// Simple text
{
  "name": "discord_send",
  "arguments": {
    "message": "Hello from MCP!"
  }
}

// Rich embed
{
  "name": "discord_send_embed",
  "arguments": {
    "title": "Deploy Complete",
    "description": "v2.3.1 deployed to production",
    "color": 5763719,
    "fields": [
      { "name": "Status", "value": "✅ Success", "inline": true },
      { "name": "Duration", "value": "142s", "inline": true }
    ]
  }
}
```

## License

MIT
