package slack

import (
	"testing"

	"github.com/format5/switchboard/internal/config"
)

func TestReloadConfig_RebuildChannelNameCache(t *testing.T) {
	// Setup: create an Edge with initial channels.
	initialChannels := []config.ChannelConfig{
		{ID: "C001", Name: "general"},
		{ID: "C002", Name: "random"},
	}
	e := &Edge{
		channelSet:  make(map[string]bool),
		channelName: make(map[string]string),
	}
	for _, ch := range initialChannels {
		e.channelSet[ch.ID] = true
		e.channelName[ch.ID] = ch.Name
	}

	// Verify initial state.
	if got := e.ChannelName("C001"); got != "general" {
		t.Fatalf("initial ChannelName(C001) = %q, want %q", got, "general")
	}
	if got := e.ChannelName("C002"); got != "random" {
		t.Fatalf("initial ChannelName(C002) = %q, want %q", got, "random")
	}

	// Reload with new channels (C002 removed, C003 added, C001 renamed).
	newChannels := []config.ChannelConfig{
		{ID: "C001", Name: "announcements"},
		{ID: "C003", Name: "engineering"},
	}
	e.ReloadConfig(newChannels, nil)

	// After reload, C001 should have the new name.
	if got := e.ChannelName("C001"); got != "announcements" {
		t.Errorf("after reload ChannelName(C001) = %q, want %q", got, "announcements")
	}

	// C002 should no longer be present (was removed from config).
	if got := e.ChannelName("C002"); got != "" {
		t.Errorf("after reload ChannelName(C002) = %q, want empty (channel removed)", got)
	}

	// C003 should be populated.
	if got := e.ChannelName("C003"); got != "engineering" {
		t.Errorf("after reload ChannelName(C003) = %q, want %q", got, "engineering")
	}
}
