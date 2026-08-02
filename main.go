// discord-mcp — Discord MCP server with webhook tools and a DAVE voice bot.
//
// Replaces the legacy Perl discord-mcp and adds voice chat (STT -> Baron ->
// TTS). The voice bot runs for the whole MCP server lifetime; webhook tools
// work independently via a Discord webhook URL.
package main

import (
	"log"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

// init redirects log output to a file: the gateway (and systemd) sends the
// child stderr to /dev/null, so bot logs would otherwise be invisible.
func init() {
	f, err := os.OpenFile("/tmp/discord-bot.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		log.SetOutput(f)
	}
}

func main() {
	// Voice bot runs in the background for the whole MCP server lifetime.
	go startBot()

	s := server.NewMCPServer(
		"discord-mcp",
		"1.0.0",
		server.WithInstructions(`Discord MCP server: webhook messaging and a voice bot (Baron).
The bot auto-joins the configured voice channel and listens for speech.`),
	)
	s.AddTools(tools()...)

	log.Printf("registered %d tools", len(tools()))

	// Start the server over stdin/stdout (JSON-RPC 2.0) — the transport used
	// by mcp-gateway for local servers.
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
