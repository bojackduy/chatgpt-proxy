package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
)

// Lane identity must come from the client, not from content or tool counts.
// Main retains the legacy session key; auxiliary journals cannot alter it.
func completionSessionID(r *http.Request) (string, string, error) {
	id := sessionID(r)
	lane := r.Header.Get("X-OpenCode-Request-Lane")
	switch lane {
	case "", "main":
		return id, "main", nil
	case "title", "summary", "compaction", "auxiliary":
		if id == "" {
			return "", lane, nil
		}
		return fmt.Sprintf("aux:%x", sha256.Sum256([]byte(id+"\x00"+lane))), lane, nil
	default:
		return "", "", errors.New("unknown X-OpenCode-Request-Lane")
	}
}
