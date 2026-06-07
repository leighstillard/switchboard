package claude

import "errors"

var (
	errClosed = errors.New("claude: session is closed")
	errFatal  = errors.New("claude: session is unrecoverable")
)
