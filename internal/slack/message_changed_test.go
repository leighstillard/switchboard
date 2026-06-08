package slack

import (
	"testing"

	"github.com/format5/switchboard/internal/config"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func TestHandleMessageChangedDispatchesImageFileInThread(t *testing.T) {
	edge := &Edge{
		botUserID:  "U0BOT123",
		channelSet: map[string]bool{"C123": true},
	}

	var got *InboundMessage
	edge.SetInboundHandler(func(msg *InboundMessage) {
		got = msg
	})

	edge.handleMessage(&slackevents.MessageEvent{
		Type:        "message",
		SubType:     "message_changed",
		Channel:     "C123",
		ChannelType: slackevents.ChannelTypeChannel,
		Message: &slackapi.Msg{
			Type:            "message",
			Channel:         "C123",
			User:            "UUSER123",
			Text:            "",
			Timestamp:       "1710000001.000200",
			ThreadTimestamp: "1710000000.000100",
			Files: []slackapi.File{{
				ID:         "FPNG",
				Name:       "screen.png",
				Mimetype:   "image/png",
				Size:       1234,
				URLPrivate: "https://files.slack.test/screen.png",
			}},
		},
	})

	if got == nil {
		t.Fatal("message_changed with image file was not dispatched")
	}
	if got.ChannelID != "C123" || got.ThreadTS != "1710000000.000100" || got.MessageTS != "1710000001.000200" {
		t.Fatalf("wrong message coordinates: channel=%q thread=%q message=%q", got.ChannelID, got.ThreadTS, got.MessageTS)
	}
	if got.UserID != "UUSER123" {
		t.Fatalf("user = %q, want UUSER123", got.UserID)
	}
	if len(got.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(got.Files))
	}
	if got.Files[0].ID != "FPNG" || got.Files[0].MimeType != "image/png" || got.Files[0].URL == "" {
		t.Fatalf("unexpected file metadata: %+v", got.Files[0])
	}
}

func TestHandleMessageChangedWithoutFilesIsIgnored(t *testing.T) {
	edge := &Edge{
		botUserID:  "U0BOT123",
		channelSet: map[string]bool{"C123": true},
	}

	dispatched := false
	edge.SetInboundHandler(func(msg *InboundMessage) {
		dispatched = true
	})

	edge.handleMessage(&slackevents.MessageEvent{
		Type:        "message",
		SubType:     "message_changed",
		Channel:     "C123",
		ChannelType: slackevents.ChannelTypeChannel,
		Message: &slackapi.Msg{
			Type:            "message",
			Channel:         "C123",
			User:            "UUSER123",
			Text:            "edited text",
			Timestamp:       "1710000001.000200",
			ThreadTimestamp: "1710000000.000100",
		},
	})

	if dispatched {
		t.Fatal("message_changed without files should be ignored")
	}
}

func TestHandleMessageChangedDropsUnconfiguredChannels(t *testing.T) {
	edge := &Edge{
		botUserID:  "U0BOT123",
		channelSet: map[string]bool{"C123": true},
		channels:   []config.ChannelConfig{{ID: "C123"}},
	}

	dispatched := false
	edge.SetInboundHandler(func(msg *InboundMessage) {
		dispatched = true
	})

	edge.handleMessage(&slackevents.MessageEvent{
		Type:        "message",
		SubType:     "message_changed",
		Channel:     "C999",
		ChannelType: slackevents.ChannelTypeChannel,
		Message: &slackapi.Msg{
			Type:            "message",
			Channel:         "C999",
			User:            "UUSER123",
			Timestamp:       "1710000001.000200",
			ThreadTimestamp: "1710000000.000100",
			Files: []slackapi.File{{
				ID:         "FPNG",
				Name:       "screen.png",
				Mimetype:   "image/png",
				URLPrivate: "https://files.slack.test/screen.png",
			}},
		},
	})

	if dispatched {
		t.Fatal("message_changed from unconfigured channel should be ignored")
	}
}
