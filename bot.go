// bot.go — Discord Gateway voice bot.
//
// The bot runs for the whole MCP server lifetime. It connects to Discord via
// the Gateway API (not webhooks), listens for the !voice command, joins the
// configured voice channel, records speech, transcribes it, asks Baron
// (opencode-serve) for a reply, and speaks the reply back via TTS.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cartridge-gg/discordgo"
	"github.com/cartridge-gg/discordgo/dave"
	"github.com/mark3labs/mcp-go/mcp"
)

// redirectLibdaveLogs installs a global log sink for libdave. Without it,
// libdave writes log lines straight to stdout ("(file:line) ..."), which
// corrupts the MCP stdio protocol on this process's stdout.
func redirectLibdaveLogs() {
	dave.InstallLogSink(func(severity dave.LogSeverity, file string, line int, message string) {
		log.Printf("[dave] %s", message)
	})
}

// bot is the singleton voice bot.
type bot struct {
	session *discordgo.Session
	vc      *discordgo.VoiceConnection

	mu       sync.Mutex
	guildID  string
	channelID string
	listening bool
	ttsSpeaking bool
	// busy is set when an utterance has been sent for processing (STT ->
	// brain -> TTS). While busy, incoming audio is discarded (it is either
	// the tail of the phrase being processed or speech that cannot interrupt
	// the bot anyway). Cleared after playback finishes.
	busy bool

	// processMu serializes the STT -> brain -> TTS pipeline so replies
	// never overlap. speakMu guards the OpusSend writer against concurrent
	// playback from different goroutines.
	processMu sync.Mutex
	speakMu   sync.Mutex

	// voice states of users in the channel: userID -> {channelID}
	usersInChannel map[string]bool
}

var theBot = &bot{usersInChannel: make(map[string]bool)}

// startBot connects the Gateway bot if a token is present. It never returns
// on success; on failure it logs and keeps the MCP server alive.
func startBot() {
	redirectLibdaveLogs()
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Printf("discord-bot: DISCORD_BOT_TOKEN not set, voice bot disabled")
		return
	}

	s, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Printf("discord-bot: failed to create session: %v", err)
		return
	}
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildVoiceStates |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent
	s.LogLevel = discordgo.LogInformational

	s.AddHandler(theBot.onReady)
	s.AddHandler(theBot.onMessageCreate)
	s.AddHandler(theBot.onVoiceStateUpdate)

	if err := s.Open(); err != nil {
		log.Printf("discord-bot: failed to open gateway: %v", err)
		return
	}

	theBot.mu.Lock()
	theBot.session = s
	theBot.mu.Unlock()

	log.Printf("discord-bot: connected to Discord")
	select {} // keep the goroutine alive; bot handlers run in session goroutines
}

// onReady logs the connected guilds.
func (b *bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("discord-bot: ready, user=%s, guilds=%d", r.User.Username, len(r.Guilds))
	for _, g := range r.Guilds {
		log.Printf("discord-bot: guild %s (%s)", g.Name, g.ID)
	}
	// Auto-join disabled — use discord_voice_join MCP tool or !voice command.
}

// autoJoin joins the voice channel from DISCORD_VOICE_CHANNEL env after a
// short delay so the session is fully ready.
func (b *bot) autoJoin() {
	time.Sleep(2 * time.Second)
	channelName := os.Getenv("DISCORD_VOICE_CHANNEL")
	if channelName == "" {
		log.Printf("discord-bot: DISCORD_VOICE_CHANNEL not set, waiting for !voice command")
		return
	}
	b.mu.Lock()
	s := b.session
	b.mu.Unlock()
	if s == nil {
		return
	}
	guildID, channelID := b.resolveChannel(s, channelName)
	if guildID == "" || channelID == "" {
		log.Printf("discord-bot: channel %q not found, waiting for !voice command", channelName)
		return
	}
	if err := b.joinVoice(s, guildID, channelID); err != nil {
		log.Printf("discord-bot: auto-join failed: %v", err)
	}
}

// onMessageCreate handles text commands.
func (b *bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.ID == s.State.User.ID {
		return
	}
	content := strings.TrimSpace(m.Content)
	switch {
	case strings.HasPrefix(content, "!voice"):
		channelID := m.ChannelID
		// Try to join the voice channel the user is currently in.
		guildID := m.GuildID
		if vcID, ok := b.voiceChannelOfUser(guildID, m.Author.ID); ok {
			channelID = vcID
		} else if name := os.Getenv("DISCORD_VOICE_CHANNEL"); name != "" {
			if _, id := b.resolveChannel(s, name); id != "" {
				channelID = id
			}
		}
		if guildID == "" {
			s.ChannelMessageSend(m.ChannelID, "Этот сервер не поддерживается.")
			return
		}
		if err := b.joinVoice(s, guildID, channelID); err != nil {
			s.ChannelMessageSend(m.ChannelID, "Не могу зайти в голосовой канал: "+err.Error())
			return
		}
		s.ChannelMessageSend(m.ChannelID, "🎙 Барон на связи, Ваше Величество.")
	case strings.HasPrefix(content, "!leave"):
		b.leaveVoice()
	case strings.HasPrefix(content, "!status"):
		b.mu.Lock()
		status := fmt.Sprintf("guild: %s, channel: %s, listening: %v",
			b.guildID, b.channelID, b.listening)
		b.mu.Unlock()
		s.ChannelMessageSend(m.ChannelID, "Статус: "+status)
	}
}

// onVoiceStateUpdate tracks which users are in the voice channel and notifies
// Baron when a participant joins or leaves, including the current roster.
func (b *bot) onVoiceStateUpdate(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	b.mu.Lock()
	if vs.UserID == s.State.User.ID {
		b.mu.Unlock()
		return
	}
	wasIn := b.usersInChannel[vs.UserID]
	if vs.ChannelID != "" {
		b.usersInChannel[vs.UserID] = true
	} else {
		delete(b.usersInChannel, vs.UserID)
	}
	present := make([]string, 0, len(b.usersInChannel))
	for uid := range b.usersInChannel {
		present = append(present, uid)
	}
	b.mu.Unlock()

	// Notify Baron (technical message, not spoken aloud) on real join/leave,
	// not on mic toggles (mic toggle keeps ChannelID non-empty).
	if vs.ChannelID != "" && !wasIn {
		go b.notifyJoined(s, vs.UserID, present)
	} else if vs.ChannelID == "" && wasIn {
		go b.notifyLeft(s, vs.UserID, present)
	}
}

// notifyJoined resolves the user's display name and passes it to Baron.
func (b *bot) notifyJoined(s *discordgo.Session, userID string, present []string) {
	display := userID
	if u, err := s.User(userID); err == nil {
		display = u.Username
		if u.GlobalName != "" {
			display = u.GlobalName
		}
	}
	log.Printf("discord-bot: participant joined: %s (%s)", display, userID)
	brainNotifyState("join", display, userID, present)
}

// notifyLeft resolves the user's display name and passes it to Baron.
func (b *bot) notifyLeft(s *discordgo.Session, userID string, present []string) {
	display := userID
	if u, err := s.User(userID); err == nil {
		display = u.Username
		if u.GlobalName != "" {
			display = u.GlobalName
		}
	}
	log.Printf("discord-bot: participant left: %s (%s)", display, userID)
	brainNotifyState("leave", display, userID, present)
}

// onVoiceSpeakingUpdate is wired here for speaker-ID (Level 1). Actual audio
// buffering per speaker happens in the recording loop in voice.go.
func (b *bot) onVoiceSpeakingUpdate(vc *discordgo.VoiceConnection, vs *discordgo.VoiceSpeakingUpdate) {
	// no-op for now; recording loop handles per-speaker buffering via SSRC
}

// voiceChannelOfUser returns the voice channel ID a user is currently in.
func (b *bot) voiceChannelOfUser(guildID, userID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.channelID, b.usersInChannel[userID] && b.channelID != ""
}

// resolveChannel finds guildID+channelID by channel name (or ID). It uses the
// REST API for guild channels since State may be incomplete right after Ready.
func (b *bot) resolveChannel(s *discordgo.Session, name string) (string, string) {
	// Collect guild IDs from State (may include unavailable guilds).
	guildIDs := []string{}
	for _, g := range s.State.Guilds {
		guildIDs = append(guildIDs, g.ID)
	}
	if len(guildIDs) == 0 {
		if guilds, err := s.UserGuilds(10, "", "", false); err == nil {
			for _, g := range guilds {
				guildIDs = append(guildIDs, g.ID)
			}
		}
	}

	for _, gid := range guildIDs {
		// If name is a snowflake ID, use it directly.
		if len(name) >= 17 && isDigits(name) {
			return gid, name
		}
		channels, err := s.GuildChannels(gid)
		if err != nil {
			continue
		}
		for _, ch := range channels {
			if ch.Name == name && ch.Type == discordgo.ChannelTypeGuildVoice {
				return gid, ch.ID
			}
		}
	}
	return "", ""
}

// joinVoice joins the given voice channel and starts the recording loop.
func (b *bot) joinVoice(s *discordgo.Session, guildID, channelID string) error {
	b.leaveVoice()

	var vc *discordgo.VoiceConnection
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		vc, err = s.ChannelVoiceJoin(guildID, channelID, false, false)
		if err == nil {
			break
		}
		log.Printf("discord-bot: voice join attempt %d failed: %v", attempt, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return err
	}
	vc.Speaking(false)
	vc.AddHandler(theBot.onVoiceSpeakingUpdate)

	b.mu.Lock()
	b.session = s
	b.vc = vc
	b.guildID = guildID
	b.channelID = channelID
	b.listening = true
	b.mu.Unlock()

	log.Printf("discord-bot: joined voice channel %s", channelID)
	if sttProvider() == "yandex-stream" {
		go b.recordingLoopStream(vc, guildID, channelID)
	} else {
		go b.recordingLoop(vc, guildID, channelID)
	}
	return nil
}

// leaveVoice leaves the current voice channel and stops recording.
func (b *bot) leaveVoice() {
	b.mu.Lock()
	vc := b.vc
	b.vc = nil
	b.guildID = ""
	b.channelID = ""
	b.listening = false
	b.mu.Unlock()
	if vc != nil {
		vc.Speaking(false)
		vc.Disconnect()
	}
}

// shutdown performs a graceful shutdown on SIGTERM/SIGINT: leaves the voice
// channel first (so Discord is notified the bot disconnected) and then closes
// the gateway session. Without this, a killed process leaves the bot visible
// in the channel as a ghost until Discord's timeout expires.
func (b *bot) shutdown() {
	log.Printf("discord-bot: graceful shutdown")
	b.leaveVoice()
	b.mu.Lock()
	s := b.session
	b.mu.Unlock()
	if s != nil {
		if err := s.Close(); err != nil {
			log.Printf("discord-bot: gateway close: %v", err)
		}
	}
	log.Printf("discord-bot: shutdown complete")
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// handlerDiscordVoiceJoin is the MCP tool handler for discord_voice_join.
func handlerDiscordVoiceJoin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	theBot.mu.Lock()
	s := theBot.session
	theBot.mu.Unlock()
	if s == nil {
		return mcp.NewToolResultText("бот не подключён к Discord"), nil
	}

	channelName := ""
	if args, ok := req.Params.Arguments.(map[string]any); ok {
		if v, ok2 := args["channel"]; ok2 {
			if s, ok2 := v.(string); ok2 {
				channelName = s
			}
		}
	}
	if channelName == "" {
		channelName = os.Getenv("DISCORD_VOICE_CHANNEL")
	}
	if channelName == "" {
		return mcp.NewToolResultText("не указан канал и DISCORD_VOICE_CHANNEL не задан"), nil
	}

	guildID, channelID := theBot.resolveChannel(s, channelName)
	if guildID == "" || channelID == "" {
		return mcp.NewToolResultText(fmt.Sprintf("канал %q не найден", channelName)), nil
	}

	if err := theBot.joinVoice(s, guildID, channelID); err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("ошибка: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("зашёл в канал %s", channelName)), nil
}

// handlerDiscordVoiceLeave is the MCP tool handler for discord_voice_leave.
func handlerDiscordVoiceLeave(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	theBot.leaveVoice()
	return mcp.NewToolResultText("вышел из голосового канала"), nil
}

// handlerDiscordBotStatus returns bot status for the MCP tool.
func handlerDiscordBotStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	theBot.mu.Lock()
	defer theBot.mu.Unlock()
	status := map[string]any{
		"connected": theBot.session != nil,
		"guild":     theBot.guildID,
		"channel":   theBot.channelID,
		"listening": theBot.listening,
	}
	if theBot.session != nil {
		status["username"] = theBot.session.State.User.Username
	}
	res, _ := json.Marshal(status)
	return mcp.NewToolResultText(string(res)), nil
}
