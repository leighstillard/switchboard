package router

import (
	"context"
	"testing"

	"github.com/format5/switchboard/internal/slack"
)

func TestImagesFromSlackFilesDownloadsImageAttachments(t *testing.T) {
	files := []slack.SlackFile{
		{
			ID:       "FPNG",
			Name:     "screen.png",
			MimeType: "image/png",
			URL:      "https://files.slack.test/screen.png",
		},
		{
			ID:       "FTXT",
			Name:     "notes.txt",
			MimeType: "text/plain",
			URL:      "https://files.slack.test/notes.txt",
		},
	}

	called := 0
	images := imagesFromSlackFiles(context.Background(), files, func(ctx context.Context, f slack.SlackFile) ([]byte, error) {
		called++
		if f.ID != "FPNG" {
			t.Fatalf("downloaded non-image file %s", f.ID)
		}
		return []byte{0x89, 'P', 'N', 'G'}, nil
	})

	if called != 1 {
		t.Fatalf("download calls = %d, want 1", called)
	}
	if len(images) != 1 {
		t.Fatalf("images = %d, want 1", len(images))
	}
	if images[0].MediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", images[0].MediaType)
	}
	if string(images[0].Data) != string([]byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("image bytes = %x, want PNG bytes", images[0].Data)
	}
}

func TestSlackFileAttachmentsJSONRoundTrip(t *testing.T) {
	files := []slack.SlackFile{{
		ID:       "FPNG",
		Name:     "screen.png",
		MimeType: "image/png",
		Size:     1234,
		URL:      "https://files.slack.test/screen.png",
	}}

	encoded := slackFileAttachmentsJSON(files)
	if encoded == nil {
		t.Fatal("encoded attachments JSON is nil")
	}

	got := slackFilesFromAttachmentsJSON(encoded)
	if len(got) != 1 {
		t.Fatalf("files = %d, want 1", len(got))
	}
	if got[0] != files[0] {
		t.Fatalf("file = %+v, want %+v", got[0], files[0])
	}
}

func TestSlackFileAttachmentsJSONEmpty(t *testing.T) {
	if got := slackFileAttachmentsJSON(nil); got != nil {
		t.Fatalf("encoded nil files = %q, want nil", *got)
	}
	if got := slackFilesFromAttachmentsJSON(nil); got != nil {
		t.Fatalf("decoded nil JSON = %+v, want nil", got)
	}
}
