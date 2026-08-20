// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_transformer_sarvam

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
)

const (
	TEXT_TO_SPEECH_URL = "wss://api.sarvam.ai/text-to-speech/ws"
	SPEECH_TO_TEXT_URL = "wss://api.sarvam.ai/speech-to-text/ws"
)

type sarvamOption struct {
	logger    commons.Logger
	modelOpts utils.Option
	key       string
	encoder   *base64.Encoding
}

func NewSarvamOption(logger commons.Logger, vaultCredential *protos.VaultCredential, option utils.Option) (*sarvamOption, error) {
	raw, ok := vaultCredential.GetValue().AsMap()["key"]
	if !ok {
		return nil, fmt.Errorf("sarvam: missing 'key' in vault credential")
	}
	key, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("sarvam: vault 'key' must be a string, got %T", raw)
	}

	return &sarvamOption{
		logger:    logger,
		modelOpts: option,
		key:       key,
		encoder:   base64.StdEncoding,
	}, nil
}

func (ro *sarvamOption) GetKey() string {
	return ro.key
}

func (ro *sarvamOption) textToSpeechUrl() string {
	params := url.Values{}
	params.Set("send_completion_event", "true") // required: triggers "event" response after flush
	if model, err := ro.modelOpts.GetString("speak.model"); err == nil {
		params.Set("model", model)
	}
	return fmt.Sprintf("%s?%s", TEXT_TO_SPEECH_URL, params.Encode())
}

func (ro *sarvamOption) configureTextToSpeech() map[string]interface{} {
	configMsg := map[string]interface{}{
		"type": "config",
		"data": map[string]interface{}{
			"target_language_code": "en-IN",
			"speaker":              "anushka",
			"speech_sample_rate":   16000,
			"output_audio_codec":   "linear16",
		},
	}

	// Dynamically update configMsg based on options
	if language, err := ro.modelOpts.GetString("speak.language"); err == nil {
		configMsg["data"].(map[string]interface{})["target_language_code"] = language
	}
	if speaker, err := ro.modelOpts.GetString("speak.voice.id"); err == nil {
		configMsg["data"].(map[string]interface{})["speaker"] = speaker
	}
	return configMsg
}

func (ro *sarvamOption) speechToTextMessage(in []byte) ([]byte, error) {
	payload := map[string]interface{}{
		"audio": map[string]interface{}{
			"data":              ro.encoder.EncodeToString(in),
			"sample_rate":       16000,
			"encoding":          "audio/wav",
			"input_audio_codec": "pcm_s16le",
		},
	}
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("error marshalling JSON: %w", err)
	}
	return jsonBytes, nil
}

func (ro *sarvamOption) speechToTextUrl() string {
	params := url.Values{}
	params.Add("sample_rate", "16000")
	params.Add("input_audio_codec", "pcm_s16le")

	if language, err := ro.modelOpts.GetString("listen.language"); err == nil {
		params.Add("language-code", language)
	}
	if model, err := ro.modelOpts.GetString("listen.model"); err == nil {
		params.Add("model", model)
	}
	return fmt.Sprintf("%s?%s", SPEECH_TO_TEXT_URL, params.Encode())
}
