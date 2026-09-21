package router

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestSaveNonImageFilesWritesToDataDirAndBuildsSuffix(t *testing.T) {
	dataDir := t.TempDir()
	files := []slack.SlackFile{
		{
			ID:       "FPNG",
			Name:     "screen.png",
			MimeType: "image/png",
		},
		{
			ID:       "F0C33HVE3QV",
			Name:     "deck.pptx",
			MimeType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			Size:     1234,
		},
	}

	called := 0
	suffix := saveNonImageFiles(context.Background(), files, dataDir, 0, func(ctx context.Context, f slack.SlackFile) ([]byte, error) {
		called++
		if f.ID != "F0C33HVE3QV" {
			t.Fatalf("downloaded non-target file %s", f.ID)
		}
		return []byte("pptx-bytes"), nil
	})

	if called != 1 {
		t.Fatalf("download calls = %d, want 1", called)
	}

	wantPath := filepath.Join(dataDir, "attachments", "F0C33HVE3QV_deck.pptx")
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("reading written attachment: %v", err)
	}
	if string(got) != "pptx-bytes" {
		t.Fatalf("attachment contents = %q, want %q", got, "pptx-bytes")
	}

	if !strings.Contains(suffix, "[Attached files]") {
		t.Fatalf("suffix missing header: %q", suffix)
	}
	if !strings.Contains(suffix, "deck.pptx") {
		t.Fatalf("suffix missing file name: %q", suffix)
	}
	if !strings.Contains(suffix, wantPath) {
		t.Fatalf("suffix missing absolute path: %q", suffix)
	}
}

func TestSaveNonImageFilesSkipsOversize(t *testing.T) {
	dataDir := t.TempDir()
	files := []slack.SlackFile{{
		ID:       "FBIG",
		Name:     "huge.zip",
		MimeType: "application/zip",
		Size:     30 * 1024 * 1024,
	}}

	called := 0
	suffix := saveNonImageFiles(context.Background(), files, dataDir, 25<<20, func(ctx context.Context, f slack.SlackFile) ([]byte, error) {
		called++
		return []byte("nope"), nil
	})

	if called != 0 {
		t.Fatalf("download calls = %d, want 0", called)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "attachments")); !os.IsNotExist(err) {
		t.Fatalf("expected no attachments dir, stat err = %v", err)
	}
	if !strings.Contains(suffix, "skipped") {
		t.Fatalf("suffix missing skipped marker: %q", suffix)
	}
}

func TestSaveNonImageFilesEmptyWhenOnlyImages(t *testing.T) {
	dataDir := t.TempDir()
	files := []slack.SlackFile{{
		ID:       "FPNG",
		Name:     "screen.png",
		MimeType: "image/png",
	}}

	suffix := saveNonImageFiles(context.Background(), files, dataDir, 0, func(ctx context.Context, f slack.SlackFile) ([]byte, error) {
		t.Fatal("download should not be called for image files")
		return nil, nil
	})

	if suffix != "" {
		t.Fatalf("suffix = %q, want empty", suffix)
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

func TestSaveNonImageFilesReportsWriteFailure(t *testing.T) {
	// dataDir is a regular file, so MkdirAll under it must fail.
	dataDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(dataDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []slack.SlackFile{{ID: "FDOC", Name: "doc.pdf", MimeType: "application/pdf", Size: 10}}

	suffix := saveNonImageFiles(context.Background(), files, dataDir, 0, func(ctx context.Context, f slack.SlackFile) ([]byte, error) {
		return []byte("pdf"), nil
	})

	if !strings.Contains(suffix, "doc.pdf (skipped: write failed)") {
		t.Fatalf("suffix = %q, want write-failed note", suffix)
	}
}
