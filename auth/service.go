// Package auth signs people in to the web UI: users with argon2id passwords
// and optional TOTP, and the tokens Solstein issues them, built along OAuth
// 2.0 lines (opaque bearer tokens stored as hashes, with kinds, scopes and
// expiry) so personal access tokens and OIDC sign-in can be added later
// without a new model (docs/sign-in.md). Users are added and reset on the
// console only.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// Token kinds.
const (
	// KindSession is a signed-in web UI session, carried in a cookie.
	KindSession = "session"
	// KindSignIn is a sign-in in progress: the password was right, and a
	// step is left (choosing a password, a TOTP code).
	KindSignIn = "sign-in"
)

// Scopes. A session has ScopeUI; a sign-in token carries the steps left.
const (
	ScopeUI          = "ui"
	ScopeSetPassword = "set-password"
	ScopeTOTP        = "totp"
)

const (
	// SessionIdle is how long a session lasts without use; each use moves
	// it on, up to SessionMax after sign-in.
	SessionIdle = 7 * 24 * time.Hour
	SessionMax  = 30 * 24 * time.Hour
	// signInLifetime is how long the steps after the password may take.
	signInLifetime = 10 * time.Minute
	// oneTimeLifetime is how long a console one-time password works.
	oneTimeLifetime = 24 * time.Hour
	// touchEvery limits session writes: its last use is recorded at most
	// this often.
	touchEvery = 5 * time.Minute

	minPasswordLength = 10
	maxPasswordLength = 256
)

var (
	// ErrWrongCredentials is a wrong username or password, deliberately
	// not saying which.
	ErrWrongCredentials = errors.New("wrong username or password")
	ErrWrongCode        = errors.New("wrong or already used code")
	ErrWrongPassword    = errors.New("wrong password")
	// ErrTooManyAttempts refuses further tries for a while; see
	// TooManyAttempts for until when.
	ErrTooManyAttempts = errors.New("too many failed attempts")
	ErrNoSession       = errors.New("not signed in, or the session expired")
	ErrSignInExpired   = errors.New("the sign-in took too long; start again")
	ErrInvalidUsername = errors.New("a username is 1 to 64 lower-case letters, digits, dots, dashes or underscores")
	ErrPasswordRules   = fmt.Errorf("a password is %d to %d characters", minPasswordLength, maxPasswordLength)
	ErrPasswordsDiffer = errors.New("the two passwords differ")
	ErrPasswordReused  = errors.New("choose a password other than the one-time password")
	ErrNoPendingTOTP   = errors.New("no authenticator is being set up")
)

// TooManyAttempts is ErrTooManyAttempts with when trying is allowed again.
type TooManyAttempts struct {
	Until time.Time
}

func (err TooManyAttempts) Error() string { return ErrTooManyAttempts.Error() }
func (err TooManyAttempts) Is(target error) bool {
	return target == ErrTooManyAttempts
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// Service is the sign-in system. It is safe for concurrent use.
type Service struct {
	store   *database.Store
	now     func() time.Time
	limiter *limiter
	// dummyHash is verified against when a username doesn't exist, so a
	// wrong name takes as long as a wrong password.
	dummyHash string
}

// New builds the service; now is nil for the real clock.
func New(store *database.Store, now func() time.Time) (*Service, error) {
	if now == nil {
		now = time.Now
	}
	dummy, err := hashPassword(rand.Text())
	if err != nil {
		return nil, err
	}
	return &Service{store: store, now: now, limiter: newLimiter(), dummyHash: dummy}, nil
}

// NormaliseUsername lower-cases and checks a username.
func NormaliseUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return "", ErrInvalidUsername
	}
	return username, nil
}

// Result is where a sign-in stands after a step: Next is the step left
// (ScopeSetPassword, ScopeTOTP) with Token the sign-in token to carry to it,
// or empty with Token the new session.
type Result struct {
	User  models.User
	Next  string
	Token string
}

// --- Console ---

// AddUser creates a user with a one-time password, which they replace at
// their first sign-in.
func (service *Service) AddUser(ctx context.Context, username string) (models.User, string, error) {
	username, err := NormaliseUsername(username)
	if err != nil {
		return models.User{}, "", err
	}
	user := models.User{Username: username}
	oneTime, err := service.setOneTimePassword(&user)
	if err != nil {
		return models.User{}, "", err
	}
	if err := service.store.CreateUser(ctx, &user); err != nil {
		return models.User{}, "", err
	}
	return user, oneTime, nil
}

// ResetPassword gives a user a new one-time password and signs them out
// everywhere; their authenticator stays as it is. It returns how many
// sessions were ended.
func (service *Service) ResetPassword(ctx context.Context, username string) (string, int64, error) {
	user, err := service.user(ctx, username)
	if err != nil {
		return "", 0, err
	}
	oneTime, err := service.setOneTimePassword(&user)
	if err != nil {
		return "", 0, err
	}
	if err := service.store.UpdateUser(ctx, &user); err != nil {
		return "", 0, err
	}
	ended, err := service.store.DeleteUserTokens(ctx, user.ID)
	return oneTime, ended, err
}

// ResetTOTP removes a user's authenticator (a lost phone) and signs them out
// everywhere; they sign in with their password alone until they set one up
// again.
func (service *Service) ResetTOTP(ctx context.Context, username string) (int64, error) {
	user, err := service.user(ctx, username)
	if err != nil {
		return 0, err
	}
	user.TOTPEnabled, user.TOTPSecret, user.TOTPPending, user.TOTPLastStep = false, "", "", 0
	if err := service.store.UpdateUser(ctx, &user); err != nil {
		return 0, err
	}
	return service.store.DeleteUserTokens(ctx, user.ID)
}

// DeleteUser removes a user and all their tokens.
func (service *Service) DeleteUser(ctx context.Context, username string) error {
	user, err := service.user(ctx, username)
	if err != nil {
		return err
	}
	return service.store.DeleteUser(ctx, user.ID)
}

// Users lists every user, by name.
func (service *Service) Users(ctx context.Context) ([]models.User, error) {
	return service.store.ListUsers(ctx)
}

func (service *Service) user(ctx context.Context, username string) (models.User, error) {
	username, err := NormaliseUsername(username)
	if err != nil {
		return models.User{}, err
	}
	return service.store.GetUserByUsername(ctx, username)
}

func (service *Service) setOneTimePassword(user *models.User) (string, error) {
	oneTime := rand.Text()
	hash, err := hashPassword(oneTime)
	if err != nil {
		return "", err
	}
	expires := service.now().Add(oneTimeLifetime)
	user.PasswordHash, user.MustChangePassword, user.PasswordExpiresAt = hash, true, &expires
	return oneTime, nil
}

// --- Signing in ---

// SignIn checks a username and password: the first step. What's left, if
// anything, is in the result. Failed attempts are limited per username and
// per client address.
func (service *Service) SignIn(ctx context.Context, username, password, clientIP string) (Result, error) {
	now := service.now()
	username = strings.ToLower(strings.TrimSpace(username))
	if err := service.checkLimits(username, clientIP, now); err != nil {
		return Result{}, err
	}

	// Nobody's password is that long; refused like a wrong one.
	if len(password) > maxPasswordLength {
		password = password[:maxPasswordLength+1]
	}
	user, err := service.store.GetUserByUsername(ctx, username)
	if errors.Is(err, database.ErrUserNotFound) {
		passwordMatches(password, service.dummyHash) // the same time as a real check
		service.failed(username, clientIP, now)
		return Result{}, ErrWrongCredentials
	}
	if err != nil {
		return Result{}, err
	}
	matches, err := passwordMatches(password, user.PasswordHash)
	if err != nil {
		return Result{}, fmt.Errorf("user %s: %w", user.Username, err)
	}
	if !matches || user.PasswordExpiresAt != nil && now.After(*user.PasswordExpiresAt) {
		service.failed(username, clientIP, now)
		return Result{}, ErrWrongCredentials
	}
	service.limiter.reset("user:" + username)

	var steps []string
	if user.MustChangePassword {
		steps = append(steps, ScopeSetPassword)
	}
	if user.TOTPEnabled {
		steps = append(steps, ScopeTOTP)
	}
	return service.next(ctx, user, steps)
}

// SetPassword is the step after a one-time password: the user chooses their
// own.
func (service *Service) SetPassword(ctx context.Context, signInToken, password, repeat string) (Result, error) {
	token, user, err := service.signInStep(ctx, signInToken, ScopeSetPassword)
	if err != nil {
		return Result{}, err
	}
	if err := checkNewPassword(password, repeat); err != nil {
		return Result{}, err
	}
	if same, _ := passwordMatches(password, user.PasswordHash); same {
		return Result{}, ErrPasswordReused
	}
	if err := service.setPassword(ctx, &user, password); err != nil {
		return Result{}, err
	}
	if err := service.store.DeleteToken(ctx, token.ID); err != nil {
		return Result{}, err
	}
	return service.next(ctx, user, slices.DeleteFunc(slices.Clone(token.Scopes), func(scope string) bool { return scope == ScopeSetPassword }))
}

// VerifyTOTP is the step for a user with an authenticator: the code it shows.
func (service *Service) VerifyTOTP(ctx context.Context, signInToken, code, clientIP string) (Result, error) {
	token, user, err := service.signInStep(ctx, signInToken, ScopeTOTP)
	if err != nil {
		return Result{}, err
	}
	if slices.Contains(token.Scopes, ScopeSetPassword) {
		// Choosing a password comes first.
		return Result{}, ErrSignInExpired
	}
	now := service.now()
	if err := service.checkLimits(user.Username, clientIP, now); err != nil {
		return Result{}, err
	}
	step, ok := matchTOTP(user.TOTPSecret, code, now, user.TOTPLastStep)
	if !ok {
		service.failed(user.Username, clientIP, now)
		return Result{}, ErrWrongCode
	}
	user.TOTPLastStep = step
	if err := service.store.UpdateUser(ctx, &user); err != nil {
		return Result{}, err
	}
	if err := service.store.DeleteToken(ctx, token.ID); err != nil {
		return Result{}, err
	}
	return service.next(ctx, user, nil)
}

// next issues a sign-in token for the steps left, or the session when none
// are.
func (service *Service) next(ctx context.Context, user models.User, steps []string) (Result, error) {
	now := service.now()
	if len(steps) > 0 {
		token, err := service.issue(ctx, user.ID, KindSignIn, steps, now.Add(signInLifetime))
		return Result{User: user, Next: steps[0], Token: token}, err
	}
	// A good moment to clear out expired tokens: sign-ins are rare.
	if _, err := service.store.DeleteExpiredTokens(ctx, now); err != nil {
		return Result{}, err
	}
	token, err := service.issue(ctx, user.ID, KindSession, []string{ScopeUI}, now.Add(SessionIdle))
	if err != nil {
		return Result{}, err
	}
	user.LastSignInAt = &now
	if err := service.store.UpdateUser(ctx, &user); err != nil {
		return Result{}, err
	}
	return Result{User: user, Token: token}, nil
}

// signInStep checks a sign-in token that should allow a step.
func (service *Service) signInStep(ctx context.Context, raw, scope string) (models.Token, models.User, error) {
	token, err := service.store.GetTokenByHash(ctx, KindSignIn, hashToken(raw))
	if errors.Is(err, database.ErrTokenNotFound) {
		return models.Token{}, models.User{}, ErrSignInExpired
	}
	if err != nil {
		return models.Token{}, models.User{}, err
	}
	if service.now().After(token.ExpiresAt) || !slices.Contains(token.Scopes, scope) {
		return models.Token{}, models.User{}, ErrSignInExpired
	}
	user, err := service.store.GetUser(ctx, token.UserID)
	if errors.Is(err, database.ErrUserNotFound) {
		return models.Token{}, models.User{}, ErrSignInExpired
	}
	return token, user, err
}

func (service *Service) checkLimits(username, clientIP string, now time.Time) error {
	for _, check := range []struct {
		key   string
		limit int
	}{{"user:" + username, limitPerUser}, {"ip:" + clientIP, limitPerIP}} {
		if blocked, until := service.limiter.blocked(check.key, check.limit, now); blocked {
			return TooManyAttempts{Until: until}
		}
	}
	return nil
}

func (service *Service) failed(username, clientIP string, now time.Time) {
	service.limiter.fail("user:"+username, now)
	service.limiter.fail("ip:"+clientIP, now)
}

// --- Sessions ---

// Session returns the user a session token belongs to, and records its use.
// Anything else — unknown, expired, another kind of token — is ErrNoSession.
func (service *Service) Session(ctx context.Context, raw string) (models.User, error) {
	if raw == "" {
		return models.User{}, ErrNoSession
	}
	token, err := service.store.GetTokenByHash(ctx, KindSession, hashToken(raw))
	if errors.Is(err, database.ErrTokenNotFound) {
		return models.User{}, ErrNoSession
	}
	if err != nil {
		return models.User{}, err
	}
	now := service.now()
	if now.After(token.ExpiresAt) || now.Sub(token.CreatedAt) > SessionMax {
		return models.User{}, ErrNoSession
	}
	user, err := service.store.GetUser(ctx, token.UserID)
	if errors.Is(err, database.ErrUserNotFound) {
		return models.User{}, ErrNoSession
	}
	if err != nil {
		return models.User{}, err
	}
	if now.Sub(token.LastUsedAt) >= touchEvery {
		expires := now.Add(SessionIdle)
		if limit := token.CreatedAt.Add(SessionMax); expires.After(limit) {
			expires = limit
		}
		if err := service.store.TouchToken(ctx, token.ID, now, expires); err != nil && !errors.Is(err, database.ErrTokenNotFound) {
			return models.User{}, err
		}
	}
	return user, nil
}

// SignOut ends a session; one already gone is not an error.
func (service *Service) SignOut(ctx context.Context, raw string) error {
	token, err := service.store.GetTokenByHash(ctx, KindSession, hashToken(raw))
	if errors.Is(err, database.ErrTokenNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return service.store.DeleteToken(ctx, token.ID)
}

// --- Account ---

// ChangePassword replaces a signed-in user's password, given the current one.
// Their other sessions end; the returned session replaces the current one.
func (service *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, password, repeat string) (string, error) {
	user, err := service.store.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if matches, err := passwordMatches(current, user.PasswordHash); err != nil || !matches {
		return "", ErrWrongPassword
	}
	if err := checkNewPassword(password, repeat); err != nil {
		return "", err
	}
	if err := service.setPassword(ctx, &user, password); err != nil {
		return "", err
	}
	if _, err := service.store.DeleteUserTokens(ctx, user.ID); err != nil {
		return "", err
	}
	result, err := service.next(ctx, user, nil)
	return result.Token, err
}

// BeginTOTP starts setting up an authenticator: a new secret, kept pending
// until ConfirmTOTP gets a code from it. It returns the secret and the
// otpauth:// URI for the QR code.
func (service *Service) BeginTOTP(ctx context.Context, userID uuid.UUID) (secret, uri string, err error) {
	user, err := service.store.GetUser(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if user.TOTPPending == "" {
		if user.TOTPPending, err = newTOTPSecret(); err != nil {
			return "", "", err
		}
		if err := service.store.UpdateUser(ctx, &user); err != nil {
			return "", "", err
		}
	}
	return user.TOTPPending, totpURI(user.Username, user.TOTPPending), nil
}

// PendingTOTP is the authenticator being set up, as BeginTOTP returned it;
// ok is false when none is.
func (service *Service) PendingTOTP(ctx context.Context, userID uuid.UUID) (secret, uri string, ok bool, err error) {
	user, err := service.store.GetUser(ctx, userID)
	if err != nil || user.TOTPPending == "" {
		return "", "", false, err
	}
	return user.TOTPPending, totpURI(user.Username, user.TOTPPending), true, nil
}

// User returns a user by ID.
func (service *Service) User(ctx context.Context, userID uuid.UUID) (models.User, error) {
	return service.store.GetUser(ctx, userID)
}

// ConfirmTOTP turns the pending authenticator on, given a code from it.
func (service *Service) ConfirmTOTP(ctx context.Context, userID uuid.UUID, code string) error {
	user, err := service.store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if user.TOTPPending == "" {
		return ErrNoPendingTOTP
	}
	step, ok := matchTOTP(user.TOTPPending, code, service.now(), 0)
	if !ok {
		return ErrWrongCode
	}
	user.TOTPSecret, user.TOTPPending, user.TOTPEnabled, user.TOTPLastStep = user.TOTPPending, "", true, step
	return service.store.UpdateUser(ctx, &user)
}

// DisableTOTP turns a user's authenticator off, given their password.
func (service *Service) DisableTOTP(ctx context.Context, userID uuid.UUID, password string) error {
	user, err := service.store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if matches, err := passwordMatches(password, user.PasswordHash); err != nil || !matches {
		return ErrWrongPassword
	}
	user.TOTPEnabled, user.TOTPSecret, user.TOTPPending, user.TOTPLastStep = false, "", "", 0
	return service.store.UpdateUser(ctx, &user)
}

func checkNewPassword(password, repeat string) error {
	if password != repeat {
		return ErrPasswordsDiffer
	}
	if length := len([]rune(password)); length < minPasswordLength || length > maxPasswordLength {
		return ErrPasswordRules
	}
	return nil
}

func (service *Service) setPassword(ctx context.Context, user *models.User, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	user.PasswordHash, user.MustChangePassword, user.PasswordExpiresAt = hash, false, nil
	return service.store.UpdateUser(ctx, user)
}

// --- Tokens ---

// issue creates a token and returns it; only its hash is stored.
func (service *Service) issue(ctx context.Context, userID uuid.UUID, kind string, scopes []string, expires time.Time) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(secret)
	now := service.now()
	token := models.Token{Kind: kind, Hash: hashToken(raw), UserID: userID, Scopes: scopes, ExpiresAt: expires, LastUsedAt: now}
	token.CreatedAt = now
	if err := service.store.CreateToken(ctx, &token); err != nil {
		return "", err
	}
	return raw, nil
}

// hashToken is how a token is stored and looked up. A plain SHA-256 is
// enough: tokens are 256 random bits, not guessable like passwords.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
