package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// asrTG adds file downloads to the fake Telegram used by the harness.
type asrTG struct {
	*fakeTG
	audio map[string][]byte
}

func (a *asrTG) DownloadFile(ctx context.Context, fileID string, max int64) ([]byte, error) {
	data, ok := a.audio[fileID]
	if !ok {
		return nil, &NetError{Method: "getFile", Kind: "no_path"}
	}
	if int64(len(data)) > max {
		return nil, &NetError{Method: "getFile", Kind: "too_large"}
	}
	return data, nil
}

func voiceUpdate(chat, from int64, fileID string, seconds int) Update {
	u := biz(testConn, chat, from, "")
	u.BusinessMessage.Voice = &Voice{FileID: fileID, Duration: seconds, MimeType: "audio/ogg"}
	return u
}

func TestVoiceNoteIsTranscribed(t *testing.T) {
	var gotModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("posted to %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("bad multipart: %v", err)
		}
		gotModel = r.FormValue("model")
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"text":"vàng đang lên, có nên mua không"}`)
	}))
	defer srv.Close()

	h := newHarness(func(c *Config) {
		c.DebounceMS = 0
		c.TranscribeModel = "whisper-1"
		c.APIBase = srv.URL
		c.APIKey = "test-key"
	})
	h.b.tg = &asrTG{fakeTG: h.tg, audio: map[string][]byte{"voice-1": []byte("OGG")}}

	h.b.handleUpdate(voiceUpdate(670, 670, "voice-1", 12))
	waitSent(t, h, 1)

	if gotModel != "whisper-1" {
		t.Fatalf("model field = %q", gotModel)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	joined := ""
	for _, m := range h.llm.seen[0] {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "vàng đang lên") {
		t.Fatalf("model never saw the transcript: %q", joined)
	}
	// The transcript is kept in history so later turns read it too (no audio).
	h.b.mu.Lock()
	hist := ""
	for _, m := range h.b.chats["b:"+testConn+":670"].msgs {
		hist += m.Content + "\n"
	}
	h.b.mu.Unlock()
	if !strings.Contains(hist, "vàng đang lên") {
		t.Fatalf("history lost the transcript: %q", hist)
	}
	if strings.Contains(hist, "OGG") {
		t.Fatal("audio must never be stored")
	}
}

func TestVoiceSkippedWhenTranscriptionOff(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"text":"x"}`)
	}))
	defer srv.Close()

	h := newHarness(func(c *Config) { c.DebounceMS = 0; c.APIBase = srv.URL })
	h.b.tg = &asrTG{fakeTG: h.tg, audio: map[string][]byte{"voice-2": []byte("OGG")}}

	h.b.handleUpdate(voiceUpdate(671, 671, "voice-2", 5))
	waitSent(t, h, 1)
	if calls != 0 {
		t.Fatalf("transcription must stay off without transcribe_model, got %d calls", calls)
	}
	if joined := h.llm.seen[0][len(h.llm.seen[0])-1].Content; !strings.Contains(joined, "không phải văn bản") {
		t.Fatalf("expected the non-text placeholder, got %q", joined)
	}
}

func TestLongVoiceIsSkipped(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0; c.TranscribeModel = "whisper-1"; c.MaxVoiceSec = 30 })
	h.b.tg = &asrTG{fakeTG: h.tg, audio: map[string][]byte{"voice-3": []byte("OGG")}}

	h.b.handleUpdate(voiceUpdate(672, 672, "voice-3", 90))
	waitSent(t, h, 1)
	if n := len(h.b.chats["b:"+testConn+":672"].voices); n != 0 {
		t.Fatalf("an over-long note must not be queued, got %d", n)
	}
}

func TestTranscriptionFailureStillReplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newHarness(func(c *Config) {
		c.DebounceMS = 0
		c.TranscribeModel = "whisper-1"
		c.APIBase = srv.URL
	})
	h.b.tg = &asrTG{fakeTG: h.tg, audio: map[string][]byte{"voice-4": []byte("OGG")}}

	h.b.handleUpdate(voiceUpdate(673, 673, "voice-4", 8))
	waitSent(t, h, 1)
	if got := len(h.tg.sends()); got != 1 {
		t.Fatalf("a failed transcription must still answer once, got %d", got)
	}
}
