package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/format5/switchboard/internal/agent"
	"github.com/format5/switchboard/internal/slack"
)

type slackFileDownloader func(context.Context, slack.SlackFile) ([]byte, error)

// fileMediaType resolves a Slack file's media type from its reported MIME
// type, falling back to a guess from its extension.
func fileMediaType(file slack.SlackFile) string {
	mediaType := file.MimeType
	if mediaType == "" && file.Name != "" {
		mediaType = mime.TypeByExtension(filepath.Ext(file.Name))
	}
	return mediaType
}

func imagesFromSlackFiles(ctx context.Context, files []slack.SlackFile, download slackFileDownloader) []agent.Image {
	if len(files) == 0 || download == nil {
		return nil
	}

	images := make([]agent.Image, 0, len(files))
	for _, file := range files {
		mediaType := fileMediaType(file)
		if !strings.HasPrefix(mediaType, "image/") {
			continue
		}

		data, err := download(ctx, file)
		if err != nil {
			slog.Warn("router: failed to download Slack image", "file_id", file.ID, "name", file.Name, "err", err)
			continue
		}
		if len(data) == 0 {
			continue
		}

		images = append(images, agent.Image{
			MediaType: mediaType,
			Data:      data,
		})
	}

	return images
}

// saveNonImageFiles downloads non-image Slack attachments to
// <dataDir>/attachments/<fileID>_<basename> and returns a prompt suffix
// listing them, or "" when there were none.
func saveNonImageFiles(ctx context.Context, files []slack.SlackFile, dataDir string, maxBytes int, download slackFileDownloader) string {
	if len(files) == 0 || download == nil {
		return ""
	}

	var lines []string
	for _, file := range files {
		if strings.HasPrefix(fileMediaType(file), "image/") {
			continue
		}

		if maxBytes > 0 && file.Size > maxBytes {
			slog.Warn("router: skipping oversize Slack attachment", "file_id", file.ID, "name", file.Name, "size", file.Size)
			lines = append(lines, fmt.Sprintf("- %s (skipped: %s exceeds %s limit)", file.Name, formatFileSize(file.Size), formatFileSize(maxBytes)))
			continue
		}

		data, err := download(ctx, file)
		if err != nil {
			slog.Warn("router: failed to download Slack attachment", "file_id", file.ID, "name", file.Name, "err", err)
			lines = append(lines, fmt.Sprintf("- %s (skipped: download failed)", file.Name))
			continue
		}
		if len(data) == 0 {
			continue
		}

		base := filepath.Base(file.Name)
		if base == "" || base == "." || base == "/" {
			base = file.ID
		}
		path := filepath.Join(dataDir, "attachments", file.ID+"_"+base)
		if abs, err := filepath.Abs(path); err == nil {
			path = abs // the agent's cwd differs from ours; never hand it a relative path
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = os.WriteFile(path, data, 0o644)
		}
		if err != nil {
			slog.Warn("router: failed to write Slack attachment", "path", path, "err", err)
			lines = append(lines, fmt.Sprintf("- %s (skipped: write failed)", file.Name))
			continue
		}

		lines = append(lines, fmt.Sprintf("- %s (%s): %s", file.Name, formatFileSize(file.Size), path))
	}

	if len(lines) == 0 {
		return ""
	}
	return "\n\n[Attached files]\n" + strings.Join(lines, "\n")
}

// formatFileSize renders a byte count as MB (one decimal) when >= 1 MB,
// otherwise as whole KB.
func formatFileSize(bytes int) string {
	const kb = 1024
	const mb = 1024 * kb
	if bytes >= mb {
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	}
	return fmt.Sprintf("%d KB", bytes/kb)
}

func slackFileAttachmentsJSON(files []slack.SlackFile) *string {
	if len(files) == 0 {
		return nil
	}
	b, err := json.Marshal(files)
	if err != nil {
		slog.Warn("router: failed to marshal Slack file metadata", "err", err)
		return nil
	}
	s := string(b)
	return &s
}

func slackFilesFromAttachmentsJSON(raw *string) []slack.SlackFile {
	if raw == nil || *raw == "" {
		return nil
	}
	var files []slack.SlackFile
	if err := json.Unmarshal([]byte(*raw), &files); err != nil {
		slog.Warn("router: failed to unmarshal Slack file metadata", "err", err)
		return nil
	}
	return files
}
