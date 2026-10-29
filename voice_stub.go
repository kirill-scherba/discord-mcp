// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build novoice

// The voice-free build: text tools only.
//
// Voice needs libopus and the DAVE end-to-end encryption library, both of them C. That is fine on a Linux
// server where the bot actually lives, and impossible to carry into a single Windows binary honestly - so
// this build keeps every tool that speaks HTTP and replaces the bot with a stub that explains itself.

package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// voiceBot is the do-nothing stand-in for the real bot: it has no connection to close.
type voiceBot struct{}

func (b *voiceBot) shutdown() {}

var theBot = &voiceBot{}

// startBot does nothing: without the voice stack there is no gateway connection to open.
func startBot() {}

func handlerDiscordVoiceJoin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("voice is not available in this build: it needs libopus and DAVE, which this binary does not carry. Text tools work normally."), nil
}

func handlerDiscordVoiceLeave(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("voice is not available in this build."), nil
}

func handlerDiscordBotStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("voice bot is not part of this build (Windows text-only)."), nil
}

// voiceTools returns nothing: this build has no voice, so it does not advertise voice tools.
func voiceTools() []server.ServerTool { return nil }
