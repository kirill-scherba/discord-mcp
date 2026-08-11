package voicekit

import "strings"

// MatchCommand normalizes a transcribed phrase into a known voice command.
// A command must be a SHORT phrase (<= 3 words): "молчи", "барон молчи",
// "повтори ещё раз". Long phrases that merely contain a command word
// ("молчи и слушай...") are NOT commands — they are ordinary speech.
// Root-substring matching tolerates STT noise ("молчу" vs "молчи").
// Commands:
//   - "молчи" (mute): "молчи", "замолчи"
//   - "стоп" (interrupt only): "остановись", "хватит"
//   - "продолжаем" (un-mute)
//   - "повтори" (replay last reply)
func MatchCommand(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return ""
	}
	// Only short phrases can be commands.
	if len(strings.Fields(t)) > 3 {
		return ""
	}
	switch {
	case strings.Contains(t, "останов") || strings.Contains(t, "хват"):
		return "стоп"
	case strings.Contains(t, "молч"):
		return "молчи"
	case strings.Contains(t, "продолж"):
		return "продолжаем"
	case strings.Contains(t, "повтор"):
		return "повтори"
	}
	return ""
}
