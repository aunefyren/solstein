package models

import "time"

// User is someone who can sign in to the web UI (docs/sign-in.md). Users are
// added and reset on the console, never through the web.
type User struct {
	Base
	// Username is stored lower-case, so sign-in doesn't depend on case.
	Username string `json:"username" gorm:"not null;uniqueIndex"`
	// PasswordHash is an argon2id hash in PHC string form, never the
	// password itself.
	PasswordHash string `json:"-"`
	// MustChangePassword is set by a one-time password from the console: the
	// user has to choose their own at the next sign-in.
	MustChangePassword bool `json:"must_change_password"`
	// PasswordExpiresAt is when a one-time password stops working; nil for
	// a password the user chose.
	PasswordExpiresAt *time.Time `json:"-"`

	// TOTPSecret is the base32 secret of the user's authenticator app, set
	// once TOTPEnabled; TOTPPending is a secret being set up, until the user
	// confirms it with a code.
	TOTPSecret  string `json:"-"`
	TOTPPending string `json:"-"`
	TOTPEnabled bool   `json:"totp_enabled"`
	// TOTPLastStep is the time step of the last code accepted, so a code
	// can't be used twice.
	TOTPLastStep int64 `json:"-"`

	LastSignInAt *time.Time `json:"last_sign_in_at"`
}
