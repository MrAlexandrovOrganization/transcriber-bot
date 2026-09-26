package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mymmrac/telego"

	"transcriber-bot/config"
)

const testToken = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"

type telegramRequest struct {
	method string
	body   map[string]any
}

func fakeTelegram(t *testing.T, handler http.HandlerFunc) (*Bot, <-chan telegramRequest) {
	t.Helper()
	requests := make(chan telegramRequest, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")
		w.Header().Set("Content-Type", "application/json")
		if method == "getMe" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":123456789,"is_bot":true,"first_name":"Test","username":"test_bot"}}`)
			return
		}
		if handler != nil {
			handler(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode %s request: %v", method, err)
		}
		requests <- telegramRequest{method: method, body: body}
		if method == "answerCallbackQuery" {
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		} else {
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"date":1,"chat":{"id":7,"type":"private"}}}`)
		}
	}))
	t.Cleanup(server.Close)
	b, err := New(&config.Config{BotToken: testToken, LocalAPIURL: server.URL + "/", RootID: 7}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b, requests
}

func TestTelegramMessagesAndKeyboards(t *testing.T) {
	b, requests := fakeTelegram(t, nil)
	msg := &telego.Message{MessageID: 10, Chat: telego.Chat{ID: 7}, From: &telego.User{ID: 7}}
	status, err := b.sendInitialStatus(context.Background(), msg, "голосовое")
	if err != nil || status.MessageID != 42 {
		t.Fatalf("initial status: %v, %v", status, err)
	}
	r := <-requests
	if r.method != "sendMessage" || r.body["chat_id"] != float64(7) || r.body["reply_parameters"].(map[string]any)["message_id"] != float64(10) {
		t.Fatalf("unexpected reply: %+v", r)
	}
	b.edit(7, 42, "В очереди", cancelKeyboard("job-1"))
	r = <-requests
	if r.method != "editMessageText" || r.body["message_id"] != float64(42) {
		t.Fatalf("unexpected edit: %+v", r)
	}
	keyboard := r.body["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if keyboard[0].([]any)[0].(map[string]any)["callback_data"] != "cancel:job-1" {
		t.Fatalf("unexpected cancel keyboard: %v", keyboard)
	}
	b.editFinal(7, 42, "Текст <записи> & результат")
	r = <-requests
	if r.body["reply_markup"] != nil || r.body["text"] != "Текст &lt;записи&gt; &amp; результат" || r.body["parse_mode"] != "HTML" {
		t.Fatalf("unexpected final edit: %+v", r)
	}
	b.sendPresetKeyboard(msg)
	r = <-requests
	if r.method != "sendMessage" || r.body["reply_markup"] == nil {
		t.Fatalf("unexpected preset keyboard: %+v", r)
	}
	// Both accessible and inaccessible callback messages expose chat/message IDs
	// through telego's MaybeInaccessibleMessage interface.
	for _, date := range []int{0, 1} {
		var cb telego.CallbackQuery
		data := fmt.Sprintf(`{"id":"cb","from":{"id":7,"first_name":"Test"},"data":"preset:lecture","message":{"message_id":42,"date":%d,"chat":{"id":7,"type":"private"}}}`, date)
		if err := json.Unmarshal([]byte(data), &cb); err != nil {
			t.Fatal(err)
		}
		b.handlePresetCallback(context.Background(), &cb)
		if r = <-requests; r.method != "answerCallbackQuery" {
			t.Fatalf("callback not acknowledged: %+v", r)
		}
		r = <-requests
		if r.method != "editMessageText" || r.body["chat_id"] != float64(7) || r.body["message_id"] != float64(42) {
			t.Fatalf("unexpected callback edit: %+v", r)
		}
		if preset, _ := b.userPreset.Load(int64(7)); preset != "lecture" {
			t.Fatalf("preset = %v", preset)
		}
	}
}

func TestTelegramLectureUpload(t *testing.T) {
	var called atomic.Bool
	b, _ := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if !strings.HasSuffix(r.URL.Path, "/sendDocument") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, header, err := r.FormFile("document")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil || string(data) != "Текст лекции" || header.Filename != "lecture_10.txt" {
			t.Errorf("wrong document: %q, %s, %v", data, header.Filename, err)
		}
		var reply telego.ReplyParameters
		if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply); err != nil {
			t.Error(err)
		}
		if r.FormValue("chat_id") != "7" || reply.MessageID != 10 || reply.ChatID.ID != 7 {
			t.Errorf("wrong reply fields: %v", r.MultipartForm.Value)
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"date":1,"chat":{"id":7,"type":"private"}}}`)
	})
	b.sendAsFile(&telego.Message{MessageID: 10, Chat: telego.Chat{ID: 7}}, "Текст лекции", 10)
	if !called.Load() {
		t.Fatal("document was not sent")
	}
}

func TestTelegramDownload(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("local=%t", local), func(t *testing.T) {
			path := "voice/test.ogg"
			if local {
				path = filepath.Join(t.TempDir(), "voice.ogg")
				if err := os.WriteFile(path, []byte("audio"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			b, _ := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bot" + testToken + "/getFile":
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "voice", "file_path": path, "file_size": 5}})
				case "/file/bot" + testToken + "/voice/test.ogg":
					fmt.Fprint(w, "audio")
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			if !local {
				b.cfg.LocalAPIURL = "" // HTTP download branch, still using the fake API server.
			}
			reader, err := b.downloadFile(context.Background(), "voice", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			data, err := io.ReadAll(reader)
			if err != nil || string(data) != "audio" {
				t.Fatalf("download = %q, %v", data, err)
			}
		})
	}
}

func TestMessageCommand(t *testing.T) {
	for _, text := range []string{"/start", "/start@test_bot", "/start@test_bot payload"} {
		msg := &telego.Message{Text: text, Entities: []telego.MessageEntity{{Type: "bot_command", Offset: 0, Length: len(strings.Fields(text)[0])}}}
		if got := messageCommand(msg); got != "start" {
			t.Errorf("messageCommand(%q) = %q", text, got)
		}
	}
	if got := messageCommand(&telego.Message{Text: "/start"}); got != "" {
		t.Errorf("text without command entity = %q", got)
	}
}
