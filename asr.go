package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Voice notes.
//
// A customer sending a voice note gets it transcribed before the turn is built:
// the audio is downloaded, posted to an OpenAI-compatible /audio/transcriptions
// endpoint, and only the text is kept — history, transcripts and the DB never
// hold audio. Transcription is skipped (and the message keeps the non-text
// placeholder) unless transcribe_model is configured.

const (
	voiceMarker = "[Voice]"
	// voiceMaxBytes bounds one download. Voice notes are ~1 MB/minute of opus,
	// so a max_voice_sec note fits with room to spare.
	voiceMaxBytes = 4 << 20
	// maxVoicesQueued keeps a burst from replaying minutes of audio.
	maxVoicesQueued = 2
	// transcribeMaxChars keeps a long transcript from crowding the prompt.
	transcribeMaxChars = 1200
)

// transcribeOn reports whether voice notes are turned into text.
func (b *Bridge) transcribeOn() bool {
	return b.cfg.TranscribeModel != "" && b.cfg.MaxVoiceSec > 0
}

// voiceFileID returns the voice note's file id, or "" when there is none.
func (m *Message) voiceFileID() string {
	if m == nil || m.Voice == nil {
		return ""
	}
	return m.Voice.FileID
}

// messageVoices lists the voice notes of a message worth transcribing.
func (b *Bridge) messageVoices(m *Message) []string {
	if !b.transcribeOn() || m == nil || m.Voice == nil || m.Voice.FileID == "" {
		return nil
	}
	if m.Voice.Duration > b.cfg.MaxVoiceSec || m.Voice.FileSize > voiceMaxBytes {
		logf("INFO", "voice skipped: too long (%ds, %d bytes)", m.Voice.Duration, m.Voice.FileSize)
		return nil
	}
	return []string{m.Voice.FileID}
}

// addVoicesLocked queues voice notes on a chat, keeping the newest ones.
// Caller holds b.mu.
func addVoicesLocked(cs *chatState, ids []string) {
	if len(ids) == 0 {
		return
	}
	cs.voices = append(cs.voices, ids...)
	if over := len(cs.voices) - maxVoicesQueued; over > 0 {
		cs.voices = append([]string(nil), cs.voices[over:]...)
	}
}

// transcribeVoices downloads the queued voice notes and returns the joined
// transcript. Best effort: failures are logged without content and the reply
// still goes out.
func (b *Bridge) transcribeVoices(ctx context.Context, ids []string) string {
	if len(ids) == 0 || !b.transcribeOn() {
		return ""
	}
	d, ok := b.tg.(fileDownloader)
	if !ok {
		return ""
	}
	select {
	case b.asrSem <- struct{}{}:
	case <-ctx.Done():
		return ""
	}
	defer func() { <-b.asrSem }()

	var parts []string
	for _, id := range ids {
		data, err := d.DownloadFile(ctx, id, voiceMaxBytes)
		if err != nil {
			logf("WARN", "voice download skipped: %v", err)
			continue
		}
		text, err := b.transcribeOne(ctx, data)
		if err != nil {
			logf("WARN", "voice transcription failed: %v", err)
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	return truncRunes(strings.Join(parts, " / "), transcribeMaxChars)
}

// transcribeOne posts one audio blob to an OpenAI-compatible transcription
// endpoint. Errors never carry the URL or the key.
func (b *Bridge) transcribeOne(ctx context.Context, audio []byte) (string, error) {
	base := b.cfg.TranscribeBase
	if base == "" {
		base = b.cfg.APIBase
	}
	key := b.cfg.TranscribeKey
	if key == "" {
		key = b.cfg.APIKey
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "voice.ogg")
	if err != nil {
		return "", &LLMError{Kind: "bad_response"}
	}
	if _, err := fw.Write(audio); err != nil {
		return "", &LLMError{Kind: "bad_response"}
	}
	if err := mw.WriteField("model", b.cfg.TranscribeModel); err != nil {
		return "", &LLMError{Kind: "bad_response"}
	}
	if err := mw.Close(); err != nil {
		return "", &LLMError{Kind: "bad_response"}
	}

	wait := time.Duration(b.cfg.LLMTimeoutSec) * time.Second
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/audio/transcriptions", &body)
	if err != nil {
		return "", &LLMError{Kind: "bad_response"}
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := b.media.Do(req)
	if err != nil {
		return "", &LLMError{Kind: netKind(err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused; never log the body,
		// it may echo the request.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", &LLMError{Status: resp.StatusCode, Kind: "http"}
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", &LLMError{Kind: "bad_response"}
	}
	return out.Text, nil
}

// applyVoiceTranscriptLocked replaces the pending voice marker with what the
// customer actually said, so later turns and /tomtat read the real content.
// Caller holds b.mu.
func (b *Bridge) applyVoiceTranscriptLocked(cs *chatState, text string) {
	if text == "" {
		return
	}
	for i := len(cs.msgs) - 1; i >= 0; i-- {
		if cs.msgs[i].Role != "user" {
			continue
		}
		if !strings.Contains(cs.msgs[i].Content, voiceMarker) {
			return
		}
		cs.msgs[i].Content = strings.Replace(cs.msgs[i].Content,
			voiceMarker, fmt.Sprintf("%s «%s»", voiceMarker, text), 1)
		cs.runes = chatRunes(cs.msgs)
		return
	}
}
