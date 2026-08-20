package internal_transformer_sarvam

import (
	"encoding/json"
	"testing"

	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/structpb"
)

func newTestLogger() commons.Logger {
	l, _ := commons.NewApplicationLogger()
	return l
}

func newVaultCredential(m map[string]interface{}) *protos.VaultCredential {
	val, _ := structpb.NewStruct(m)
	return &protos.VaultCredential{Value: val}
}

// --- Constructor Tests ---

func TestNewSarvamOption_ValidCredentials(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "test-api-key"})
	opt, err := NewSarvamOption(newTestLogger(), cred, utils.Option{})
	assert.NoError(t, err)
	assert.NotNil(t, opt)
	assert.Equal(t, "test-api-key", opt.GetKey())
}

func TestNewSarvamOption_MissingKey(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"other": "value"})
	opt, err := NewSarvamOption(newTestLogger(), cred, utils.Option{})
	assert.Error(t, err)
	assert.Nil(t, opt)
	assert.Contains(t, err.Error(), "missing 'key'")
}

func TestNewSarvamOption_EmptyVault(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{})
	opt, err := NewSarvamOption(newTestLogger(), cred, utils.Option{})
	assert.Error(t, err)
	assert.Nil(t, opt)
}

// --- configureTextToSpeech Tests ---

func TestConfigureTextToSpeech_Defaults(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opt, _ := NewSarvamOption(newTestLogger(), cred, utils.Option{})
	config := opt.configureTextToSpeech()

	assert.Equal(t, "config", config["type"])

	data, ok := config["data"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "en-IN", data["target_language_code"])
	assert.Equal(t, "anushka", data["speaker"])
	assert.Equal(t, 16000, data["speech_sample_rate"])
	assert.Equal(t, "linear16", data["output_audio_codec"])
}

func TestConfigureTextToSpeech_WithLanguageOverride(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opts := utils.Option{
		"speak.language": "hi-IN",
	}
	opt, _ := NewSarvamOption(newTestLogger(), cred, opts)
	config := opt.configureTextToSpeech()

	data := config["data"].(map[string]interface{})
	assert.Equal(t, "hi-IN", data["target_language_code"])
	assert.Equal(t, "anushka", data["speaker"]) // default speaker unchanged
	assert.Equal(t, 16000, data["speech_sample_rate"])
	assert.Equal(t, "linear16", data["output_audio_codec"])
}

func TestConfigureTextToSpeech_WithSpeakerOverride(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opts := utils.Option{
		"speak.voice.id": "meera",
	}
	opt, _ := NewSarvamOption(newTestLogger(), cred, opts)
	config := opt.configureTextToSpeech()

	data := config["data"].(map[string]interface{})
	assert.Equal(t, "meera", data["speaker"])
	assert.Equal(t, "en-IN", data["target_language_code"]) // default language unchanged
}

func TestConfigureTextToSpeech_WithAllOverrides(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opts := utils.Option{
		"speak.language": "ta-IN",
		"speak.voice.id": "meera",
	}
	opt, _ := NewSarvamOption(newTestLogger(), cred, opts)
	config := opt.configureTextToSpeech()

	data := config["data"].(map[string]interface{})
	assert.Equal(t, "ta-IN", data["target_language_code"])
	assert.Equal(t, "meera", data["speaker"])
	assert.Equal(t, 16000, data["speech_sample_rate"])
	assert.Equal(t, "linear16", data["output_audio_codec"])
}

// --- speechToTextMessage Tests ---

func TestSpeechToTextMessage_ValidInput(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opt, _ := NewSarvamOption(newTestLogger(), cred, utils.Option{})

	input := []byte("hello audio data")
	result, err := opt.speechToTextMessage(input)
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Parse the JSON output
	var payload map[string]interface{}
	err = json.Unmarshal(result, &payload)
	assert.NoError(t, err)

	audio, ok := payload["audio"].(map[string]interface{})
	assert.True(t, ok)
	assert.NotEmpty(t, audio["data"])
	assert.Equal(t, float64(16000), audio["sample_rate"])
	assert.Equal(t, "audio/wav", audio["encoding"])
	assert.Equal(t, "pcm_s16le", audio["input_audio_codec"])
}

func TestSpeechToTextMessage_EmptyInput(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opt, _ := NewSarvamOption(newTestLogger(), cred, utils.Option{})

	result, err := opt.speechToTextMessage([]byte{})
	assert.NoError(t, err)
	assert.NotNil(t, result)

	var payload map[string]interface{}
	err = json.Unmarshal(result, &payload)
	assert.NoError(t, err)

	audio := payload["audio"].(map[string]interface{})
	assert.Equal(t, "", audio["data"]) // base64 of empty = ""
}

// --- URL Tests ---

func TestTextToSpeechUrl_Default(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opt, _ := NewSarvamOption(newTestLogger(), cred, utils.Option{})
	url := opt.textToSpeechUrl()

	assert.Contains(t, url, TEXT_TO_SPEECH_URL)
	assert.Contains(t, url, "send_completion_event=true") // required for handleFlushComplete
	assert.NotContains(t, url, "model=")
}

func TestTextToSpeechUrl_WithModel(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opts := utils.Option{
		"speak.model": "bulbul:v2",
	}
	opt, _ := NewSarvamOption(newTestLogger(), cred, opts)
	url := opt.textToSpeechUrl()

	assert.Contains(t, url, TEXT_TO_SPEECH_URL)
	assert.Contains(t, url, "send_completion_event=true")
	assert.Contains(t, url, "model=bulbul")
}

func TestSpeechToTextUrl_Default(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opt, _ := NewSarvamOption(newTestLogger(), cred, utils.Option{})
	url := opt.speechToTextUrl()

	assert.Contains(t, url, SPEECH_TO_TEXT_URL)
	assert.Contains(t, url, "sample_rate=16000")
	assert.Contains(t, url, "input_audio_codec=pcm_s16le")
	assert.NotContains(t, url, "language-code=")
	assert.NotContains(t, url, "model=")
}

func TestSpeechToTextUrl_WithLanguageAndModel(t *testing.T) {
	cred := newVaultCredential(map[string]interface{}{"key": "k"})
	opts := utils.Option{
		"listen.language": "hi-IN",
		"listen.model":    "saaras:v2",
	}
	opt, _ := NewSarvamOption(newTestLogger(), cred, opts)
	url := opt.speechToTextUrl()

	assert.Contains(t, url, SPEECH_TO_TEXT_URL)
	assert.Contains(t, url, "sample_rate=16000")
	assert.Contains(t, url, "input_audio_codec=pcm_s16le")
	assert.Contains(t, url, "language-code=hi-IN")
	assert.Contains(t, url, "model=saaras")
}
