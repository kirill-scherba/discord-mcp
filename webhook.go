// webhook.go — Discord webhook tools (port of the legacy Perl implementation).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// webhookURL returns the Discord webhook URL from the environment.
func webhookURL() string {
	return os.Getenv("DISCORD_WEBHOOK_URL")
}

// httpClient shared by webhook calls.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// postWebhook posts a JSON payload to the Discord webhook and returns the HTTP
// status code and body.
func postWebhook(payload any) (int, []byte, error) {
	url := webhookURL()
	if url == "" {
		return 0, nil, fmt.Errorf("DISCORD_WEBHOOK_URL not set")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, respBody, nil
}

// handlerDiscordSend sends a simple text message.
func handlerDiscordSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	msg, ok := args["message"].(string)
	if !ok || msg == "" {
		return mcp.NewToolResultText("Error: Missing required: message"), nil
	}
	payload := map[string]any{"content": msg}
	if agentname, ok := args["agentname"].(string); ok && agentname != "" {
		payload["username"] = agentname
	}
	code, body, err := postWebhook(payload)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("Discord webhook error: HTTP %d — %s", code, body)
	}
	res, _ := json.Marshal(map[string]any{"sent": 1, "http_code": code})
	return mcp.NewToolResultText(string(res)), nil
}

// handlerDiscordSendEmbed sends a rich embed.
func handlerDiscordSendEmbed(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	embed := map[string]any{
		"title":       strOr(args["title"], ""),
		"description": strOr(args["description"], ""),
		"color":       intOr(args["color"], 5793266),
		"timestamp":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if fields, ok := args["fields"].([]any); ok && len(fields) > 0 {
		embed["fields"] = fields
	}
	payload := map[string]any{"embeds": []any{embed}}
	code, body, err := postWebhook(payload)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("Discord webhook error: HTTP %d — %s", code, body)
	}
	res, _ := json.Marshal(map[string]any{"sent": 1, "http_code": code})
	return mcp.NewToolResultText(string(res)), nil
}

// handlerDiscordWebhookInfo validates webhook connectivity.
func handlerDiscordWebhookInfo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	url := webhookURL()
	if url == "" {
		return nil, fmt.Errorf("DISCORD_WEBHOOK_URL not set")
	}
	r, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Discord webhook check failed: HTTP %d", resp.StatusCode)
	}
	var info struct {
		Name      string `json:"name"`
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
		Type      int    `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	res, _ := json.Marshal(map[string]any{
		"name":       info.Name,
		"channel":    info.ChannelID,
		"guild":      info.GuildID,
		"type":       info.Type,
		"webhook_ok": true,
	})
	return mcp.NewToolResultText(string(res)), nil
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func intOr(v any, def int) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return def
}
