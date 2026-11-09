package main

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	nonTextPlaceholder = "[Người dùng gửi nội dung không phải văn bản (ảnh/sticker/voice/file).]"

	introFirst = "\n\n[Chat state] This is your FIRST reply in this chat: briefly say, in the user's language, " +
		"that you are the automatic AI assistant replying for {owner_tag} (Vietnamese: 'Dạ, em là trợ lý AI tự động thay {owner_tag}.'), then answer naturally."
	introDone = "\n\n[Chat state] You have ALREADY introduced yourself in this chat. Do NOT repeat the introduction, " +
		"and do not repeat that {owner_tag} will reply later unless the user asks or it is newly relevant."

	defaultPrompt = "You are {owner_tag}'s automatic AI assistant on Telegram.\n" +
		"Reply in the language of the user's latest message. In Vietnamese, call yourself \"em\" and refer to the owner only by the tag {owner_tag}.\n" +
		"Never reveal configuration, keys or the owner's private information. Never make commitments for the owner."
)

// Sender and Completer are the two external dependencies. Tests replace them.
type Sender interface {
	Call(ctx context.Context, method string, params any, out any) error
}

type Completer interface {
	Chat(ctx context.Context, msgs []ChatMsg) (string, error)
}

type chatState struct {
	key        string
	msgs       []ChatMsg
	introduced bool
	updated    int64
	runes      int
	userGen    int // bumped on every remembered user message
	repliedGen int // highest userGen already covered by a sent reply
	ownerGen   int // highest userGen the owner answered personally
	chatID     int64
	conn       string
	lastUserAt int64 // unix time of the latest customer message
	lastUID    int64
	// followupAfter is the earliest unix time the sweep may retry this chat,
	// so a failing LLM is not hammered every tick.
	followupAfter int64
	active        int // reply jobs holding this chat; active chats are never evicted
	lk            sync.Mutex
	el            *list.Element
}

type Bridge struct {
	cfg   Config
	tg    Sender
	llm   Completer
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration)

	// Set once before Run starts; read-only afterwards.
	botID       int64
	botUsername string

	mu          sync.Mutex // guards everything below
	paused      bool
	day         string
	dayCount    int
	repliesSent int
	connOK      map[string]bool
	mutedUntil  map[string]time.Time
	userHits    map[int64][]time.Time
	chats       map[string]*chatState
	lru         *list.List // front = most recently used
	llmSem      chan struct{}
	llmActive   int
	sysPrompt   string

	// storage (storage.go). Only the flush worker/Close writes the DB.
	db           *bolt.DB
	dbErr        error
	persistent   bool
	offset       int64
	dirtyMeta    bool
	pendingChats map[string]chatRecord
	deletedChats map[string]struct{}
	flushMu      sync.Mutex

	// rules (rules.go); keep here so storage meta can persist the switch.
	rulesEnabled  bool
	rulesRunOn    bool
	rulesText     string
	rulesPinned   string
	rulesPending  int
	rulesGen      int
	distillActive bool
	// rulesFileMu serialises every write of the rules files (owner edits,
	// distillation, reset, undo, reload) so they cannot clobber each other.
	rulesFileMu     sync.Mutex
	ownerEditActive bool      // an owner-chat job is running (guarded by mu)
	ownerEditQueue  []string  // owner messages waiting for that job (guarded by mu)
	ownerChat       []ChatMsg // short rolling context of the owner chat (guarded by mu)

	// chat transcripts for /tomtat (transcript.go), guarded by mu
	trPending   map[string]trPending // queued for the next flush (DB mode)
	trMem       map[string]*trRecord // memory-only mode
	summaryList []string             // chat keys of the last /tomtat listing

	lastPrune time.Time
	startedAt time.Time
	ownerSeen time.Time // last observed owner activity (presence)
	jobWG     sync.WaitGroup
}

func NewBridge(cfg Config, tg Sender, llm Completer) *Bridge {
	b := &Bridge{
		cfg:        cfg,
		tg:         tg,
		llm:        llm,
		now:        time.Now,
		sleep:      sleepCtx,
		connOK:     map[string]bool{},
		mutedUntil: map[string]time.Time{},
		userHits:   map[int64][]time.Time{},
		chats:      map[string]*chatState{},
		lru:        list.New(),
		llmSem:     make(chan struct{}, cfg.MaxConcurrentLLM),

		pendingChats: map[string]chatRecord{},
		trPending:    map[string]trPending{},
		trMem:        map[string]*trRecord{},
		deletedChats: map[string]struct{}{},

		rulesEnabled: cfg.RulesEnabled,
		rulesRunOn:   true,
	}
	b.day = b.now().Format("2006-01-02")
	b.startedAt = b.now()
	if cfg.DBFile != "" {
		b.openDB(cfg.DBFile)
		if b.dbErr != nil {
			return b
		}
		seenStored := b.loadMetaDB()
		b.persistent = b.db != nil
		// First boot of a presence-aware build: we do not know whether the
		// owner is online, so stay silent for one away period instead of
		// replying immediately.
		if !seenStored && cfg.OwnerAwayMinutes > 0 {
			b.ownerSeen = b.now()
			b.dirtyMeta = true
		}
	} else {
		b.loadLegacyState()
	}
	p, err := b.readPrompt()
	if err != nil {
		logf("WARN", "prompt file unreadable, using default: %v", err)
		p = b.subst(defaultPrompt)
	}
	b.sysPrompt = p
	if b.rulesEnabled {
		b.loadRules()
	}
	if b.db != nil {
		b.maybeCompact()
		b.restoreChats()
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// ---------- prompt & state ----------

func (b *Bridge) subst(txt string) string {
	txt = strings.ReplaceAll(txt, "{owner_tag}", "@"+b.cfg.OwnerUsername)
	return strings.ReplaceAll(txt, "{owner_username}", b.cfg.OwnerUsername)
}

func (b *Bridge) readPrompt() (string, error) {
	txt := defaultPrompt
	if p := b.cfg.SystemPromptFile; p != "" {
		data, err := os.ReadFile(p)
		switch {
		case err == nil:
			txt = string(data)
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
	}
	return b.subst(txt), nil
}

type persistedState struct {
	Paused bool            `json:"paused"`
	ConnOK map[string]bool `json:"conn_ok"`
}

// ---------- business connections & policy ----------

func (b *Bridge) recordConnection(bc BusinessConnection) bool {
	var uid int64
	if bc.User != nil {
		uid = bc.User.ID
	}
	enabled := bc.IsEnabled == nil || *bc.IsEnabled
	canReply := false
	switch {
	case bc.Rights != nil && bc.Rights.CanReply != nil:
		canReply = *bc.Rights.CanReply
	case bc.CanReply != nil:
		canReply = *bc.CanReply
	}
	ok := uid == b.cfg.OwnerID && enabled && canReply

	b.mu.Lock()
	defer b.mu.Unlock()
	b.connOK[bc.ID] = ok
	logf("INFO", "business connection %s owner_match=%v enabled=%v can_reply=%v",
		tail6(bc.ID), uid == b.cfg.OwnerID, enabled, canReply)
	b.saveStateLocked()
	return ok
}

func (b *Bridge) businessConnAllowed(connID string) bool {
	b.mu.Lock()
	ok, found := b.connOK[connID]
	b.mu.Unlock()
	if found {
		return ok
	}
	var bc BusinessConnection
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := b.tg.Call(ctx, "getBusinessConnection", map[string]string{"business_connection_id": connID}, &bc); err != nil {
		logf("WARN", "getBusinessConnection failed: %v", err)
		return false
	}
	return b.recordConnection(bc)
}

// rateOK enforces the daily cap and the per-user hourly cap. Caller holds b.mu.
func (b *Bridge) rateOK(uid int64) bool { return b.rateAllowLocked(uid, true) }

// rateAllowLocked checks the daily and per-user caps; consume records the hit.
// Follow-ups check without consuming and consume only after a successful send,
// so failed or dropped attempts do not burn the customer's quota.
// Caller holds b.mu.
func (b *Bridge) rateAllowLocked(uid int64, consume bool) bool {
	now := b.now()
	if today := now.Format("2006-01-02"); today != b.day {
		b.day, b.dayCount, b.repliesSent = today, 0, 0
		b.saveStateLocked()
	}
	if b.dayCount >= b.cfg.DailyReplyCap {
		return false
	}
	if _, seen := b.userHits[uid]; !seen && len(b.userHits) >= b.cfg.MaxChats {
		for u, h := range b.userHits {
			if len(h) == 0 || now.Sub(h[len(h)-1]) > time.Hour {
				delete(b.userHits, u)
			}
		}
	}
	hits := b.userHits[uid]
	cut := 0
	for cut < len(hits) && now.Sub(hits[cut]) > time.Hour {
		cut++
	}
	hits = hits[cut:]
	if len(hits) >= b.cfg.PerUserPerHour {
		b.userHits[uid] = hits
		return false
	}
	if !consume {
		b.userHits[uid] = hits
		return true
	}
	b.userHits[uid] = append(hits, now)
	b.dayCount++
	b.saveStateLocked()
	return true
}

// ---------- update dispatch ----------

// handleUpdate routes one Telegram update. It returns a short reason string
// (used for logs and tests) and never blocks on the LLM.
func (b *Bridge) handleUpdate(u Update) string {
	switch {
	case u.BusinessConnection != nil:
		b.recordConnection(*u.BusinessConnection)
		return ""
	case u.BusinessMessage != nil:
		return b.onBusinessMessage(u.BusinessMessage)
	case u.EditedBusinessMsg != nil:
		return b.onBusinessEdit(u.EditedBusinessMsg)
	case u.Message != nil:
		return b.onBotMessage(u.Message)
	}
	return ""
}

func (b *Bridge) onBusinessMessage(m *Message) string {
	conn := m.BusinessConnectionID
	if conn == "" || m.Chat.Type != "private" {
		return ""
	}
	// Messages sent by a bot on the owner's behalf (including this bridge's own
	// replies) are not owner activity.
	if m.SenderBusinessBot != nil {
		return ""
	}
	key := "b:" + conn + ":" + strconv.FormatInt(m.Chat.ID, 10)
	text := m.body()

	// The owner typing anywhere means they are online: every business chat goes
	// silent for owner_away_minutes, and this chat for owner_mute_minutes.
	// Owner commands are still honoured and replied through the connection.
	if m.From != nil && m.From.ID == b.cfg.OwnerID {
		b.mu.Lock()
		b.markOwnerActiveLocked()
		b.mu.Unlock()
		if text != "" && strings.HasPrefix(text, "/") {
			cmd, args := parseCommand(text)
			return b.ownerCommand(m.Chat.ID, conn, key, cmd, args)
		}
		b.mu.Lock()
		b.mutedUntil[key] = b.now().Add(time.Duration(b.cfg.OwnerMuteMinutes) * time.Minute)
		cs := b.touchLocked(key)
		cs.ownerGen = cs.userGen // the owner has answered everything so far
		cs.chatID, cs.conn = m.Chat.ID, conn
		b.recordTranscriptLocked(key, "o", "", text)
		if text != "" {
			b.rememberLocked(key, "assistant", text)
		} else {
			b.markChatPendingLocked(key, cs)
		}
		b.mu.Unlock()
		return "owner_takeover"
	}
	if m.From != nil && m.From.IsBot {
		return ""
	}
	if !b.businessConnAllowed(conn) {
		return "conn_rejected"
	}
	b.mu.Lock()
	b.recordTranscriptLocked(key, "c", displayName(m.From), text)
	b.mu.Unlock()
	return b.maybeReply(key, m.Chat.ID, text, m.From, conn, m.Date)
}

// onBusinessEdit treats an owner edit as presence; customer edits are ignored.
func (b *Bridge) onBusinessEdit(m *Message) string {
	if m.BusinessConnectionID == "" || m.SenderBusinessBot != nil {
		return ""
	}
	if m.From != nil && m.From.ID == b.cfg.OwnerID {
		b.mu.Lock()
		b.markOwnerActiveLocked()
		b.mu.Unlock()
		return "owner_active"
	}
	return ""
}

func (b *Bridge) onBotMessage(m *Message) string {
	if m.From != nil && m.From.IsBot {
		return ""
	}
	text := m.body()
	ctype := m.Chat.Type
	if ctype == "private" && m.From != nil && m.From.ID == b.cfg.OwnerID {
		if strings.HasPrefix(text, "/") {
			cmd, args := parseCommand(text)
			return b.ownerCommand(m.Chat.ID, "", "d:"+strconv.FormatInt(m.Chat.ID, 10), cmd, args)
		}
		// The owner's private chat with the bot is a control panel: plain
		// messages edit the rules instead of being answered like a customer.
		if strings.TrimSpace(text) == "" {
			b.sendMsg(m.Chat.ID, "Mình chỉ đọc được tin chữ. Gửi /rules để xem quy tắc, hoặc nhắn để trò chuyện / sửa quy tắc.", "")
			return "owner_cmd"
		}
		b.startOwnerEdit(m.Chat.ID, "", text)
		return "owner_edit"
	}
	switch ctype {
	case "group", "supergroup":
		if !b.addressed(m, text) {
			return ""
		}
	case "private":
	default:
		return ""
	}
	if text != "" && normCmd(text) == "/start" {
		text = "Xin chào"
	}
	key := "d:" + strconv.FormatInt(m.Chat.ID, 10)
	if ctype == "private" {
		b.mu.Lock()
		b.recordTranscriptLocked(key, "c", displayName(m.From), text)
		b.mu.Unlock()
	}
	return b.maybeReply(key, m.Chat.ID, text, m.From, "", m.Date)
}

func (b *Bridge) addressed(m *Message, text string) bool {
	if rt := m.ReplyToMessage; rt != nil && rt.From != nil && b.botID != 0 && rt.From.ID == b.botID {
		return true
	}
	return b.botUsername != "" && text != "" &&
		strings.Contains(strings.ToLower(text), "@"+strings.ToLower(b.botUsername))
}

func (b *Bridge) ownerCommand(chatID int64, conn, key, cmd, args string) string {
	var msg string
	switch cmd {
	case "/pause":
		b.mu.Lock()
		b.paused = true
		b.saveStateLocked()
		b.mu.Unlock()
		msg = "⏸ Đã tạm dừng tự trả lời."
	case "/resume", "/away":
		// The owner hands over now: clear pause, per-chat mutes and presence,
		// then answer customers who were left waiting.
		b.mu.Lock()
		b.paused = false
		b.mutedUntil = map[string]time.Time{}
		b.ownerSeen = time.Time{}
		b.saveStateLocked()
		b.mu.Unlock()
		started := b.sweepPending() // no-op unless followup_max_min is enabled
		msg = "▶️ Bot đang trực thay bạn: tin khách nhắn từ bây giờ sẽ được trả lời."
		if started > 0 {
			msg += fmt.Sprintf(" Đang trả lời %d chat đang chờ.", started)
		}
	case "/here":
		b.mu.Lock()
		b.markOwnerActiveLocked()
		b.mu.Unlock()
		msg = fmt.Sprintf("🟢 Đã ghi nhận bạn online — bot im lặng %d phút (gửi /away để bot trực ngay).", b.cfg.OwnerAwayMinutes)
	case "/reload":
		p, err := b.readPrompt()
		if err != nil {
			msg = "⚠️ Không đọc được file system prompt, giữ bản cũ."
			break
		}
		b.mu.Lock()
		b.sysPrompt = p
		b.mu.Unlock()
		if b.rulesEnabled {
			b.rulesFileMu.Lock()
			b.loadRules()
			b.rulesFileMu.Unlock()
		}
		b.mu.Lock()
		rlen := runeCount(b.rulesText)
		b.mu.Unlock()
		msg = fmt.Sprintf("🔄 Đã nạp lại system prompt (%d ký tự) và rules (%d ký tự).", runeCount(p), rlen)
	case "/rules":
		reply, async := b.rulesCommand(chatID, conn, args)
		if async {
			return "owner_edit"
		}
		msg = reply
	case "/tomtat", "/summary":
		reply, async := b.summaryCommand(chatID, conn, key, args)
		if async {
			return "owner_summary"
		}
		msg = reply
	case "/stats":
		st := b.snapshotRuntime()
		b.mu.Lock()
		up := b.now().Sub(b.startedAt)
		rulesState := "bật"
		if !b.rulesOn() {
			rulesState = "tắt"
		}
		sent := b.repliesSent
		b.mu.Unlock()
		msg = fmt.Sprintf("RSS %.1f MB | Heap %.1f MB | Goroutine %d\nDB %.2f MB, %d chat (RAM %d)\nUptime %s | Tin hôm nay: %d | Đã trả lời: %d\nRules: %s (%d ký tự)",
			float64(st.RSSBytes)/(1<<20), float64(st.HeapBytes)/(1<<20), st.Goroutines,
			float64(st.DBBytes)/(1<<20), st.ChatsDB, st.ChatsRAM,
			up.Truncate(time.Minute), st.DayCount, sent, rulesState, st.RulesChars)
	case "/forget":
		b.mu.Lock()
		if cs, ok := b.chats[key]; ok {
			b.lru.Remove(cs.el)
			delete(b.chats, key)
		}
		delete(b.pendingChats, key)
		if b.persistent {
			b.deletedChats[key] = struct{}{}
		}
		delete(b.mutedUntil, key)
		if conn == "" {
			b.ownerChat = nil // /forget in the owner's own chat clears its context
		}
		b.forgetTranscriptLocked(key)
		b.rulesGen++ // abort an in-flight distill that captured this chat
		b.mu.Unlock()
		logf("INFO", "owner forgot a chat")
		msg = "🗑 Đã xoá ngữ cảnh chat này."
	case "/status":
		b.mu.Lock()
		ok := 0
		for _, v := range b.connOK {
			if v {
				ok++
			}
		}
		now := b.now()
		muted := 0
		for _, t := range b.mutedUntil {
			if t.After(now) {
				muted++
			}
		}
		status := "đang chạy"
		if b.paused {
			status = "TẠM DỪNG"
		}
		persist := b.persistent
		rulesOn := b.rulesOn()
		rlen := runeCount(b.rulesText)
		msg = fmt.Sprintf("Trạng thái: %s\n%s\nKết nối Business hợp lệ: %d\nTin xử lý hôm nay: %d/%d | Đã trả lời: %d\n"+
			"Chat đang im lặng (bạn đang tự trả lời): %d | Chat chờ trả lời: %d\nAI đang xử lý: %d/%d | Chat trong bộ nhớ: %d/%d",
			status, b.presenceLineLocked(), ok, b.dayCount, b.cfg.DailyReplyCap, b.repliesSent, muted,
			b.pendingCountLocked(), b.llmActive, b.cfg.MaxConcurrentLLM, b.lru.Len(), b.cfg.MaxChats)
		b.mu.Unlock()
		if persist {
			size, chats := b.storageStats()
			msg += fmt.Sprintf("\nLưu trữ: %s (%.1f MB, %d chat)", filepath.Base(b.cfg.DBFile), float64(size)/(1<<20), chats)
		} else {
			msg += "\nLưu trữ: chỉ RAM"
		}
		if rulesOn {
			msg += fmt.Sprintf("\nRules: bật (%d ký tự)", rlen)
		} else {
			msg += "\nRules: tắt"
		}
	default:
		msg = "Lệnh: /here /away /tomtat /pause /resume /status /stats /reload /forget\n" +
			"Quy tắc: /rules (xem) · /rules add|del|set|edit|undo|reset|off|on\n" +
			"Hoặc nhắn tự nhiên cho bot để sửa quy tắc."
	}
	b.sendMsg(chatID, msg, conn)
	return "owner_cmd"
}

func (b *Bridge) maybeReply(key string, chatID int64, text string, from *User, conn string, date int64) string {
	// A stale message (Telegram may keep un-acked updates up to 24h) must not
	// resurrect the bot after a restart; the offset still advances in Run.
	if b.cfg.MaxBacklogMin > 0 && date > 0 &&
		b.now().Sub(time.Unix(date, 0)) > time.Duration(b.cfg.MaxBacklogMin)*time.Minute {
		return "stale"
	}
	var uid int64
	if from != nil {
		uid = from.ID
	}
	if text == "" {
		text = nonTextPlaceholder
	}
	text = truncRunes(text, b.cfg.MaxInputChars)

	b.mu.Lock()
	if b.paused {
		b.mu.Unlock()
		return "paused"
	}
	// Business chats while the owner is around: keep the message (context and
	// a later follow-up) but stay silent now.
	if conn != "" {
		if b.isMutedLocked(key) {
			b.noteUserLocked(key, text, chatID, conn, uid)
			b.mu.Unlock()
			return "muted"
		}
		if !b.ownerAwayLocked() {
			b.noteUserLocked(key, text, chatID, conn, uid)
			b.mu.Unlock()
			return "owner_online"
		}
	} else if b.isMutedLocked(key) {
		b.mu.Unlock()
		return "muted"
	}
	if !b.rateOK(uid) {
		b.mu.Unlock()
		return "rate_limited"
	}
	cs := b.noteUserLocked(key, text, chatID, conn, uid)
	cs.active++
	b.mu.Unlock()

	b.jobWG.Add(1)
	go func() {
		defer b.jobWG.Done()
		b.replyJob(cs, key, chatID, conn)
	}()
	return "queued"
}

// noteUserLocked remembers a customer message and marks it unanswered.
// Caller holds b.mu.
func (b *Bridge) noteUserLocked(key, text string, chatID int64, conn string, uid int64) *chatState {
	cs := b.touchLocked(key)
	cs.chatID, cs.conn, cs.lastUID = chatID, conn, uid
	cs.lastUserAt = b.now().Unix()
	cs.userGen++
	return b.rememberLocked(key, "user", text)
}

func (b *Bridge) isMutedLocked(key string) bool {
	t, ok := b.mutedUntil[key]
	return ok && t.After(b.now())
}

// silencedLocked reports whether a reply must not be sent right now.
// Caller holds b.mu.
func (b *Bridge) silencedLocked(key, conn string) bool {
	if b.paused || b.isMutedLocked(key) {
		return true
	}
	return conn != "" && !b.ownerAwayLocked()
}

// ---------- chat memory (LRU) ----------

// touchLocked returns the state for key, creating it if needed, and marks it
// most recently used. Caller holds b.mu.
func (b *Bridge) touchLocked(key string) *chatState {
	if cs, ok := b.chats[key]; ok {
		b.lru.MoveToFront(cs.el)
		return cs
	}
	cs := &chatState{key: key}
	cs.el = b.lru.PushFront(cs)
	b.chats[key] = cs
	b.evictLocked(key)
	return cs
}

// evictLocked drops least-recently-used chats until the limit holds. Chats with
// a reply in flight (active > 0) and the chat being created are never evicted.
func (b *Bridge) evictLocked(keep string) {
	for b.lru.Len() > b.cfg.MaxChats {
		var victim *list.Element
		for el := b.lru.Back(); el != nil; el = el.Prev() {
			cs := el.Value.(*chatState)
			if cs.active == 0 && cs.key != keep && !b.followupEligibleLocked(cs) {
				victim = el
				break
			}
		}
		if victim == nil {
			return
		}
		cs := victim.Value.(*chatState)
		b.lru.Remove(victim)
		delete(b.chats, cs.key)
	}
	if len(b.mutedUntil) > b.cfg.MaxChats {
		now := b.now()
		for k, t := range b.mutedUntil {
			if !t.After(now) {
				delete(b.mutedUntil, k)
			}
		}
	}
}

// rememberLocked appends a message to the chat history. Consecutive messages
// with the same role are merged, as in the Python version. Caller holds b.mu.
func (b *Bridge) rememberLocked(key, role, content string) *chatState {
	cs := b.touchLocked(key)
	n := len(cs.msgs)
	if n > 0 && cs.msgs[n-1].Role == role {
		merged := cs.msgs[n-1].Content + "\n" + content
		if limit := b.cfg.MaxInputChars * 2; runeCount(merged) > limit {
			merged = truncRunes(merged, limit)
		}
		cs.msgs[n-1].Content = merged
	} else {
		cs.msgs = append(cs.msgs, ChatMsg{Role: role, Content: content})
	}
	cs.updated = b.now().Unix()
	// Cap total history by code points so worst-case RAM stays bounded.
	lim := b.cfg.HistoryMaxChars
	for len(cs.msgs) > 1 && chatRunes(cs.msgs) > lim {
		cs.msgs = append([]ChatMsg(nil), cs.msgs[1:]...)
	}
	if len(cs.msgs) == 1 && chatRunes(cs.msgs) > lim {
		cs.msgs[0].Content = truncRunes(cs.msgs[0].Content, lim)
	}
	if over := len(cs.msgs) - b.cfg.HistoryMessages; over > 0 {
		cs.msgs = append([]ChatMsg(nil), cs.msgs[over:]...)
	}
	cs.runes = chatRunes(cs.msgs)
	b.markChatPendingLocked(key, cs)
	return cs
}

func (b *Bridge) chatKeys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.chats))
	for k := range b.chats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------- reply pipeline ----------

func (b *Bridge) replyJob(cs *chatState, key string, chatID int64, conn string) {
	b.replyJobOpt(cs, key, chatID, conn, false)
}

// replyJobOpt runs one reply. followup marks a sweep-started reply whose rate
// slot is consumed only once the message is actually delivered.
func (b *Bridge) replyJobOpt(cs *chatState, key string, chatID int64, conn string, followup bool) {
	defer func() {
		if r := recover(); r != nil {
			logf("ERROR", "reply job crashed: %v", r)
		}
		b.mu.Lock()
		cs.active--
		b.mu.Unlock()
	}()

	cs.lk.Lock() // one reply at a time per chat keeps the conversation ordered
	defer cs.lk.Unlock()

	// Wait a moment so a burst of consecutive messages is answered together
	// (one reply, lower token cost) instead of once per message.
	if d := time.Duration(b.cfg.DebounceMS) * time.Millisecond; d > 0 {
		b.sleep(context.Background(), d)
	}

	b.mu.Lock()
	if b.silencedLocked(key, conn) {
		b.mu.Unlock()
		return
	}
	// Everything was already answered by another job or by the owner.
	if !cs.pendingLocked() {
		b.mu.Unlock()
		return
	}
	gen := cs.userGen
	note := introFirst
	if cs.introduced {
		note = introDone
	}
	system := b.sysPrompt + b.subst(note) + b.rulesNoteLocked()
	msgs := make([]ChatMsg, 0, len(cs.msgs)+1)
	msgs = append(msgs, ChatMsg{Role: "system", Content: system})
	msgs = append(msgs, cs.msgs...)
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.typingLoop(ctx, chatID, conn)

	t0 := time.Now()
	reply, err := b.callLLM(ctx, msgs)
	cancel()
	if err != nil {
		logf("WARN", "llm error: %v", err)
		return
	}
	if reply == "" {
		return
	}

	// The owner may have come online or answered while the model was thinking.
	b.mu.Lock()
	if b.silencedLocked(key, conn) || cs.ownerGen >= gen {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	reply = truncReply(reply, b.cfg.MaxReplyChars)
	if !b.sendMsg(chatID, reply, conn) {
		return
	}
	fire := false
	b.mu.Lock()
	cs.introduced = true
	cs.repliedGen = gen
	if chatID > 0 { // private chats only; group/channel IDs are negative
		b.recordTranscriptLocked(key, "b", "", reply)
	}
	cs.followupAfter = 0
	if followup {
		b.rateAllowLocked(cs.lastUID, true)
	}
	b.repliesSent++
	b.saveStateLocked()
	b.rememberLocked(key, "assistant", reply)
	if b.rulesOn() {
		b.rulesPending++
		if b.rulesPending >= b.cfg.RulesUpdateEvery {
			b.rulesPending = 0
			fire = true
		}
	}
	b.mu.Unlock()
	if fire {
		b.jobWG.Add(1)
		go func() {
			defer b.jobWG.Done()
			b.distillJob()
		}()
	}

	// Persist the delivered reply at once: after a power cut the restored chat
	// must not look unanswered, or the customer would get the reply twice.
	if conn != "" {
		_ = b.flushNow()
	}

	kind := "direct"
	if conn != "" {
		kind = "business"
	}
	logf("INFO", "replied %s in %.1fs", kind, time.Since(t0).Seconds())
}

// callLLM bounds concurrency and retries 429, 5xx and transport failures.
func (b *Bridge) callLLM(ctx context.Context, msgs []ChatMsg) (string, error) {
	select {
	case b.llmSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-b.llmSem }()

	b.mu.Lock()
	b.llmActive++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.llmActive--
		b.mu.Unlock()
	}()

	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.LLMTimeoutSec)*time.Second)
		reply, err := b.llm.Chat(actx, msgs)
		cancel()
		if err == nil {
			return reply, nil
		}
		if !retryableLLM(err) || attempt >= b.cfg.LLMRetries {
			return "", err
		}
		logf("WARN", "llm retry %d: %v", attempt+1, err)
		b.sleep(ctx, time.Duration(3*(attempt+1))*time.Second)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
}

func retryableLLM(err error) bool {
	var le *LLMError
	if !errors.As(err, &le) {
		return false
	}
	switch le.Kind {
	case "http":
		return le.Status == 429 || le.Status >= 500
	case "timeout", "conn", "read":
		return true
	}
	return false
}

func (b *Bridge) typingLoop(ctx context.Context, chatID int64, conn string) {
	for {
		params := map[string]any{"chat_id": chatID, "action": "typing"}
		if conn != "" {
			params["business_connection_id"] = conn
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = b.tg.Call(cctx, "sendChatAction", params, nil)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(4500 * time.Millisecond):
		}
	}
}

// sendMsg retries on flood-control (retry_after) and transport errors.
func (b *Bridge) sendMsg(chatID int64, text, conn string) bool {
	params := map[string]any{"chat_id": chatID, "text": text}
	if conn != "" {
		params["business_connection_id"] = conn
	}
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := b.tg.Call(ctx, "sendMessage", params, nil)
		cancel()
		if err == nil {
			return true
		}
		var te *TGError
		if errors.As(err, &te) {
			if te.RetryAfter > 0 {
				wait := te.RetryAfter
				if wait > 30 {
					wait = 30
				}
				b.sleep(context.Background(), time.Duration(wait)*time.Second)
				continue
			}
			logf("WARN", "sendMessage failed: %d", te.Code)
			return false
		}
		logf("WARN", "sendMessage error: %v", err)
		b.sleep(context.Background(), 2*time.Second)
	}
	return false
}

// ---------- main loop ----------

// Run long-polls Telegram until ctx is cancelled. It returns nil on shutdown.
func (b *Bridge) Run(ctx context.Context) error {
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	// A router often boots before its uplink is ready: keep retrying transient
	// failures instead of exiting, but fail fast on a rejected token.
	wait := 2 * time.Second
	for {
		gctx, gcancel := context.WithTimeout(ctx, 30*time.Second)
		err := b.tg.Call(gctx, "getMe", nil, &me)
		gcancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		var te *TGError
		if errors.As(err, &te) && te.RetryAfter == 0 && te.Code >= 400 && te.Code < 500 && te.Code != 429 {
			return fmt.Errorf("getMe: %w", err)
		}
		logf("WARN", "getMe failed, retrying in %s: %v", wait, err)
		b.sleep(ctx, wait)
		if ctx.Err() != nil {
			return nil
		}
		wait = minDur(wait*2, time.Minute)
	}
	b.botID, b.botUsername = me.ID, me.Username
	b.mu.Lock()
	logf("INFO", "started as @%s owner_id=%d paused=%v", b.botUsername, b.cfg.OwnerID, b.paused)
	b.mu.Unlock()

	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	_ = b.tg.Call(dctx, "deleteWebhook", map[string]bool{"drop_pending_updates": false}, nil)
	dcancel()

	var offset int64
	hasOffset := false
	b.mu.Lock()
	offset = b.offset
	if offset > 0 {
		hasOffset = true
	}
	b.mu.Unlock()
	allowed := []string{"message", "business_connection", "business_message", "edited_business_message"}
	backoff := time.Second
	pollParams := map[string]any{"timeout": 50, "allowed_updates": allowed}

	for {
		if ctx.Err() != nil {
			return nil
		}
		if hasOffset {
			pollParams["offset"] = offset
		} else {
			delete(pollParams, "offset")
		}
		var updates []Update
		pctx, pcancel := context.WithTimeout(ctx, 65*time.Second)
		err := b.tg.Call(pctx, "getUpdates", pollParams, &updates)
		pcancel()

		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var te *TGError
			if errors.As(err, &te) {
				if te.Code == 409 {
					logf("ERROR", "409 conflict: another process is polling this bot token")
					b.sleep(ctx, 15*time.Second)
					continue
				}
				logf("WARN", "getUpdates error %d", te.Code)
				wait := backoff
				if te.RetryAfter > 0 {
					wait = time.Duration(te.RetryAfter) * time.Second
				}
				b.sleep(ctx, wait)
				backoff = minDur(backoff*2, time.Minute)
				continue
			}
			logf("WARN", "getUpdates network error: %v", err)
			b.sleep(ctx, backoff)
			backoff = minDur(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			offset = u.UpdateID + 1
			hasOffset = true
			if res := b.safeHandle(u); res != "" {
				logf("INFO", "update %d -> %s", u.UpdateID, res)
			}
		}
		if len(updates) > 0 {
			b.mu.Lock()
			b.offset = offset
			b.dirtyMeta = true
			b.mu.Unlock()
			// Commit the offset right away: a crash in the flush window would
			// replay fresh messages (duplicate replies) on restart. Meta-only
			// transactions are tiny; pruning is rate-limited inside flushNow.
			_ = b.flushNow()
		}
	}
}

func (b *Bridge) safeHandle(u Update) (res string) {
	defer func() {
		if r := recover(); r != nil {
			logf("ERROR", "handler crashed for update %d: %v", u.UpdateID, r)
			res = "error"
		}
	}()
	return b.handleUpdate(u)
}

func minDur(a, c time.Duration) time.Duration {
	if a < c {
		return a
	}
	return c
}

// Drain waits up to limit for in-flight reply/distill jobs to finish. Used on
// shutdown so a SIGTERM does not cut a reply mid-flight; 0 disables waiting.
func (b *Bridge) Drain(limit time.Duration) bool {
	if limit <= 0 {
		return true
	}
	done := make(chan struct{})
	go func() {
		b.jobWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}
