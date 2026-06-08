package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"github.com/format5/switchboard/internal/agent"
	"github.com/format5/switchboard/internal/slack"
)

type slackFileDownloader func(context.Context, slack.SlackFile) ([]byte, error)

func imagesFromSlackFiles(ctx context.Context, files []slack.SlackFile, download slackFileDownloader) []agent.Image {
	if len(files) == 0 || download == nil {
		return nil
	}

	images := make([]agent.Image, 0, len(files))
	for _, file := range files {
		mediaType := file.MimeType
		if mediaType == "" && file.Name != "" {
			mediaType = mime.TypeByExtension(filepath.Ext(file.Name))
		}
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
