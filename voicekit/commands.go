package voicekit

import "strings"

// MatchCommand normalizes a transcribed phrase into a known voice command.
// A command must be a SHORT phrase (<= 3 words): "молчи", "барон молчи",
// "повтори ещё раз". Long phrases that merely contain a command word
// ("молчи и слушай...") are NOT commands — they are ordinary speech.
// Root-substring matching tolerates STT noise ("молчу" vs "молчи").
// EXCEPTION: "барон" matches at the START of any-length phrase
// ("барон че то нет опять ответ потерялся") — it is the attention call,
// so a phrase that begins with it must break through to the pipeline.
// Commands (Govorilka wake-word world):
//   - "молчи" (sleep): "молчи", "замолчи" — stop listening, go to sleep
//     (wake up again with "Барон")
//   - "стоп" (interrupt only): "остановись", "хватит"
//   - "повтори" (replay last reply)
//   - "барон" (attention): interrupt playback, then continue the phrase
// NOTE: "продолжаем" was removed — there is no mute/un-mute anymore;
// you wake the peer with the wake word instead.
func MatchCommand(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return ""
	}
	// "барон" at the start always breaks through, any length.
	if strings.HasPrefix(t, "барон") {
		return "барон"
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
	case strings.Contains(t, "повтор"):
		return "повтори"
	}
	return ""
}
