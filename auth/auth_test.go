package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
)

// TestTOTPCodeRFC6238 checks the code against RFC 6238's test vectors
// (appendix B, SHA-1, eight digits).
func TestTOTPCodeRFC6238(t *testing.T) {
	key := []byte("12345678901234567890")
	for _, c := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		if got := totpCode(key, totpStep(time.Unix(c.unix, 0)), 8); got != c.want {
			t.Errorf("code at %d = %s, want %s", c.unix, got, c.want)
		}
	}
}

func TestMatchTOTP(t *testing.T) {
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := totpEncoding.DecodeString(secret)
	now := time.Unix(1_800_000_000, 0)
	code := func(offset int64) string { return totpCode(key, totpStep(now)+offset, totpDigits) }

	if step, ok := matchTOTP(secret, code(0), now, 0); !ok || step != totpStep(now) {
		t.Error("the current code isn't accepted")
	}
	if _, ok := matchTOTP(strings.ToLower(secret), code(-1)[:3]+" "+code(-1)[3:], now, 0); !ok {
		t.Error("the previous step's code, spaced as apps show it, isn't accepted")
	}
	if _, ok := matchTOTP(secret, code(2), now, 0); ok {
		t.Error("a code two steps ahead is accepted")
	}
	if _, ok := matchTOTP(secret, code(0), now, totpStep(now)); ok {
		t.Error("a code already used is accepted again")
	}
	if _, ok := matchTOTP(secret, "12345", now, 0); ok {
		t.Error("a five-digit code is accepted")
	}
	if uri := totpURI("alice", secret); !strings.HasPrefix(uri, "otpauth://totp/Solstein:alice?") || !strings.Contains(uri, "secret="+secret) || !strings.Contains(uri, "issuer=Solstein") {
		t.Errorf("uri = %s", uri)
	}
}

func TestPasswordHash(t *testing.T) {
	hash, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(hash, "correct horse") {
		t.Errorf("hash = %s", hash)
	}
	if ok, err := passwordMatches("correct horse battery", hash); err != nil || !ok {
		t.Errorf("the right password doesn't match: %v", err)
	}
	if ok, _ := passwordMatches("correct horse batterY", hash); ok {
		t.Error("a wrong password matches")
	}
	again, _ := hashPassword("correct horse battery")
	if again == hash {
		t.Error("two hashes of one password are the same: no salt")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=0,t=1,p=1$c2FsdA$aGFzaA"} {
		if _, err := passwordMatches("x", bad); !errors.Is(err, errBadHash) {
			t.Errorf("%q: err = %v, want errBadHash", bad, err)
		}
	}
}

func TestLimiter(t *testing.T) {
	limiter, now := newLimiter(), time.Now()
	for range 3 {
		limiter.fail("k", now)
	}
	if blocked, _ := limiter.blocked("k", 4, now); blocked {
		t.Error("blocked below the limit")
	}
	limiter.fail("k", now)
	if blocked, until := limiter.blocked("k", 4, now); !blocked || !until.Equal(now.Add(limitWindow)) {
		t.Errorf("blocked %v until %v, want blocked for the window", blocked, until)
	}
	if blocked, _ := limiter.blocked("k", 4, now.Add(limitWindow)); blocked {
		t.Error("still blocked after the window")
	}
	limiter.fail("k", now)
	limiter.reset("k")
	if blocked, _ := limiter.blocked("k", 1, now); blocked {
		t.Error("still counted after a reset")
	}
}

type clock struct{ now time.Time }

func (clock *clock) Now() time.Time { return clock.now }

func newTestService(t *testing.T) (*Service, *clock, *database.Store) {
	t.Helper()
	store, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	clock := &clock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	service, err := New(store, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return service, clock, store
}

// currentCode is what the user's authenticator shows now.
func currentCode(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	key, err := totpEncoding.DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return totpCode(key, totpStep(now), totpDigits)
}

func TestSignInFlow(t *testing.T) {
	service, clock, store := newTestService(t)
	ctx := context.Background()

	user, oneTime, err := service.AddUser(ctx, " Alice ")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "alice" || len(oneTime) < 20 || strings.Contains(user.PasswordHash, oneTime) {
		t.Fatalf("user %+v, one-time password %q", user, oneTime)
	}
	if _, _, err := service.AddUser(ctx, "alice"); !errors.Is(err, database.ErrUserExists) {
		t.Errorf("second alice: %v", err)
	}
	if _, _, err := service.AddUser(ctx, "not/ok"); !errors.Is(err, ErrInvalidUsername) {
		t.Errorf("bad name: %v", err)
	}

	// The one-time password leads to choosing a password.
	result, err := service.SignIn(ctx, "ALICE", oneTime, "192.0.2.1")
	if err != nil || result.Next != ScopeSetPassword || result.Token == "" {
		t.Fatalf("sign-in with the one-time password: %+v, %v", result, err)
	}
	if _, err := service.Session(ctx, result.Token); !errors.Is(err, ErrNoSession) {
		t.Error("a sign-in token works as a session")
	}
	for _, c := range []struct {
		password, repeat string
		want             error
	}{
		{"long enough password", "long enough passwort", ErrPasswordsDiffer},
		{"short", "short", ErrPasswordRules},
		{oneTime, oneTime, ErrPasswordReused},
	} {
		if _, err := service.SetPassword(ctx, result.Token, c.password, c.repeat); !errors.Is(err, c.want) {
			t.Errorf("SetPassword(%q) = %v, want %v", c.password, err, c.want)
		}
	}
	result, err = service.SetPassword(ctx, result.Token, "long enough password", "long enough password")
	if err != nil || result.Next != "" || result.Token == "" {
		t.Fatalf("set password: %+v, %v", result, err)
	}
	session := result.Token
	if got, err := service.Session(ctx, session); err != nil || got.Username != "alice" {
		t.Fatalf("session: %+v, %v", got, err)
	}
	if _, err := service.SignIn(ctx, "alice", oneTime, "192.0.2.1"); !errors.Is(err, ErrWrongCredentials) {
		t.Error("the one-time password still works")
	}

	// Setting up an authenticator; until confirmed, sign-in doesn't ask.
	secret, uri, err := service.BeginTOTP(ctx, user.ID)
	if err != nil || !strings.Contains(uri, secret) {
		t.Fatal(err)
	}
	if again, _, _ := service.BeginTOTP(ctx, user.ID); again != secret {
		t.Error("reloading the set-up page makes a new secret")
	}
	if err := service.ConfirmTOTP(ctx, user.ID, "000000"); !errors.Is(err, ErrWrongCode) {
		t.Errorf("wrong confirmation code: %v", err)
	}
	if err := service.ConfirmTOTP(ctx, user.ID, currentCode(t, secret, clock.now)); err != nil {
		t.Fatal(err)
	}

	// Now sign-in asks for a code, and a code works once.
	clock.now = clock.now.Add(time.Minute)
	result, err = service.SignIn(ctx, "alice", "long enough password", "192.0.2.1")
	if err != nil || result.Next != ScopeTOTP {
		t.Fatalf("sign-in with TOTP on: %+v, %v", result, err)
	}
	if _, err := service.VerifyTOTP(ctx, result.Token, "000000", "192.0.2.1"); !errors.Is(err, ErrWrongCode) {
		t.Errorf("wrong code: %v", err)
	}
	code := currentCode(t, secret, clock.now)
	signIn := result.Token
	if result, err = service.VerifyTOTP(ctx, signIn, code, "192.0.2.1"); err != nil || result.Token == "" {
		t.Fatalf("right code: %+v, %v", result, err)
	}
	if _, err := service.VerifyTOTP(ctx, signIn, code, "192.0.2.1"); !errors.Is(err, ErrSignInExpired) {
		t.Error("a sign-in token works twice")
	}
	second, _ := service.SignIn(ctx, "alice", "long enough password", "192.0.2.1")
	if _, err := service.VerifyTOTP(ctx, second.Token, code, "192.0.2.1"); !errors.Is(err, ErrWrongCode) {
		t.Error("a TOTP code works twice")
	}

	// Signing out ends the session.
	if err := service.SignOut(ctx, result.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Session(ctx, result.Token); !errors.Is(err, ErrNoSession) {
		t.Error("a signed-out session still works")
	}

	// The console: a lost phone, then a forgotten password; both sign out.
	if ended, err := service.ResetTOTP(ctx, "alice"); err != nil || ended == 0 {
		t.Errorf("reset TOTP ended %d sessions, %v", ended, err)
	}
	if _, err := service.Session(ctx, session); !errors.Is(err, ErrNoSession) {
		t.Error("a session survives a TOTP reset")
	}
	if result, err := service.SignIn(ctx, "alice", "long enough password", "192.0.2.1"); err != nil || result.Next != "" {
		t.Errorf("after a TOTP reset, the password alone should do: %+v, %v", result, err)
	}
	fresh, _, err := service.ResetPassword(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SignIn(ctx, "alice", "long enough password", "192.0.2.1"); !errors.Is(err, ErrWrongCredentials) {
		t.Error("the old password works after a reset")
	}
	clock.now = clock.now.Add(oneTimeLifetime + time.Minute)
	if _, err := service.SignIn(ctx, "alice", fresh, "192.0.2.1"); !errors.Is(err, ErrWrongCredentials) {
		t.Error("a one-time password works after it expired")
	}

	if err := service.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if users, _ := store.ListUsers(ctx); len(users) != 0 {
		t.Errorf("users after delete: %v", users)
	}
}

func TestSignInLimits(t *testing.T) {
	service, clock, _ := newTestService(t)
	ctx := context.Background()
	_, oneTime, _ := service.AddUser(ctx, "bob")

	// An unknown user gets the same answer as a wrong password.
	if _, err := service.SignIn(ctx, "nobody", "whatever", "192.0.2.9"); !errors.Is(err, ErrWrongCredentials) {
		t.Errorf("unknown user: %v", err)
	}
	for range limitPerUser {
		service.SignIn(ctx, "bob", "wrong", "192.0.2.1")
	}
	_, err := service.SignIn(ctx, "bob", oneTime, "192.0.2.2")
	var tooMany TooManyAttempts
	if !errors.As(err, &tooMany) || !errors.Is(err, ErrTooManyAttempts) || !tooMany.Until.After(clock.now) {
		t.Fatalf("after %d failures, even the right password: %v", limitPerUser, err)
	}
	clock.now = clock.now.Add(limitWindow)
	if _, err := service.SignIn(ctx, "bob", oneTime, "192.0.2.2"); err != nil {
		t.Errorf("after the window: %v", err)
	}

	// Per address, across usernames.
	for i := range limitPerIP {
		service.SignIn(ctx, "guess"+string(rune('a'+i%26)), "wrong", "198.51.100.7")
	}
	if _, err := service.SignIn(ctx, "bob", oneTime, "198.51.100.7"); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("after %d failures from one address: %v", limitPerIP, err)
	}
}

func TestSessionExpiry(t *testing.T) {
	service, clock, _ := newTestService(t)
	ctx := context.Background()
	user, oneTime, _ := service.AddUser(ctx, "carol")
	result, _ := service.SignIn(ctx, "carol", oneTime, "192.0.2.1")
	result, err := service.SetPassword(ctx, result.Token, "carol's own password", "carol's own password")
	if err != nil {
		t.Fatal(err)
	}
	session, signedIn := result.Token, clock.now

	// Used every few days, it lasts; but never past SessionMax.
	for elapsed := time.Duration(0); elapsed < SessionMax-SessionIdle; elapsed += 6 * 24 * time.Hour {
		clock.now = clock.now.Add(6 * 24 * time.Hour)
		if _, err := service.Session(ctx, session); err != nil {
			t.Fatalf("after %v of regular use: %v", elapsed, err)
		}
	}
	clock.now = signedIn.Add(SessionMax + time.Minute)
	if _, err := service.Session(ctx, session); !errors.Is(err, ErrNoSession) {
		t.Error("a session outlives SessionMax")
	}

	// Unused for longer than SessionIdle, it ends.
	result, _ = service.SignIn(ctx, "carol", "carol's own password", "192.0.2.1")
	clock.now = clock.now.Add(SessionIdle + time.Minute)
	if _, err := service.Session(ctx, result.Token); !errors.Is(err, ErrNoSession) {
		t.Error("an idle session still works")
	}

	// Changing the password ends the other sessions and hands a new one.
	result, _ = service.SignIn(ctx, "carol", "carol's own password", "192.0.2.1")
	if _, err := service.ChangePassword(ctx, user.ID, "wrong", "a newer password", "a newer password"); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("wrong current password: %v", err)
	}
	replacement, err := service.ChangePassword(ctx, user.ID, "carol's own password", "a newer password", "a newer password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Session(ctx, result.Token); !errors.Is(err, ErrNoSession) {
		t.Error("the old session survives a password change")
	}
	if _, err := service.Session(ctx, replacement); err != nil {
		t.Errorf("the replacement session: %v", err)
	}
}

// TestAccountErrors covers what goes wrong on the account side.
func TestAccountErrors(t *testing.T) {
	service, _, store := newTestService(t)
	ctx := context.Background()
	user, oneTime, _ := service.AddUser(ctx, "dave")
	result, _ := service.SignIn(ctx, "dave", oneTime, "192.0.2.1")
	service.SetPassword(ctx, result.Token, "dave's own password", "dave's own password")

	if err := service.SignOut(ctx, "not-a-session"); err != nil {
		t.Errorf("signing out an unknown session: %v", err)
	}
	if _, err := service.Session(ctx, ""); !errors.Is(err, ErrNoSession) {
		t.Errorf("empty session: %v", err)
	}
	if _, err := service.ChangePassword(ctx, user.ID, "dave's own password", "short", "short"); !errors.Is(err, ErrPasswordRules) {
		t.Errorf("a short new password: %v", err)
	}
	if err := service.DisableTOTP(ctx, user.ID, "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("disable with a wrong password: %v", err)
	}
	if err := service.ConfirmTOTP(ctx, user.ID, "123456"); !errors.Is(err, ErrNoPendingTOTP) {
		t.Errorf("confirm with nothing pending: %v", err)
	}
	if _, _, ok, err := service.PendingTOTP(ctx, user.ID); ok || err != nil {
		t.Errorf("pending with nothing begun: %v, %v", ok, err)
	}
	if _, _, err := service.ResetPassword(ctx, "nobody"); !errors.Is(err, database.ErrUserNotFound) {
		t.Errorf("reset an unknown user: %v", err)
	}
	if _, err := service.ResetTOTP(ctx, "Not Valid!"); !errors.Is(err, ErrInvalidUsername) {
		t.Errorf("reset an invalid name: %v", err)
	}
	if err := service.DeleteUser(ctx, "nobody"); !errors.Is(err, database.ErrUserNotFound) {
		t.Errorf("delete an unknown user: %v", err)
	}
	if got, err := service.User(ctx, user.ID); err != nil || got.Username != "dave" {
		t.Errorf("User = %+v, %v", got, err)
	}
	if err := (TooManyAttempts{}).Error(); err != ErrTooManyAttempts.Error() {
		t.Errorf("TooManyAttempts says %q", err)
	}

	// The store's own not-found answers.
	missing := user
	missing.ID = [16]byte{1}
	if err := store.UpdateUser(ctx, &missing); !errors.Is(err, database.ErrUserNotFound) {
		t.Errorf("update a missing user: %v", err)
	}
	if err := store.DeleteUser(ctx, missing.ID); !errors.Is(err, database.ErrUserNotFound) {
		t.Errorf("delete a missing user: %v", err)
	}
	if err := store.TouchToken(ctx, missing.ID, time.Now(), time.Now()); !errors.Is(err, database.ErrTokenNotFound) {
		t.Errorf("touch a missing token: %v", err)
	}
	if _, err := store.GetUser(ctx, missing.ID); !errors.Is(err, database.ErrUserNotFound) {
		t.Errorf("get a missing user: %v", err)
	}
}
