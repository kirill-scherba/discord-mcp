// stt_stream.go — streaming speech-to-text via Yandex SpeechKit API v3.
//
// Unlike the synchronous sttYandex (one POST after the utterance ends), this
// streams PCM chunks to the server WHILE the user is speaking. The server
// decides when the utterance ends (endOfUtterance) based on meaning, not on a
// silence timer — so pauses inside a phrase no longer split it into pieces.
//
// Enabled with STT_PROVIDER=yandex-stream. The synchronous path (whisper,
// yandex) is kept intact.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	sttv3 "github.com/yandex-cloud/go-genproto/yandex/cloud/ai/stt/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// yandexStreamEndpoint is the gRPC endpoint for Yandex SpeechKit streaming STT.
const yandexStreamEndpoint = "stt.api.cloud.yandex.net:443"

// streamChunkSize is how many PCM frames (20ms each) per gRPC chunk. 5 frames
// = 100ms of audio per message — a good balance of latency vs overhead.
const streamChunkFrames = 5

// streamSilenceInterval is how often the bot sends a SilenceChunk to the
// server while the user is not talking, so the server emits eou_update for
// the completed utterance instead of hanging until the idle timeout.
const streamSilenceInterval = 800 * time.Millisecond

// sttStream is one active streaming recognition session.
type sttStream struct {
	mu     sync.Mutex
	stream grpc.BidiStreamingClient[sttv3.StreamingRequest, sttv3.StreamingResponse]
	cancel context.CancelFunc

	finalText string // accumulated final text of the current utterance
	lastAudio time.Time
}

// startYandexStream opens a bidirectional gRPC stream and sends the session
// options (48 kHz mono LPCM, Russian).
func startYandexStream() (*sttStream, error) {
	key := os.Getenv("YANDEX_AI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("YANDEX_AI_API_KEY not set")
	}

	conn, err := grpc.NewClient(yandexStreamEndpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(nil)),
	)
	if err != nil {
		return nil, fmt.Errorf("yandex stream: connect: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Api-Key "+key)

	stub := sttv3.NewRecognizerClient(conn)
	stream, err := stub.RecognizeStreaming(ctx)
	if err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("yandex stream: open: %w", err)
	}

	// Session options: LPCM 48 kHz mono, Russian. The v3 API always streams
	// partial + final results and sends eou_update at end of utterance.
	opts := &sttv3.StreamingOptions{
		RecognitionModel: &sttv3.RecognitionModelOptions{
			Model:               "general",
			AudioProcessingType: sttv3.RecognitionModelOptions_REAL_TIME,
			AudioFormat: &sttv3.AudioFormatOptions{
				AudioFormat: &sttv3.AudioFormatOptions_RawAudio{
					RawAudio: &sttv3.RawAudio{
						AudioEncoding:     sttv3.RawAudio_LINEAR16_PCM,
						SampleRateHertz:   48000,
						AudioChannelCount: 1,
					},
				},
			},
			TextNormalization: &sttv3.TextNormalizationOptions{
				TextNormalization: sttv3.TextNormalizationOptions_TEXT_NORMALIZATION_ENABLED,
				ProfanityFilter:   false,
				LiteratureText:    false,
			},
			LanguageRestriction: &sttv3.LanguageRestrictionOptions{
				RestrictionType: sttv3.LanguageRestrictionOptions_WHITELIST,
				LanguageCode:    []string{"ru-RU"},
			},
		},
	}

	if err := stream.Send(&sttv3.StreamingRequest{
		Event: &sttv3.StreamingRequest_SessionOptions{SessionOptions: opts},
	}); err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("yandex stream: send options: %w", err)
	}

	log.Printf("voice: yandex stream opened")
	return &sttStream{
		stream:    stream,
		cancel:    cancel,
		lastAudio: time.Now(),
	}, nil
}

// sendPCM pushes a PCM chunk ([]int16, 48 kHz mono) into the stream.
func (s *sttStream) sendPCM(pcm []int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Convert int16 -> little-endian bytes.
	buf := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		buf[i*2] = byte(v)
		buf[i*2+1] = byte(v >> 8)
	}

	if err := s.stream.Send(&sttv3.StreamingRequest{
		Event: &sttv3.StreamingRequest_Chunk{
			Chunk: &sttv3.AudioChunk{Data: buf},
		},
	}); err != nil {
		return fmt.Errorf("yandex stream: send chunk: %w", err)
	}
	s.lastAudio = time.Now()
	return nil
}

// sendSilence tells the server the user has gone quiet. The server treats it
// as a pause and emits eou_update for the completed utterance. Used when the
// bot stops receiving Discord packets (user stopped talking) so the stream
// doesn't hang until the idle timeout.
func (s *sttStream) sendSilence(ms int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.stream.Send(&sttv3.StreamingRequest{
		Event: &sttv3.StreamingRequest_SilenceChunk{
			SilenceChunk: &sttv3.SilenceChunk{DurationMs: ms},
		},
	}); err != nil {
		return fmt.Errorf("yandex stream: send silence: %w", err)
	}
	s.lastAudio = time.Now()
	return nil
}

// recvResult reads one server message. Returns (finalText, ended, err):
//   - finalText: the recognized text accumulated so far
//   - ended: true if the server signaled endOfUtterance (utterance complete)
//
// NOTE: Recv() blocks until the server sends something, so it must NOT hold
// s.mu — otherwise sendPCM (which takes the same mutex) would deadlock and no
// audio would ever be sent. finalText is only written here, read in the same
// goroutine, so no locking is needed for it.
func (s *sttStream) recvResult() (string, bool, error) {
	resp, err := s.stream.Recv()
	if err != nil {
		return s.finalText, false, fmt.Errorf("yandex stream: recv: %w", err)
	}

	switch e := resp.GetEvent().(type) {
	case *sttv3.StreamingResponse_Partial:
		// Intermediate result; text may change. Log for debugging only.
		if len(e.Partial.GetAlternatives()) > 0 {
			log.Printf("voice: yandex partial: %q", e.Partial.GetAlternatives()[0].GetText())
		}
	case *sttv3.StreamingResponse_Final:
		if len(e.Final.GetAlternatives()) > 0 {
			s.finalText = e.Final.GetAlternatives()[0].GetText()
		}
	case *sttv3.StreamingResponse_FinalRefinement:
		if len(e.FinalRefinement.GetNormalizedText().GetAlternatives()) > 0 {
			s.finalText = e.FinalRefinement.GetNormalizedText().GetAlternatives()[0].GetText()
		}
	case *sttv3.StreamingResponse_EouUpdate:
		// End of utterance — the phrase is complete.
		return s.finalText, true, nil
	}
	return s.finalText, false, nil
}

// close gracefully finishes the stream.
func (s *sttStream) close() {
	s.cancel()
}
