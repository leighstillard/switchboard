// Package outbound provides a rate-limited send queue for outbound messages
// to Slack and other destinations.
package outbound

// SlackPoster abstracts the Slack API methods needed by the outbound queue.
// The slack.Edge type satisfies this interface.
type SlackPoster interface {
	PostMessage(channelID, text string, opts ...PostOption) (ts string, err error)
	UpdateMessage(channelID, ts, text string, opts ...PostOption) error
	UploadFile(channelID, threadTS, filename string, content []byte) error
	AddReaction(channelID, ts, emoji string) error
	RemoveReaction(channelID, ts, emoji string) error
}

// PostOption configures optional parameters for outbound Slack messages.
type PostOption struct {
	ThreadTS string
	Username string
	IconURL  string
	Blocks   []map[string]interface{}
}
