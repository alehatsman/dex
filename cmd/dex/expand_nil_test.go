package main

import "testing"

// TestNewServerFromEnvNoTypedNilExpandClient pins the #502 fix: when no
// expansion model is configured, newExpandClient returns a nil *chat.Client.
// Assigning that concrete nil pointer straight into the Server.ExpandClient
// interface field would leave the interface non-nil (it wraps a typed nil),
// so the per-request expand=on|full override would deref it and panic instead
// of degrading to the documented no-op. The constructor must leave the
// interface genuinely nil.
func TestNewServerFromEnvNoTypedNilExpandClient(t *testing.T) {
	// Clear every input newExpandClient consults so it returns a nil client.
	t.Setenv("DEX_EXPAND_MODEL", "")
	t.Setenv("DEX_EXPAND_URL", "")
	t.Setenv("DEX_CHAT_URL", "")
	t.Setenv("DEX_CHAT_MODEL", "")

	srv, _ := newServerFromEnv(t.TempDir())

	if srv.ExpandClient != nil {
		t.Fatalf("ExpandClient should be a nil interface when no expand model is configured; "+
			"got a non-nil interface (typed-nil leak, #502): %#v", srv.ExpandClient)
	}
	if srv.ExpandMode != "off" {
		t.Errorf("ExpandMode = %q, want \"off\" when no expand client is wired", srv.ExpandMode)
	}
}
