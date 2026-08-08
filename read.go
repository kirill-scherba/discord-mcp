// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style license.

// read.go — read channel messages via the Discord bot.
// Lets Baron answer "what's new in the reports" by actually reading a
// channel (e.g. #mail-butler), not only sending to it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// handlerDiscordRead reads recent messages from a channel (by name or ID).
func handlerDiscordRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	channel, _ := args["channel"].(string)
	if channel == "" {
		return mcp.NewToolResultText("Error: missing required: channel (name or ID)"), nil
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok && int(l) > 0 && int(l) <= 100 {
		limit = int(l)
	}
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		return mcp.NewToolResultText("Error: DISCORD_BOT_TOKEN not set"), nil
	}

	channelID, err := resolveChannelID(token, channel)
	if err != nil {
		return mcp.NewToolResultText("Error: " + err.Error()), nil
	}

	u := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages?limit=%d", channelID, limit)
	body, code, err := botGet(token, u)
	if err != nil {
		return mcp.NewToolResultText("Error: " + err.Error()), nil
	}
	if code != http.StatusOK {
		return mcp.NewToolResultText(fmt.Sprintf("Error: HTTP %d %s", code, truncate(string(body), 200))), nil
	}

	var msgs []struct {
		ID        string `json:"id"`
		Author    struct {
			Username string `json:"username"`
		} `json:"author"`
		Content   string `json:"content"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &msgs); err != nil {
		return mcp.NewToolResultText("Error: " + err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultText("Канал пуст"), nil
	}

	var sb strings.Builder
	for i, m := range msgs {
		t := m.Timestamp
		if len(t) >= 16 {
			t = t[:16]
		}
		sb.WriteString(fmt.Sprintf("[%d] %s @ %s:\n%s\n\n", i+1, m.Author.Username, t, truncate(m.Content, 500)))
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// resolveChannelID returns the channel ID for a name (case-insensitive,
// searched across guilds) or the input unchanged if it is already an ID.
func resolveChannelID(token, channel string) (string, error) {
	if _, err := strconv.ParseInt(channel, 10, 64); err == nil {
		return channel, nil // already an ID
	}

	guildsBody, code, err := botGet(token, "https://discord.com/api/v10/users/@me/guilds")
	if err != nil || code != http.StatusOK {
		return "", fmt.Errorf("guilds: HTTP %d %v", code, err)
	}
	var guilds []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(guildsBody, &guilds)

	needle := strings.ToLower(channel)
	for _, g := range guilds {
		chBody, code, err := botGet(token, fmt.Sprintf("https://discord.com/api/v10/guilds/%s/channels", g.ID))
		if err != nil || code != http.StatusOK {
			continue
		}
		var chans []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(chBody, &chans)
		for _, c := range chans {
			if strings.EqualFold(c.Name, needle) {
				return c.ID, nil
			}
		}
	}
	return "", fmt.Errorf("channel not found: %s", channel)
}

// botGet performs an authenticated GET with the bot token.
func botGet(token, url string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", "DiscordBot (https://matrica.work, 1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return body, resp.StatusCode, nil
}
