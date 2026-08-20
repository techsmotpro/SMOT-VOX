// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_exotel_telephony

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	callcontext "github.com/rapidaai/api/assistant-api/internal/callcontext"
	internal_telephony_base "github.com/rapidaai/api/assistant-api/internal/channel/telephony/internal/base"
	internal_exotel "github.com/rapidaai/api/assistant-api/internal/channel/telephony/internal/exotel/internal"
	internal_telephony_media "github.com/rapidaai/api/assistant-api/internal/channel/telephony/internal/media"
	"github.com/rapidaai/api/assistant-api/internal/observability"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/protos"
)

// MaxSessionDuration is the hard ceiling on an Exotel call leg. Exotel exposes
// no REST hangup (the Legs API is a paid add-on that returns 401 unless enabled
// on the account), so closing the WebSocket is the only way to release a leg.
// That path works — Exotel drops the leg within ~2s — but it only runs when
// something decides to end the call. If the model never emits end_conversation
// and the caller never hangs up, nothing stops the call. This is that backstop.
const MaxSessionDuration = 10 * time.Minute

// Drain tuning for server-initiated hangups. Closing the WebSocket releases the
// Exotel leg immediately, cutting off any assistant audio still queued for
// playout — a goodbye line gets truncated mid-word. drainOutput waits for the
// pacer to stop emitting real audio before the close, capped so a stuck TTS
// stream can never hold a leg (and its billing) open.
const (
	// DrainTimeout is deliberately short: releasing the leg promptly matters
	// more than delivering every last syllable, and a long grace period keeps a
	// call (and its billing) alive whenever TTS stalls. A farewell that needs
	// longer than this gets clipped — an accepted trade for a bounded hangup.
	DrainTimeout      = 10 * time.Second
	drainPollInterval = 100 * time.Millisecond
	// drainQuietPolls is how many consecutive flat-ActiveTicks polls count as
	// "finished speaking". 3 * 100ms tolerates normal inter-frame jitter
	// without adding audible dead air before the hangup.
	drainQuietPolls = 3
)

// ClosingHangupDelay is the pause between the assistant finishing a farewell
// and the leg being released. The model is unreliable about emitting
// end_conversation in the same turn as its closing line -- it routinely waits
// for another user turn that never comes -- so the transport ends the call
// itself once it hears a closing.
const ClosingHangupDelay = 5 * time.Second

// ClosingTailGrace covers audio that has left the pacer but has not reached the
// caller yet: the websocket write, Exotel's own buffering and the carrier's
// jitter buffer. drainOutput only knows that the last frame was *sent*, so
// closing the moment it goes quiet clips the final word ("Goodbye" arriving as
// "good"). This grace is additive to the drain, not a replacement for it.
const ClosingTailGrace = 1200 * time.Millisecond

// closingMarkers are phrases that only appear when the assistant is signing
// off. A turn containing a question is never treated as a closing: the
// assistant is still expecting an answer, and hanging up would cut the caller
// off mid-thought.
var closingMarkers = []string{
	"thank you",
	"thanks for your time",
	"goodbye",
	"good bye",
	"have a good",
	"have a nice",
	"take care",
	"call you back",
}

func isClosingLine(text string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(text))
	if trimmed == "" || strings.Contains(trimmed, "?") {
		return false
	}
	for _, marker := range closingMarkers {
		if strings.Contains(trimmed, marker) {
			return true
		}
	}
	return false
}

// noteAssistantText accumulates streamed text for the current assistant turn.
// Any new speech also disarms a pending hangup: the assistant is talking again,
// so whatever looked like a farewell was not the end of the call.
func (exotel *exotelWebsocketStreamer) noteAssistantText(chunk string) {
	exotel.closingMu.Lock()
	defer exotel.closingMu.Unlock()
	if exotel.closingTimer != nil {
		exotel.closingTimer.Stop()
		exotel.closingTimer = nil
	}
	exotel.turnText.WriteString(chunk)
}

// cancelClosingHangup disarms a pending hangup and clears the turn buffer. Used
// when the caller interrupts: they are still on the line and still talking.
func (exotel *exotelWebsocketStreamer) cancelClosingHangup() {
	exotel.closingMu.Lock()
	defer exotel.closingMu.Unlock()
	if exotel.closingTimer != nil {
		exotel.closingTimer.Stop()
		exotel.closingTimer = nil
	}
	exotel.turnText.Reset()
}

// requestClosingHangup records that the model asked to end the call. The
// hangup itself waits for the current turn to finish speaking.
func (exotel *exotelWebsocketStreamer) requestClosingHangup() {
	exotel.closingMu.Lock()
	exotel.endRequested = true
	exotel.closingMu.Unlock()
}

// armClosingHangup runs when an assistant turn finishes speaking. If that turn
// was a farewell, the leg is released after ClosingHangupDelay unless the
// caller speaks or the assistant starts a new turn first.
func (exotel *exotelWebsocketStreamer) armClosingHangup() {
	exotel.closingMu.Lock()
	spoken := exotel.turnText.String()
	exotel.turnText.Reset()
	requested := exotel.endRequested
	exotel.endRequested = false
	if !requested && !isClosingLine(spoken) {
		exotel.closingMu.Unlock()
		return
	}
	if exotel.closingTimer != nil {
		exotel.closingTimer.Stop()
	}
	exotel.closingTimer = time.AfterFunc(ClosingHangupDelay, func() {
		if exotel.closed.Load() {
			return
		}
		_ = exotel.Record(observability.RecordEvent{
			Component: observability.ComponentCall,
			Event:     observability.CallHangup,
			Attributes: observability.Attributes{
				"component":         observability.ComponentCall.String(),
				"provider":          internal_exotel.Provider,
				"stream_id":         exotel.streamID,
				"conversation_uuid": exotel.ChannelUUID,
				"reason":            "closing_line_hangup",
			},
		}, observability.RecordMetadata{
			Metadata: []*protos.Metadata{
				{Key: observability.MetadataCallStatus, Value: "completed"},
				{Key: observability.MetadataDisconnectReason, Value: "closing_line_hangup"},
			},
		})
		exotel.drainOutput()
		// Let the tail reach the caller before the leg is released.
		select {
		case <-exotel.Ctx.Done():
		case <-time.After(ClosingTailGrace):
		}
		_ = exotel.Cancel()
	})
	exotel.closingMu.Unlock()
}

type exotelWebsocketStreamer struct {
	internal_telephony_base.BaseTelephonyStreamer
	mediaSession *internal_telephony_media.MediaSession
	connection   *websocket.Conn
	writeMu      sync.Mutex
	closed       atomic.Bool
	streamID     string

	// Closing-line hangup state. turnText accumulates the assistant's streamed
	// text for the current turn; closingTimer is armed once that turn finishes
	// speaking and its text reads as a farewell.
	closingMu    sync.Mutex
	turnText     strings.Builder
	closingTimer *time.Timer
	endRequested bool
}

type StreamerOptions struct {
	Logger          commons.Logger
	Connection      *websocket.Conn
	CallContext     *callcontext.CallContext
	VaultCredential *protos.VaultCredential
	Observer        observability.Recorder
}

type FuncOption func(*StreamerOptions)

func WithLogger(logger commons.Logger) FuncOption {
	return func(options *StreamerOptions) {
		options.Logger = logger
	}
}

func WithConnection(connection *websocket.Conn) FuncOption {
	return func(options *StreamerOptions) {
		options.Connection = connection
	}
}

func WithCallContext(callContext *callcontext.CallContext) FuncOption {
	return func(options *StreamerOptions) {
		options.CallContext = callContext
	}
}

func WithVaultCredential(vaultCredential *protos.VaultCredential) FuncOption {
	return func(options *StreamerOptions) {
		options.VaultCredential = vaultCredential
	}
}

func WithObserver(observer observability.Recorder) FuncOption {
	return func(options *StreamerOptions) {
		options.Observer = observer
	}
}

func New(opts ...FuncOption) (internal_type.Streamer, error) {
	var options StreamerOptions
	for _, opt := range opts {
		opt(&options)
	}
	audioProcessor, err := internal_exotel.NewAudioProcessor(options.Logger)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", internal_exotel.ErrAudioProcessorInitFailed, err)
	}
	exotel := &exotelWebsocketStreamer{
		BaseTelephonyStreamer: internal_telephony_base.New(
			options.Logger, options.CallContext, options.VaultCredential, options.Observer,
		),
		streamID:   "",
		connection: options.Connection,
	}
	exotel.mediaSession = internal_telephony_media.NewMediaSession(internal_telephony_media.MediaSessionConfig{
		Context:     exotel.Ctx,
		Logger:      options.Logger,
		MediaEngine: audioProcessor,
		SendProviderClear: func() error {
			return exotel.sendExotelMessage(internal_exotel.EventTypeClear, nil)
		},
		StreamSink: exotel.Input,
		OutputSink: exotel.sendOutputFrame,
		Record:     exotel.Record,
	})
	go exotel.runWebSocketReader()
	go exotel.runSessionWatchdog()
	return exotel, nil
}

// drainOutput blocks until the assistant has finished speaking or DrainTimeout
// elapses, so a server-initiated hangup does not truncate queued audio.
// ActiveTicks advances only on frames carrying real audio; once it stops moving
// the pacer is emitting silence and the leg is safe to release.
func (exotel *exotelWebsocketStreamer) drainOutput() {
	if exotel.mediaSession == nil || exotel.closed.Load() {
		return
	}
	deadline := time.NewTimer(DrainTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()

	lastActive := exotel.mediaSession.OutputHealthSnapshot().ActiveTicks
	quiet := 0
	for {
		select {
		case <-exotel.Ctx.Done():
			return
		case <-deadline.C:
			_ = exotel.Record(observability.RecordLog{
				Level:   observability.LevelDebug,
				Message: "Exotel drain timed out with audio still pending",
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"drain_timeout":     DrainTimeout.String(),
				},
			})
			return
		case <-ticker.C:
			active := exotel.mediaSession.OutputHealthSnapshot().ActiveTicks
			if active != lastActive {
				lastActive = active
				quiet = 0
				continue
			}
			if quiet++; quiet >= drainQuietPolls {
				return
			}
		}
	}
}

// runSessionWatchdog force-ends the call if it outlives MaxSessionDuration.
// Exits quietly when the call ends normally — Ctx is cancelled by Cancel().
func (exotel *exotelWebsocketStreamer) runSessionWatchdog() {
	timer := time.NewTimer(MaxSessionDuration)
	defer timer.Stop()

	select {
	case <-exotel.Ctx.Done():
		return
	case <-timer.C:
		if exotel.closed.Load() {
			return
		}
		_ = exotel.Record(observability.RecordEvent{
			Component: observability.ComponentCall,
			Event:     observability.CallHangup,
			Attributes: observability.Attributes{
				"component":         observability.ComponentCall.String(),
				"provider":          internal_exotel.Provider,
				"stream_id":         exotel.streamID,
				"conversation_uuid": exotel.ChannelUUID,
				"reason":            "max_session_duration_exceeded",
				"max_duration":      MaxSessionDuration.String(),
			},
		}, observability.RecordMetadata{
			Metadata: []*protos.Metadata{
				{Key: observability.MetadataCallStatus, Value: "completed"},
				{Key: observability.MetadataDisconnectReason, Value: "max_session_duration_exceeded"},
			},
		})
		_ = exotel.Disconnect(protos.ConversationDisconnection_DISCONNECTION_TYPE_MAX_DURATION)
		exotel.stopAudioProcessing()
		_ = exotel.Cancel()
	}
}

func (exotel *exotelWebsocketStreamer) runWebSocketReader() {
	conn := exotel.connection
	if conn == nil {
		return
	}
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			exotel.stopAudioProcessing()
			_ = exotel.Record(observability.RecordLog{
				Level:   observability.LevelDebug,
				Message: "Exotel websocket reader closed",
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"error":             err.Error(),
				},
			}, observability.RecordEvent{
				Component: observability.ComponentCall,
				Event:     observability.CallEnded,
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"reason":            "websocket_closed",
				},
			}, observability.RecordMetadata{
				Metadata: []*protos.Metadata{
					{Key: observability.MetadataCallStatus, Value: "websocket_closed"},
					{Key: observability.MetadataDisconnectReason, Value: "websocket_closed"},
				},
			}, observability.RecordMetric{
				Metrics: []*protos.Metric{{
					Name:        observability.MetricCallStatus,
					Value:       "COMPLETE",
					Description: "Exotel websocket reader closed",
				}},
			})
			if msg := exotel.Disconnect(protos.ConversationDisconnection_DISCONNECTION_TYPE_USER); msg != nil {
				exotel.Input(msg)
			}
			exotel.BaseStreamer.Cancel()
			return
		}
		var mediaEvent internal_exotel.ExotelMediaEvent
		if err := json.Unmarshal(message, &mediaEvent); err != nil {
			_ = exotel.Record(observability.RecordLog{
				Level:   observability.LevelError,
				Message: "Failed to unmarshal Exotel media event",
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"error":             err.Error(),
				},
			}, observability.RecordMetric{
				Metrics: []*protos.Metric{{
					Name:        observability.MetricCallStatus,
					Value:       "FAILED",
					Description: "Failed to unmarshal Exotel media event",
				}},
			})
			continue
		}
		switch mediaEvent.Event {
		case internal_exotel.EventTypeConnected:
			exotel.Input(exotel.CreateConnectionRequest())
			_ = exotel.Record(observability.RecordEvent{
				Component: observability.ComponentCall,
				Event:     observability.CallSessionConnected,
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"provider_event":    string(internal_exotel.EventTypeConnected),
					"conversation_uuid": exotel.ChannelUUID,
				},
			}, observability.RecordMetadata{
				Metadata: []*protos.Metadata{
					{Key: observability.MetadataClientChannel, Value: internal_exotel.Provider},
					{Key: observability.MetadataClientProviderCallID, Value: exotel.ChannelUUID},
					{Key: observability.MetadataCallStatus, Value: "connected"},
				},
			}, observability.RecordMetric{
				Metrics: []*protos.Metric{{
					Name:        observability.MetricCallStatus,
					Value:       "INPROGRESS",
					Description: "Exotel websocket connected",
				}},
			})
		case internal_exotel.EventTypeStart:
			exotel.handleStartEvent(mediaEvent)
			if exotel.mediaSession != nil {
				exotel.mediaSession.Start()
			}
			_ = exotel.Record(observability.RecordEvent{
				Component: observability.ComponentCall,
				Event:     observability.CallMediaStarted,
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"provider_event":    string(internal_exotel.EventTypeStart),
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
				},
			}, observability.RecordMetadata{
				Metadata: []*protos.Metadata{
					{Key: observability.MetadataClientChannel, Value: internal_exotel.Provider},
					{Key: observability.MetadataClientProviderCallID, Value: exotel.ChannelUUID},
					{Key: observability.MetadataClientCodec, Value: "linear16"},
					{Key: observability.MetadataClientSampleRate, Value: "16000"},
					{Key: observability.MetadataCallStatus, Value: "media_started"},
					{Key: "exotel.stream_id", Value: exotel.streamID},
				},
			}, observability.RecordMetric{
				Metrics: []*protos.Metric{{
					Name:        observability.MetricCallStatus,
					Value:       "INPROGRESS",
					Description: "Exotel media stream started",
				}},
			})
		case internal_exotel.EventTypeMedia:
			if err := exotel.handleMediaEvent(mediaEvent); err != nil {
				_ = exotel.Record(observability.RecordLog{
					Level:   observability.LevelError,
					Message: "Failed to process Exotel media frame",
					Attributes: observability.Attributes{
						"component":         observability.ComponentCall.String(),
						"provider":          internal_exotel.Provider,
						"stream_id":         exotel.streamID,
						"conversation_uuid": exotel.ChannelUUID,
						"error":             err.Error(),
					},
				}, observability.RecordMetric{
					Metrics: []*protos.Metric{{
						Name:        observability.MetricCallStatus,
						Value:       "FAILED",
						Description: "Exotel media frame processing failed",
					}},
				})
			}
		case internal_exotel.EventTypeDTMF:
			_ = exotel.Record(observability.RecordEvent{
				Component: observability.ComponentCall,
				Event:     observability.CallStatus,
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"provider_event":    string(internal_exotel.EventTypeDTMF),
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"status":            internal_exotel.ChannelEventDTMF,
				},
			})
		case internal_exotel.EventTypeStop:
			_ = exotel.Record(observability.RecordEvent{
				Component: observability.ComponentCall,
				Event:     observability.CallHangup,
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"provider_event":    string(internal_exotel.EventTypeStop),
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"reason":            "provider_stop",
				},
			}, observability.RecordMetadata{
				Metadata: []*protos.Metadata{
					{Key: observability.MetadataCallStatus, Value: "provider_stop"},
					{Key: observability.MetadataDisconnectReason, Value: "provider_stop"},
				},
			}, observability.RecordMetric{
				Metrics: []*protos.Metric{{
					Name:        observability.MetricCallStatus,
					Value:       "COMPLETE",
					Description: "Exotel media stream stopped by provider",
				}},
			})
			if msg := exotel.Disconnect(protos.ConversationDisconnection_DISCONNECTION_TYPE_USER); msg != nil {
				exotel.Input(msg)
			}
			exotel.Cancel()
			return
		default:
			_ = exotel.Record(observability.RecordLog{
				Level:   observability.LevelDebug,
				Message: "Unhandled Exotel event",
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"provider_event":    string(mediaEvent.Event),
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
				},
			})
		}
	}
}

func (exotel *exotelWebsocketStreamer) Send(response internal_type.Stream) error {
	switch data := response.(type) {
	case *protos.ConversationInitialization:
		if exotel.mediaSession != nil {
			exotel.mediaSession.HandleInitialization(data)
		}
	case *protos.ConversationAssistantMessage:
		// End of turn arrives as a bare Completed message with no Audio and no
		// Text payload, so it has to be handled before the payload switch --
		// inside it, the nil Message matches no case and the turn looks like it
		// never ended.
		if data.GetCompleted() {
			exotel.armClosingHangup()
		}
		switch content := data.Message.(type) {
		case *protos.ConversationAssistantMessage_Audio:
			if exotel.mediaSession == nil {
				return nil
			}
			if err := exotel.mediaSession.HandleAssistantAudio(content.Audio, data.GetCompleted()); err != nil {
				return err
			}
			return nil
		case *protos.ConversationAssistantMessage_Text:
			exotel.noteAssistantText(content.Text)
			return nil
		}
	case *protos.ConversationInterruption:
		if data.Type == protos.ConversationInterruption_INTERRUPTION_TYPE_WORD {
			exotel.cancelClosingHangup()
			if exotel.mediaSession != nil {
				exotel.mediaSession.HandleInterrupt()
			}
		}
	case *protos.ConversationDisconnection:
		// Server-initiated disconnect: the talker already knows the reason
		// (it called Notify with it). No need to round-trip back through
		// CriticalCh — Exotel has no REST API to terminate a call; closing
		// the WebSocket via Cancel is the only way to release the call leg.
		//
		// Let any in-flight speech finish first. Skipped when the caller is
		// already gone (USER) or the session is broken (ERROR) — nobody is
		// listening, so waiting would only keep a dead leg billing.
		switch data.GetType() {
		case protos.ConversationDisconnection_DISCONNECTION_TYPE_USER,
			protos.ConversationDisconnection_DISCONNECTION_TYPE_ERROR:
		default:
			exotel.drainOutput()
		}
		_ = exotel.Disconnect(data.GetType())
		_ = exotel.Record(observability.RecordEvent{
			Component: observability.ComponentCall,
			Event:     observability.CallHangup,
			Attributes: observability.Attributes{
				"component":          observability.ComponentCall.String(),
				"provider":           internal_exotel.Provider,
				"stream_id":          exotel.streamID,
				"conversation_uuid":  exotel.ChannelUUID,
				"disconnection_type": data.GetType().String(),
				"reason":             "server_side_disconnect",
			},
		}, observability.RecordMetadata{
			Metadata: []*protos.Metadata{
				{Key: observability.MetadataCallStatus, Value: "completed"},
				{Key: observability.MetadataDisconnectReason, Value: "server_side_disconnect"},
			},
		}, observability.RecordMetric{
			Metrics: []*protos.Metric{{
				Name:        observability.MetricCallStatus,
				Value:       "COMPLETE",
				Description: "Exotel call ended by server-side disconnect",
			}},
		})
		exotel.stopAudioProcessing()
		exotel.Cancel()
	case *protos.ConversationToolCall:
		switch data.GetAction() {
		case protos.ToolCallAction_TOOL_CALL_ACTION_END_CONVERSATION:
			_ = exotel.Record(observability.RecordEvent{
				Component: observability.ComponentCall,
				Event:     observability.CallHangup,
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"tool_action":       data.GetAction().String(),
					"reason":            "tool_end_conversation",
				},
			}, observability.RecordMetadata{
				Metadata: []*protos.Metadata{
					{Key: observability.MetadataCallStatus, Value: "completed"},
					{Key: observability.MetadataDisconnectReason, Value: "tool_end_conversation"},
				},
			}, observability.RecordMetric{
				Metrics: []*protos.Metric{{
					Name:        observability.MetricCallStatus,
					Value:       "COMPLETE",
					Description: "Exotel call ended by tool action",
				}},
			})
			exotel.Input(&protos.ConversationToolCallResult{
				Id:     data.GetId(),
				ToolId: data.GetToolId(),
				Name:   data.GetName(),
				Action: data.GetAction(),
				Result: map[string]string{"status": "completed"},
			})
			// Do NOT hang up here. The model emits this tool alongside the text
			// of its closing line, before TTS has produced a single frame --
			// draining now sees a pacer that has not started yet, reads it as
			// "finished speaking", and cuts the farewell off mid-word. Record
			// the intent and let the end-of-turn handler release the leg once
			// the audio has actually played.
			exotel.requestClosingHangup()
		case protos.ToolCallAction_TOOL_CALL_ACTION_TRANSFER_CONVERSATION:
			// Exotel transfer is NOT supported. Exotel exposes call-flow level
			// "Connect" applets but no live mid-call transfer API on the
			// streaming WebSocket leg. A blind transfer would require building
			// an out-of-band Connect/Dial app and redirecting via the Exotel
			// HTTP API; resume_ai is not feasible without a B2BUA bridge
			// (Exotel does not provide an SDP/RTP path to bridge against).
			_ = exotel.Record(observability.RecordLog{
				Level:   observability.LevelError,
				Message: "Exotel call transfer is not supported",
				Attributes: observability.Attributes{
					"component":         observability.ComponentCall.String(),
					"provider":          internal_exotel.Provider,
					"stream_id":         exotel.streamID,
					"conversation_uuid": exotel.ChannelUUID,
					"tool_action":       data.GetAction().String(),
					"transfer_to":       data.GetArgs()["transfer_to"],
				},
			}, observability.RecordMetadata{
				Metadata: []*protos.Metadata{
					{Key: observability.MetadataCallStatus, Value: "transfer_failed"},
					{Key: observability.MetadataFailureReason, Value: "transfer not supported for Exotel"},
				},
			}, observability.RecordMetric{
				Metrics: []*protos.Metric{{
					Name:        observability.MetricCallStatus,
					Value:       "FAILED",
					Description: "Exotel call transfer is not supported",
				}},
			})
			exotel.Input(&protos.ConversationToolCallResult{
				Id:     data.GetId(),
				ToolId: data.GetToolId(),
				Name:   data.GetName(),
				Action: data.GetAction(),
				Result: map[string]string{"status": "failed", "reason": "transfer not supported for Exotel", "next_action": "end_call"},
			})
		}
	default:
		_ = exotel.Record(observability.RecordLog{
			Level:   observability.LevelDebug,
			Message: "Exotel Send unknown message type",
			Attributes: observability.Attributes{
				"component":         observability.ComponentCall.String(),
				"provider":          internal_exotel.Provider,
				"stream_id":         exotel.streamID,
				"conversation_uuid": exotel.ChannelUUID,
				"type":              fmt.Sprintf("%T", response),
			},
		})
	}
	return nil
}

func (exotel *exotelWebsocketStreamer) handleStartEvent(mediaEvent internal_exotel.ExotelMediaEvent) {
	exotel.streamID = mediaEvent.StreamSid
}

func (exotel *exotelWebsocketStreamer) handleMediaEvent(mediaEvent internal_exotel.ExotelMediaEvent) error {
	if mediaEvent.Media == nil {
		_ = exotel.Record(observability.RecordLog{
			Level:   observability.LevelDebug,
			Message: "Exotel media event missing media payload",
			Attributes: observability.Attributes{
				"component":         observability.ComponentCall.String(),
				"provider":          internal_exotel.Provider,
				"stream_id":         exotel.streamID,
				"conversation_uuid": exotel.ChannelUUID,
			},
		})
		return nil
	}
	receivedAt := time.Now()
	payloadBytes, err := exotel.Encoder().DecodeString(mediaEvent.Media.Payload)
	if err != nil {
		_ = exotel.Record(observability.RecordLog{
			Level:   observability.LevelError,
			Message: "Failed to decode Exotel media payload",
			Attributes: observability.Attributes{
				"component":         observability.ComponentCall.String(),
				"provider":          internal_exotel.Provider,
				"stream_id":         exotel.streamID,
				"conversation_uuid": exotel.ChannelUUID,
				"error":             err.Error(),
			},
		}, observability.RecordMetric{
			Metrics: []*protos.Metric{{
				Name:        observability.MetricCallStatus,
				Value:       "FAILED",
				Description: "Failed to decode Exotel media payload",
			}},
		})
		return nil
	}

	if exotel.mediaSession == nil {
		return nil
	}
	if err := exotel.mediaSession.HandleProviderAudioFrame(internal_telephony_media.ProviderAudioFrame{
		Audio:      payloadBytes,
		ReceivedAt: receivedAt,
	}); err != nil {
		return err
	}
	return nil
}

func (exotel *exotelWebsocketStreamer) sendExotelMessage(eventType internal_exotel.EventType, mediaData *internal_exotel.ExotelOutboundMedia) error {
	if exotel.streamID == "" {
		return nil
	}
	message := internal_exotel.ExotelOutboundMessage{
		Event:    eventType,
		StreamID: exotel.streamID,
		Media:    mediaData,
	}
	exotelMessageJSON, err := json.Marshal(message)
	if err != nil {
		_ = exotel.Record(observability.RecordLog{
			Level:   observability.LevelError,
			Message: "Failed to marshal Exotel message",
			Attributes: observability.Attributes{
				"component":         observability.ComponentCall.String(),
				"provider":          internal_exotel.Provider,
				"provider_event":    string(eventType),
				"stream_id":         exotel.streamID,
				"conversation_uuid": exotel.ChannelUUID,
				"error":             err.Error(),
			},
		}, observability.RecordMetric{
			Metrics: []*protos.Metric{{
				Name:        observability.MetricCallStatus,
				Value:       "FAILED",
				Description: "Failed to marshal Exotel message",
			}},
		})
		return err
	}
	exotel.writeMu.Lock()
	defer exotel.writeMu.Unlock()
	if exotel.connection == nil {
		return nil
	}
	if err := exotel.connection.WriteMessage(websocket.TextMessage, exotelMessageJSON); err != nil {
		_ = exotel.Record(observability.RecordLog{
			Level:   observability.LevelError,
			Message: "Failed to send message to Exotel",
			Attributes: observability.Attributes{
				"component":         observability.ComponentCall.String(),
				"provider":          internal_exotel.Provider,
				"provider_event":    string(eventType),
				"stream_id":         exotel.streamID,
				"conversation_uuid": exotel.ChannelUUID,
				"error":             err.Error(),
			},
		})
		return err
	}
	return nil
}

func (exotel *exotelWebsocketStreamer) Cancel() error {
	if !exotel.closed.CompareAndSwap(false, true) {
		return nil
	}
	exotel.stopAudioProcessing()
	exotel.writeMu.Lock()
	conn := exotel.connection
	exotel.connection = nil
	exotel.writeMu.Unlock()
	if conn != nil {
		conn.Close()
	}
	exotel.BaseStreamer.Cancel()
	return nil
}

func (exotel *exotelWebsocketStreamer) sendOutputFrame(frame internal_telephony_media.AssistantOutputFrame) error {
	if len(frame.ProviderAudio) == 0 {
		return nil
	}
	return exotel.sendExotelMessage(internal_exotel.EventTypeMedia, &internal_exotel.ExotelOutboundMedia{
		Payload: exotel.Encoder().EncodeToString(frame.ProviderAudio),
	})
}

func (exotel *exotelWebsocketStreamer) stopAudioProcessing() {
	if exotel.mediaSession != nil {
		exotel.mediaSession.Shutdown()
	}
}
