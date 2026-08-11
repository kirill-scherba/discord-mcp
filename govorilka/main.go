// govorilka — prototype WebRTC voice page (no Discord).
//
// Part of the discord-mcp module; uses the shared voicekit pipeline
// (STT -> brain -> TTS). Run from the module root:
//
//   go run ./govorilka
//
// Then open http://localhost:7790 in a browser.
package main

func main() {
	startGovorilka()
	// Keep the process alive.
	select {}
}
