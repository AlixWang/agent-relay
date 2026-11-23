package muse

// nudge.go implements Tier-2 Muse direct-chat warm-wake nudges.
//
// When a 1:1 task sits un-acked and un-progressed beyond TaskNudgeAfter (default
// 180s), SweepStalledTasks issues a single short nudge into the peer's Muse
// main chat. Unlike interactive #muse-chat jobs, a nudge:
//   - checks chatBusy first and skips cleanly if the agent is already active;
//   - only confirms user-message delivery (<=5s) without waiting 180s for a reply;
//   - closes the Noise connection and releases the account lease immediately;
//   - runs at most once per task and respects per-peer cooldown.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	nudgeSweepLookbackSecs = 1500 // 25m; stop nudging near the 30m client worker timeout
	nudgeTaskRetentionSecs = 86400
)

// NudgeResult describes the outcome of a single NudgeForTasks attempt.
type NudgeResult struct {
	PeerID     string   `json:"peer_id"`
	AccountID  string   `json:"account_id,omitempty"`
	TaskIDs    []string `json:"task_ids"`
	Status     string   `json:"status"` // sent | skipped | delivery_unknown | failed
	Reason     string   `json:"reason,omitempty"`
	UserSeq    int64    `json:"user_seq,omitempty"`
	ErrorClass string   `json:"error_class,omitempty"`
}

// NudgeForTasks sends one short warm-wake prompt to peerID's Muse main chat
// for the specified stalled taskIDs. It holds the account Noise lease only
// for open + busy-check + send + delivery-confirm (~3-8s) and releases it
// immediately on return.
func (s *Service) NudgeForTasks(ctx context.Context, peerID string, taskIDs []string) (*NudgeResult, error) {
	res := &NudgeResult{PeerID: peerID, TaskIDs: append([]string(nil), taskIDs...)}
	if s == nil || len(taskIDs) == 0 || strings.TrimSpace(peerID) == "" {
		res.Status = "skipped"
		res.Reason = "invalid_args"
		return res, nil
	}

	s.chatMu.Lock()
	deploy := s.opts.DirectChatEnabled
	nudgeOn := s.opts.TaskNudgeEnabled
	runtime := s.chatRuntime
	s.chatMu.Unlock()
	if !deploy || !nudgeOn || !runtime {
		res.Status = "skipped"
		res.Reason = "nudge_disabled"
		return res, nil
	}

	account, err := s.st.GetMuseAccountByPeer(peerID)
	if err != nil {
		res.Status = "failed"
		res.ErrorClass = "muse_store_error"
		return res, err
	}
	if account == nil || !account.Enabled || !account.AutoWake || !account.ChatEnabled {
		res.Status = "skipped"
		res.Reason = "account_ineligible"
		return res, nil
	}
	res.AccountID = account.ID
	if account.AuthState != "ok" {
		res.Status = "skipped"
		res.Reason = "auth_" + account.AuthState
		return res, nil
	}
	if account.VMState == "DISABLED" {
		res.Status = "skipped"
		res.Reason = "vm_disabled"
		return res, nil
	}

	now := s.now().Unix()
	nudgeID := "mn_" + newChatID()[3:]

	s.chatMu.Lock()
	if !s.chatRuntime {
		s.chatMu.Unlock()
		res.Status = "skipped"
		res.Reason = "chat_runtime_off"
		return res, nil
	}
	if s.chatActive[account.ID] != "" {
		s.chatMu.Unlock()
		res.Status = "skipped"
		res.Reason = "chat_active"
		return res, nil
	}
	select {
	case s.chatSem <- struct{}{}:
	default:
		s.chatMu.Unlock()
		res.Status = "skipped"
		res.Reason = "chat_busy"
		return res, nil
	}
	if _, code := s.checkChatRateLocked(peerID, now); code != "" {
		<-s.chatSem
		s.chatMu.Unlock()
		res.Status = "skipped"
		res.Reason = "rate_limited"
		return res, nil
	}
	s.chatActive[account.ID] = nudgeID
	s.chatMu.Unlock()

	defer func() {
		s.chatMu.Lock()
		if s.chatActive[account.ID] == nudgeID {
			delete(s.chatActive, account.ID)
		}
		select {
		case <-s.chatSem:
		default:
		}
		s.chatMu.Unlock()
	}()

	sess, oerr := s.openChatSession(ctx, account.ID)
	if oerr != nil {
		cf := chatErrorFromFailure(oerr)
		res.Status = "failed"
		res.ErrorClass = cf.Code
		s.auditNudge("muse.nudge.failed", peerID, account.ID, taskIDs,
			fmt.Sprintf("stage=%s error_class=%s", chatAuditValue(cf.Class), chatAuditValue(cf.Code)), s.now().Unix())
		return res, oerr
	}
	defer sess.Close()
	if live, ok := sess.(*liveChatSession); ok {
		live.setJobContext(nudgeID, peerID)
	}

	hist, herr := sess.History(ctx, "", 5, 0)
	if herr != nil {
		cf := chatErrorFromFailure(herr)
		res.Status = "failed"
		res.ErrorClass = cf.Code
		s.auditNudge("muse.nudge.failed", peerID, account.ID, taskIDs,
			fmt.Sprintf("stage=history error_class=%s", chatAuditValue(cf.Code)), s.now().Unix())
		return res, herr
	}
	if chatBusy(hist.ChatEvents, s.now()) {
		res.Status = "skipped"
		res.Reason = "agent_busy"
		s.auditNudge("muse.nudge.skipped", peerID, account.ID, taskIDs, "reason=agent_busy", s.now().Unix())
		return res, nil
	}

	baseline := maxChatSeq(hist.ChatEvents)
	text := formatNudgeText(taskIDs)
	s.auditNudge("muse.nudge.sending", peerID, account.ID, taskIDs,
		fmt.Sprintf("baseline=%d", baseline), s.now().Unix())

	sendRes, serr := sess.Send(ctx, "", text, chatNodeID(account.ID))
	if sendRes.Evidence != chatSendMayHaveArrived {
		cf := chatErrorFromFailure(serr)
		res.Status = "failed"
		res.ErrorClass = cf.Code
		s.auditNudge("muse.nudge.failed", peerID, account.ID, taskIDs,
			fmt.Sprintf("stage=%s error_class=%s", chatAuditValue(cf.Class), chatAuditValue(cf.Code)), s.now().Unix())
		return res, serr
	}

	userSeq := s.confirmNudgeDelivery(ctx, sess, baseline, text)
	if userSeq <= 0 {
		res.Status = "delivery_unknown"
		res.Reason = "no_user_event"
		s.auditNudge("muse.nudge.delivery_unknown", peerID, account.ID, taskIDs, "reason=no_user_event", s.now().Unix())
		return res, nil
	}

	res.Status = "sent"
	res.UserSeq = userSeq
	s.auditNudge("muse.nudge.sent", peerID, account.ID, taskIDs,
		fmt.Sprintf("user_seq=%d delivery=confirmed", userSeq), s.now().Unix())
	return res, nil
}

func (s *Service) confirmNudgeDelivery(ctx context.Context, sess chatSession, baseline int64, text string) int64 {
	cctx, cancel := context.WithTimeout(ctx, chatConfirmBudget)
	defer cancel()
	for {
		if cctx.Err() != nil {
			return 0
		}
		hist, err := sess.History(cctx, "", 20, 0)
		if err == nil {
			if seq, _, found := findChatUser(hist.ChatEvents, baseline, text); found {
				return seq
			}
		} else if errors.Is(err, context.Canceled) {
			return 0
		}
		timer := time.NewTimer(chatConfirmEvery)
		select {
		case <-cctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		}
	}
}

// SweepStalledTasks scans for un-acked, un-progressed 1:1 tasks older than
// TaskNudgeAfter and asynchronously dispatches at most one warm-wake nudge
// per task (bounded by TaskNudgeCooldown per peer).
func (s *Service) SweepStalledTasks(ctx context.Context) {
	if s == nil {
		return
	}
	s.chatMu.Lock()
	enabled := s.opts.DirectChatEnabled && s.opts.TaskNudgeEnabled && s.chatRuntime
	s.chatMu.Unlock()
	if !enabled {
		return
	}

	now := s.now().Unix()
	afterSecs := int64(s.opts.TaskNudgeAfter / time.Second)
	if afterSecs <= 0 {
		afterSecs = 180
	}
	cooldownSecs := int64(s.opts.TaskNudgeCooldown / time.Second)
	if cooldownSecs <= 0 {
		cooldownSecs = 600
	}

	tasks, err := s.st.ListStalledDirectTasks(now-afterSecs, now-nudgeSweepLookbackSecs, 100)
	if err != nil {
		log.Printf("muse: sweep stalled tasks: %v", err)
		return
	}
	if len(tasks) == 0 {
		return
	}

	byPeer := map[string][]string{}
	var peerOrder []string
	s.mu.Lock()
	for id, ts := range s.nudgedTasks {
		if now-ts > nudgeTaskRetentionSecs {
			delete(s.nudgedTasks, id)
		}
	}
	for _, m := range tasks {
		if m.ID == "" || m.Recipient == "" {
			continue
		}
		if _, done := s.nudgedTasks[m.ID]; done {
			continue
		}
		if _, ok := byPeer[m.Recipient]; !ok {
			peerOrder = append(peerOrder, m.Recipient)
		}
		byPeer[m.Recipient] = append(byPeer[m.Recipient], m.ID)
	}
	s.mu.Unlock()

	for _, peerID := range peerOrder {
		if ctx.Err() != nil {
			return
		}
		ids := byPeer[peerID]
		if len(ids) == 0 {
			continue
		}
		account, aerr := s.st.GetMuseAccountByPeer(peerID)
		if aerr != nil || account == nil || !account.Enabled || !account.AutoWake || !account.ChatEnabled ||
			account.AuthState != "ok" || account.VMState == "DISABLED" {
			continue
		}
		if s.chatLeaseActive(account.ID) {
			continue
		}

		s.mu.Lock()
		if s.nudging[peerID] {
			s.mu.Unlock()
			continue
		}
		if last := s.lastPeerNudge[peerID]; last > 0 && now-last < cooldownSecs {
			s.mu.Unlock()
			continue
		}
		s.nudging[peerID] = true
		s.lastPeerNudge[peerID] = now
		s.mu.Unlock()

		go func(p string, taskIDs []string) {
			defer func() {
				s.mu.Lock()
				delete(s.nudging, p)
				s.mu.Unlock()
			}()
			nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			res, nerr := s.NudgeForTasks(nctx, p, taskIDs)
			if nerr != nil {
				log.Printf("muse: nudge peer=%s tasks=%d err=%v", p, len(taskIDs), nerr)
				return
			}
			if res != nil && (res.Status == "sent" || res.Status == "delivery_unknown") {
				ts := s.now().Unix()
				s.mu.Lock()
				for _, tid := range taskIDs {
					s.nudgedTasks[tid] = ts
				}
				s.mu.Unlock()
			}
		}(peerID, ids)
	}
}

func formatNudgeText(taskIDs []string) string {
	shown := taskIDs
	more := 0
	if len(shown) > 3 {
		more = len(shown) - 3
		shown = shown[:3]
	}
	label := strings.Join(shown, ",")
	if more > 0 {
		label = fmt.Sprintf("%s 等%d条", label, len(taskIDs))
	}
	return fmt.Sprintf("[agent-relay 唤醒] 待处理任务(%s)尚未响应，请查看 ~/workspace/task-relay/spool/wake.jsonl 或 GET /messages 完成并回传 result + ack。", label)
}

func (s *Service) auditNudge(action, peerID, accountID string, taskIDs []string, extra string, now int64) {
	detail := fmt.Sprintf("peer=%s account=%s tasks=%s",
		chatAuditValue(peerID), chatAuditValue(accountID), chatAuditValue(strings.Join(taskIDs, ",")))
	if extra != "" {
		detail += " " + extra
	}
	if err := s.st.AppendAudit("system", action, truncate(detail, chatAuditDetail), now); err != nil {
		log.Printf("muse nudge: audit %s failed: %v", action, err)
	}
}
