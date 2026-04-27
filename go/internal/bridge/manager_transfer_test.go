package bridge

import (
	"strings"
	"testing"
)

func TestNormalizeReferTarget_URI(t *testing.T) {
	uri, err := normalizeReferTarget("sip:alice@example.com", "sipconnect.sipgate.de")
	if err != nil {
		t.Fatalf("normalizeReferTarget returned error: %v", err)
	}
	if got, want := uri.String(), "sip:alice@example.com"; got != want {
		t.Fatalf("uri mismatch: got %q want %q", got, want)
	}
}

func TestNormalizeReferTarget_NumberAddsDomain(t *testing.T) {
	uri, err := normalizeReferTarget("+4912345", "sipconnect.sipgate.de")
	if err != nil {
		t.Fatalf("normalizeReferTarget returned error: %v", err)
	}
	if got, want := uri.String(), "sip:+4912345@sipconnect.sipgate.de"; got != want {
		t.Fatalf("uri mismatch: got %q want %q", got, want)
	}
}

func TestNormalizeReferTarget_Invalid(t *testing.T) {
	_, err := normalizeReferTarget("", "sipconnect.sipgate.de")
	if err == nil {
		t.Fatalf("expected error for empty target")
	}
}

func TestCallManager_TransferCall_NotFound(t *testing.T) {
	manager := &CallManager{}
	if err := manager.TransferCall("missing", "sip:alice@example.com"); err == nil {
		t.Fatalf("expected not found error")
	}
}

func TestCallManager_HandleReferNotify_MissingSession(t *testing.T) {
	manager := &CallManager{}
	manager.HandleReferNotify("missing")
}

func TestCallManager_HandleReferNotify_NilDialog(t *testing.T) {
	manager := &CallManager{}
	manager.sessions.Store("call-1", &CallSession{callID: "call-1"})
	manager.HandleReferNotify("call-1")
}

func TestCallManager_EndCall_NotFound(t *testing.T) {
	manager := &CallManager{}
	err := manager.EndCall("missing")
	if err == nil {
		t.Fatalf("expected not found error")
	}
	if !strings.HasPrefix(err.Error(), "call not found:") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCallManager_EndCall_NilDialog(t *testing.T) {
	manager := &CallManager{}
	manager.sessions.Store("call-1", &CallSession{callID: "call-1"})
	err := manager.EndCall("call-1")
	if err == nil {
		t.Fatalf("expected error for nil dialog")
	}
	if !strings.Contains(err.Error(), "dialog") {
		t.Fatalf("expected dialog error, got: %v", err)
	}
}
