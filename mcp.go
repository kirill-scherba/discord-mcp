// mcp.go — MCP tool registry (mark3labs/mcp-go).
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// tools returns all MCP tools exposed by discord-mcp.
func tools() []server.ServerTool {
	return []server.ServerTool{
		{Tool: mcp.NewTool("discord_send",
			mcp.WithDescription("Send a simple text message to Discord via webhook. IMPORTANT: always pass agentname — your OWN agent name, which you can find in your system prompt (your identity, who you are). So Kirill sees who sent the message."),
			mcp.WithString("message", mcp.Description("Message text to send"), mcp.Required()),
			mcp.WithString("agentname", mcp.Description("Your own agent name — find it in your system prompt, in the description of who you are (e.g. if you are Belochka pass 'Белочка 🐿️', if Baron pass 'Барон 🐪🎩'). Always pass it.")),
			mcp.WithString("webhook_url", mcp.Description("Optional Discord webhook URL override (default: DISCORD_WEBHOOK_URL env)")),
		), Handler: handlerDiscordSend},
		{Tool: mcp.NewTool("discord_send_embed",
			mcp.WithDescription("Send a rich embed message to Discord via webhook. Supports title, description, color, and fields."),
			mcp.WithString("title", mcp.Description("Embed title")),
			mcp.WithString("description", mcp.Description("Embed description text")),
			mcp.WithNumber("color", mcp.Description("Embed color as decimal (default: 5793266 blurple)")),
			mcp.WithArray("fields", mcp.Description("Array of field objects with name, value, inline")),
			mcp.WithString("webhook_url", mcp.Description("Optional Discord webhook URL override (default: DISCORD_WEBHOOK_URL env)")),
		), Handler: handlerDiscordSendEmbed},
		{Tool: mcp.NewTool("discord_webhook_info",
			mcp.WithDescription("Check Discord webhook connectivity and return info."),
		), Handler: handlerDiscordWebhookInfo},
		{Tool: mcp.NewTool("discord_bot_status",
			mcp.WithDescription("Return the voice bot status: connected guilds, voice channel, listening state."),
		), Handler: handlerDiscordBotStatus},
		{Tool: mcp.NewTool("discord_voice_join",
			mcp.WithDescription("Join a Discord voice channel. If channel is empty, uses DISCORD_VOICE_CHANNEL env var."),
			mcp.WithString("channel", mcp.Description("Voice channel name or ID (optional, defaults to env)")),
		), Handler: handlerDiscordVoiceJoin},
		{Tool: mcp.NewTool("discord_voice_leave",
			mcp.WithDescription("Leave the current Discord voice channel."),
		), Handler: handlerDiscordVoiceLeave},
	}
}

// ctx is not used by the handlers yet; kept for signature compatibility.
var _ = context.Background
