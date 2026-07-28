package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ComputeAccountID returns the canonical multi-workspace identity:
// lowercase hex SHA-256 of user_id + "\x00" + space_id.
func ComputeAccountID(userID, spaceID string) string {
	h := sha256.Sum256([]byte(userID + "\x00" + spaceID))
	return hex.EncodeToString(h[:])
}

// EnsureAccountID populates acc.AccountID from UserID+SpaceID if empty.
// Safe to call on legacy JSON that lacks account_id.
func (acc *Account) EnsureAccountID() {
	if acc == nil {
		return
	}
	if acc.AccountID == "" && acc.UserID != "" && acc.SpaceID != "" {
		acc.AccountID = ComputeAccountID(acc.UserID, acc.SpaceID)
	}
}

// ShortSpaceID returns the first 8 hex chars of SpaceID (safe for logs/DTO).
func (acc *Account) ShortSpaceID() string {
	if acc == nil || len(acc.SpaceID) < 8 {
		return acc.SpaceID
	}
	return acc.SpaceID[:8]
}

// AmbiguousEmailError is returned when an email lookup matches multiple workspaces.
type AmbiguousEmailError struct {
	Email string
	Count int
}

func (e *AmbiguousEmailError) Error() string {
	return fmt.Sprintf("ambiguous: %d workspaces found for email %s", e.Count, e.Email)
}
