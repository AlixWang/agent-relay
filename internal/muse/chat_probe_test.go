package muse

import (
	"context"
	"errors"
	"testing"
)

func TestChatProbeApprovalsReusesJobSessionAndService(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)

	res, err := env.svc.ProbeApprovals(context.Background(), view.ChatID)
	if err != nil {
		t.Fatalf("probe approvals: %v", err)
	}
	if res.Status != 200 || res.Service != chatProbeService {
		t.Fatalf("probe result: %+v", res)
	}
	sess.mu.Lock()
	calls := append([]probeCall(nil), sess.serviceCalls...)
	sess.mu.Unlock()
	if len(calls) != 1 || calls[0].service != 1 || calls[0].verb != "GET" || calls[0].path != "/approvals" || calls[0].body != "" {
		t.Fatalf("service calls: %+v", calls)
	}
}

func TestChatProbeDecideVariantsAndGuardrails(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)

	cases := []struct {
		variant string
		want    string
	}{
		{ChatProbeVariantWithID, `{"approval_id":"ap_1","decision":"deny"}`},
		{ChatProbeVariantDecisionOnly, `{"decision":"allow_once"}`},
		{ChatProbeVariantEmpty, ""},
	}
	for i, tc := range cases {
		decision := ChatProbeDecisionDeny
		if i > 0 {
			decision = ChatProbeDecisionAllowOnce
		}
		res, err := env.svc.ProbeDecide(context.Background(), view.ChatID, "ap_1", decision, tc.variant)
		if err != nil {
			t.Fatalf("%s: %v", tc.variant, err)
		}
		if res.Status != 200 || res.RequestVariant != tc.variant {
			t.Fatalf("%s result: %+v", tc.variant, res)
		}
	}
	sess.mu.Lock()
	calls := append([]probeCall(nil), sess.serviceCalls...)
	sess.mu.Unlock()
	if len(calls) != len(cases) {
		t.Fatalf("service calls=%d", len(calls))
	}
	for i, tc := range cases {
		if calls[i].service != 1 || calls[i].verb != "POST" ||
			calls[i].path != "/approvals/ap_1/decide" || calls[i].body != tc.want {
			t.Fatalf("variant %s call: %+v", tc.variant, calls[i])
		}
	}

	if _, err := env.svc.ProbeDecide(context.Background(), view.ChatID, "ap_1", "allow_always", ChatProbeVariantDecisionOnly); err == nil {
		t.Fatal("allow_always was accepted")
	} else {
		var ce *ChatAdmissionError
		if !errors.As(err, &ce) || ce.Code != "probe_allow_always_forbidden" {
			t.Fatalf("allow_always error: %v", err)
		}
	}
	if _, err := env.svc.ProbeDecide(context.Background(), view.ChatID, "ap_1", "deny", "bogus"); err == nil {
		t.Fatal("unknown variant accepted")
	}
	if _, err := env.svc.ProbeDecide(context.Background(), view.ChatID, "bad/id", "deny", ChatProbeVariantEmpty); err == nil {
		t.Fatal("bad approval id accepted")
	}
	if _, err := env.svc.ProbeApprovals(context.Background(), "mc_missing"); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("missing job: %v", err)
	}

	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitChatDone(t, env.svc, view.ChatID)
	if _, err := env.svc.ProbeApprovals(context.Background(), view.ChatID); !errors.Is(err, ErrChatNotActive) {
		t.Fatalf("terminal job probe: %v", err)
	}
}
