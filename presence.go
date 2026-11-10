package main

import (
	"context"
	"fmt"
	"time"
)

// Owner presence.
//
// The Bot API exposes no online/read status, so presence is inferred from the
// owner's own actions: sending or editing a message in any business chat, or
// the explicit /here command. While the owner was active within
// owner_away_minutes the bot stays silent in every business chat; customer
// messages are still remembered and answered by the follow-up sweep once the
// owner has been away long enough and has not answered them.

const presenceSweepEvery = 10 * time.Second

// ownerAwayLocked reports whether the bot may answer business chats now.
// Caller holds b.mu.
func (b *Bridge) ownerAwayLocked() bool {
	away := time.Duration(b.cfg.OwnerAwayMinutes) * time.Minute
	if away <= 0 || b.ownerSeen.IsZero() {
		return true
	}
	return b.now().Sub(b.ownerSeen) >= away
}

// markOwnerActiveLocked records owner activity now. Caller holds b.mu.
func (b *Bridge) markOwnerActiveLocked() {
	b.ownerSeen = b.now()
	b.saveStateLocked()
}

// pendingLocked reports whether the chat has customer messages that neither
// the bot nor the owner has answered. Caller holds b.mu.
func (cs *chatState) pendingLocked() bool {
	answered := cs.repliedGen
	if cs.ownerGen > answered {
		answered = cs.ownerGen
	}
	return cs.userGen > answered
}

// presenceLineLocked renders the owner state for /status. Caller holds b.mu.
func (b *Bridge) presenceLineLocked() string {
	away := time.Duration(b.cfg.OwnerAwayMinutes) * time.Minute
	if away <= 0 {
		return "Chế độ theo dõi online: tắt (bot luôn trực)"
	}
	if b.ownerSeen.IsZero() {
		return "Bạn: ⚪ chưa thấy hoạt động — bot đang trực"
	}
	since := b.now().Sub(b.ownerSeen)
	if since < away {
		left := (away - since).Round(time.Minute)
		if left < time.Minute {
			left = time.Minute
		}
		return fmt.Sprintf("Bạn: 🟢 online (hoạt động %s trước) — bot im lặng thêm %s",
			fmtDur(since), fmtDur(left))
	}
	return fmt.Sprintf("Bạn: ⚪ offline %s — bot đang trực", fmtDur(since))
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return "<1 phút"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d phút", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// followupRetryAfter spaces out retries for one chat when a follow-up fails.
const followupRetryAfter = time.Minute

// followupEligibleLocked reports whether a chat is waiting for a follow-up
// inside the follow-up window. Such chats are protected from LRU eviction.
// Caller holds b.mu.
func (b *Bridge) followupEligibleLocked(cs *chatState) bool {
	if b.cfg.FollowupMaxMin <= 0 || cs.conn == "" || cs.chatID == 0 || cs.lastUserAt == 0 {
		return false
	}
	if !cs.pendingLocked() {
		return false
	}
	window := time.Duration(b.cfg.FollowupMaxMin) * time.Minute
	return b.now().Sub(time.Unix(cs.lastUserAt, 0)) <= window
}

// pendingCountLocked counts chats waiting for a follow-up. Caller holds b.mu.
func (b *Bridge) pendingCountLocked() int {
	n := 0
	for _, cs := range b.chats {
		if cs.conn != "" && cs.pendingLocked() {
			n++
		}
	}
	return n
}

// sweepPending answers business chats whose customers were left waiting while
// the owner was active, once the owner is away. Returns the jobs started.
func (b *Bridge) sweepPending() int {
	if b.cfg.FollowupMaxMin <= 0 {
		return 0
	}
	type job struct {
		cs     *chatState
		key    string
		chatID int64
		conn   string
	}
	var jobs []job

	b.mu.Lock()
	if b.paused || !b.ownerAwayLocked() {
		b.mu.Unlock()
		return 0
	}
	now := b.now()
	for key, cs := range b.chats {
		if cs.active > 0 || !b.followupEligibleLocked(cs) {
			continue
		}
		if cs.followupAfter > now.Unix() || b.isMutedLocked(key) {
			continue
		}
		// Check only; the slot is consumed when the reply is delivered.
		if !b.rateAllowLocked(cs.lastUID, false) {
			continue
		}
		cs.followupAfter = now.Add(followupRetryAfter).Unix()
		cs.active++
		cs.looping = true
		jobs = append(jobs, job{cs, key, cs.chatID, cs.conn})
	}
	b.mu.Unlock()

	for _, j := range jobs {
		j := j
		b.jobWG.Add(1)
		go func() {
			defer b.jobWG.Done()
			b.replyLoop(j.cs, j.key, j.chatID, j.conn, true)
		}()
	}
	if len(jobs) > 0 {
		logf("INFO", "follow-up: answering %d waiting chat(s)", len(jobs))
	}
	return len(jobs)
}

// presenceLoop runs the follow-up sweep until ctx is cancelled. main joins it
// before draining jobs so no reply job is spawned after shutdown starts.
func (b *Bridge) presenceLoop(ctx context.Context) {
	if b.cfg.FollowupMaxMin <= 0 || b.cfg.OwnerAwayMinutes <= 0 {
		return
	}
	t := time.NewTicker(presenceSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.sweepPending()
		}
	}
}
