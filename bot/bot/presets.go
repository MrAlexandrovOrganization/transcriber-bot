package bot

import (
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	pb "transcriber-bot/gen/whisper"
)

type preset struct {
	name  string
	label string
}

var availablePresets = []preset{
	{name: "auto", label: "🤖 Авто"},
	{name: "voice", label: "🎤 Голосовые"},
	{name: "lecture", label: "📚 Лекция"},
	{name: "meeting", label: "🤝 Встреча"},
}

var presetLabels = func() map[string]string {
	m := make(map[string]string, len(availablePresets))
	for _, p := range availablePresets {
		m[p.name] = p.label
	}
	return m
}()

// resolvePreset returns the effective preset for a message.
// If userPreset is "auto" or empty, it auto-detects from the message.
func resolvePreset(userPreset string, msg *tgbotapi.Message) string {
	if userPreset != "" && userPreset != "auto" {
		return userPreset
	}
	// Auto-detect from message type and duration.
	if msg.Voice != nil || msg.VideoNote != nil {
		return "voice"
	}
	if msg.Video != nil && msg.Video.Duration > 300 { // > 5 min → likely a lecture
		return "lecture"
	}
	return "voice"
}

// buildOptions constructs TranscriptionOptions for the given preset name.
func buildOptions(presetName string) *pb.TranscriptionOptions {
	return &pb.TranscriptionOptions{Preset: presetName}
}
