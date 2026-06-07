package claude

import "errors"

var (
	errInit        = errors.New("claude: process exited before session init")
	errInitTimeout = errors.New("claude: timed out waiting for session init")
	errClosed      = errors.New("claude: session is closed")
	errFatal       = errors.New("claude: session is unrecoverable")
)
