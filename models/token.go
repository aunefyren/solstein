package models

import (
	"time"

	"github.com/google/uuid"
)

// Token is a credential Solstein issued: an opaque bearer token in the
// OAuth 2.0 sense (RFC 6749, RFC 6750), stored only as a hash. Kind says how
// it's used: a web UI session (a cookie), a sign-in in progress; later
// personal access tokens and OAuth access or refresh tokens, without a new
// table (docs/sign-in.md).
type Token struct {
	Base
	Kind string `json:"kind" gorm:"not null;index"`
	// Hash is the SHA-256 of the token, hex; the token itself is only ever
	// in the client's hands.
	Hash   string    `json:"-" gorm:"not null;uniqueIndex"`
	UserID uuid.UUID `json:"user_id" gorm:"type:varchar(36);not null;index"`
	User   User      `json:"-" gorm:"constraint:OnDelete:CASCADE"`
	// Scopes are what the token allows, as OAuth 2.0 scopes (RFC 6749
	// section 3.3).
	Scopes []string `json:"scopes" gorm:"serializer:json"`
	// Name labels a token its owner created (a personal access token);
	// empty for sessions.
	Name       string    `json:"name"`
	ExpiresAt  time.Time `json:"expires_at" gorm:"index"`
	LastUsedAt time.Time `json:"last_used_at"`
}
