package adapter_internal

import (
	"context"
	"io"
	"sync"
	"testing"

	adapter_channel "github.com/rapidaai/api/assistant-api/internal/adapters/channel"
	adapter_lifecycle "github.com/rapidaai/api/assistant-api/internal/adapters/lifecycle"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingStreamer captures what the requestor notifies the transport with.
// The WORD interruption is what makes telephony clear its queued audio, so the
// test has to be able to see it.
type recordingStreamer struct {
	mu   sync.Mutex
	sent []internal_type.Stream
}

func (s *recordingStreamer) Context() context.Context { return context.Background() }

func (s *recordingStreamer) Recv() (internal_type.Stream, error) { return nil, io.EOF }

func (s *recordingStreamer) Send(stream internal_type.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, stream)
	return nil
}

func (s *recordingStreamer) wordInterruptions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, stream := range s.sent {
		if interruption, ok := stream.(*protos.ConversationInterruption); ok &&
			interruption.GetType() == protos.ConversationInterruption_INTERRUPTION_TYPE_WORD {
			count++
		}
	}
	return count
}

func newInterruptTestRequestor(t *testing.T) (*genericRequestor, *recordingStreamer) {
	t.Helper()

	logger, err := commons.NewApplicationLogger(
		commons.Name("dispatch-handler-interrupt-test"),
		commons.Level("error"),
		commons.EnableFile(false),
	)
	require.NoError(t, err)

	streamer := &recordingStreamer{}
	return &genericRequestor{
		logger:           logger,
		channels:         adapter_channel.NewRequestorChannels(),
		source:           utils.Debugger,
		options:          map[string]interface{}{},
		messageLifecycle: adapter_lifecycle.NewMessageLifecycle(),
		streamer:         streamer,
	}, streamer
}

// drainStopPackets reports whether the packets that actually stop the assistant
// were emitted. Without both of them queued audio keeps playing.
func drainStopPackets(r *genericRequestor) (tts bool, llm bool) {
	for {
		select {
		case env := <-r.channels.ControlChannel():
			switch env.Pkt.(type) {
			case internal_type.TextToSpeechInterruptPacket:
				tts = true
			case internal_type.LLMInterruptPacket:
				llm = true
			}
		default:
			return tts, llm
		}
	}
}

// A repeat word-interrupt inside an already-interrupted turn must still stop the
// assistant. The Interrupted->Interrupted transition is rejected by design, and
// that rejection used to return early and skip the stop packets -- leaving
// queued audio playing that the caller could no longer interrupt, so both sides
// kept trying to take the turn and neither could.
func TestHandleInterruptionDetected_RepeatWordInterrupt_StillStops(t *testing.T) {
	r, _ := newInterruptTestRequestor(t)
	h := requestorDispatchHandler{r: r}

	h.HandleInterruptionDetected(t.Context(), internal_type.InterruptionDetectedPacket{
		ContextID: "ctx-1",
		Source:    internal_type.InterruptionSourceWord,
	})
	tts, llm := drainStopPackets(r)
	require.True(t, tts, "first interrupt must stop text-to-speech")
	require.True(t, llm, "first interrupt must stop the llm")

	// the turn is now Interrupted, so the next transition is guaranteed to fail
	require.Error(t, r.Transition(Interrupted), "precondition: repeat transition must be rejected")

	h.HandleInterruptionDetected(t.Context(), internal_type.InterruptionDetectedPacket{
		ContextID: "ctx-1",
		Source:    internal_type.InterruptionSourceWord,
	})
	tts, llm = drainStopPackets(r)
	assert.True(t, tts, "repeat interrupt must still stop text-to-speech")
	assert.True(t, llm, "repeat interrupt must still stop the llm")
}
