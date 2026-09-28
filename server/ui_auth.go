package server

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aunefyren/solstein/auth"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"rsc.io/qr"
)

// Sign-in for the web UI (docs/sign-in.md): a session cookie per signed-in
// browser, the sign-in steps (password, choosing one's own after a one-time
// password, a TOTP code), and the account page.

const (
	sessionCookie = "solstein_session"
	// signInCookie carries a sign-in in progress between its steps.
	signInCookie = "solstein_sign_in"
	loginPath    = uiPrefix + "/login"
)

// sessionAuth is the web UI's authenticator: a valid session cookie, or a
// redirect to sign in.
type sessionAuth struct {
	auth *auth.Service
}

func (session sessionAuth) authenticate(context *gin.Context) (uiUser, bool) {
	raw, _ := context.Cookie(sessionCookie)
	user, err := session.auth.Session(context.Request.Context(), raw)
	if err == nil {
		return uiUser{Subject: user.ID.String(), Name: user.Username}, true
	}
	if !errors.Is(err, auth.ErrNoSession) {
		logger.Log.Error("Failed to check a web UI session. Error: " + err.Error())
	}
	if raw != "" {
		clearCookie(context, sessionCookie, uiPrefix)
	}
	// Back to where they were going once signed in; a form post can't be
	// replayed, so that goes to the page it came from.
	next := uiPrefix + "/feeds"
	if context.Request.Method == http.MethodGet {
		next = context.Request.URL.RequestURI()
	}
	context.Redirect(http.StatusSeeOther, loginPath+"?next="+url.QueryEscape(next))
	return uiUser{}, false
}

// safeNext is where to go after signing in: a path within the UI only, so
// the sign-in page can't be used to send someone elsewhere.
func safeNext(next string) string {
	if !strings.HasPrefix(next, uiPrefix+"/") || strings.HasPrefix(next, loginPath) || strings.ContainsAny(next, "\\\r\n") || strings.HasPrefix(next, "//") {
		return uiPrefix + "/feeds"
	}
	return next
}

// secureRequest is whether the browser reached Solstein over HTTPS, directly
// or through a trusted proxy: cookies are then marked Secure.
func (handlers *handlers) secureRequest(context *gin.Context) bool {
	return strings.HasPrefix(handlers.access.baseURL(context, ""), "https://")
}

func (handlers *handlers) setCookie(context *gin.Context, name, value, path string, maxAge time.Duration) {
	http.SetCookie(context.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   handlers.secureRequest(context),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(context *gin.Context, name, path string) {
	http.SetCookie(context.Writer, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// uiSignIn is what the sign-in page shows: which step, and where to go after.
type uiSignIn struct {
	Step    string // "password", "set-password", "totp"
	Next    string
	NoUsers bool
}

func (ui *ui) signInPage(context *gin.Context, status int, step string, notice *uiNotice) {
	content := uiSignIn{Step: step, Next: safeNext(context.Query("next"))}
	if step == "password" {
		if users, err := ui.handlers.auth.Users(context.Request.Context()); err == nil && len(users) == 0 {
			content.NoUsers = true
		}
	}
	ui.render(context, status, "login", "", notice, content)
}

func (ui *ui) loginForm(context *gin.Context) {
	ui.signInPage(context, http.StatusOK, "password", nil)
}

func (ui *ui) loginSubmit(context *gin.Context) {
	username := context.PostForm("username")
	result, err := ui.handlers.auth.SignIn(context.Request.Context(), username, context.PostForm("password"), context.ClientIP())
	if err != nil {
		ui.signInFailed(context, "password", fmt.Sprintf("%q", strings.ToLower(strings.TrimSpace(username))), err)
		return
	}
	ui.continueSignIn(context, result)
}

func (ui *ui) setPasswordForm(context *gin.Context) {
	ui.signInPage(context, http.StatusOK, "set-password", nil)
}

func (ui *ui) setPasswordSubmit(context *gin.Context) {
	raw, _ := context.Cookie(signInCookie)
	result, err := ui.handlers.auth.SetPassword(context.Request.Context(), raw, context.PostForm("password"), context.PostForm("repeat"))
	if err != nil {
		ui.signInFailed(context, "set-password", "a one-time password", err)
		return
	}
	ui.continueSignIn(context, result)
}

func (ui *ui) totpForm(context *gin.Context) {
	ui.signInPage(context, http.StatusOK, "totp", nil)
}

func (ui *ui) totpSubmit(context *gin.Context) {
	raw, _ := context.Cookie(signInCookie)
	result, err := ui.handlers.auth.VerifyTOTP(context.Request.Context(), raw, context.PostForm("code"), context.ClientIP())
	if err != nil {
		ui.signInFailed(context, "totp", "a TOTP code", err)
		return
	}
	ui.continueSignIn(context, result)
}

// continueSignIn sends the browser to the step left, or signs it in.
func (ui *ui) continueSignIn(context *gin.Context, result auth.Result) {
	next := url.QueryEscape(safeNext(context.Query("next")))
	switch result.Next {
	case auth.ScopeSetPassword:
		ui.handlers.setCookie(context, signInCookie, result.Token, loginPath, 10*time.Minute)
		context.Redirect(http.StatusSeeOther, loginPath+"/password?next="+next)
	case auth.ScopeTOTP:
		ui.handlers.setCookie(context, signInCookie, result.Token, loginPath, 10*time.Minute)
		context.Redirect(http.StatusSeeOther, loginPath+"/totp?next="+next)
	default:
		clearCookie(context, signInCookie, loginPath)
		ui.handlers.setCookie(context, sessionCookie, result.Token, uiPrefix, auth.SessionMax)
		logger.Log.Info("Signed in '" + result.User.Username + "' to the web UI from " + context.ClientIP() + ".")
		context.Redirect(http.StatusSeeOther, safeNext(context.Query("next")))
	}
}

// signInFailed shows the step again with what went wrong, in words that
// don't say whether a username exists.
func (ui *ui) signInFailed(context *gin.Context, step, what string, err error) {
	var tooMany auth.TooManyAttempts
	text, status := "", http.StatusUnauthorized
	switch {
	case errors.As(err, &tooMany):
		logger.Log.Warn("Refused sign-in to the web UI for " + what + " from " + context.ClientIP() + ": too many failed attempts.")
		text, status = "Too many failed attempts. Try again after "+tooMany.Until.Local().Format("15:04")+".", http.StatusTooManyRequests
	case errors.Is(err, auth.ErrWrongCredentials):
		logger.Log.Warn("Failed sign-in to the web UI as " + what + " from " + context.ClientIP() + ".")
		text = "Wrong username or password."
	case errors.Is(err, auth.ErrWrongCode):
		logger.Log.Warn("Failed sign-in to the web UI with " + what + " from " + context.ClientIP() + ".")
		text = "Wrong code, or one already used. Wait for the next one and try again."
	case errors.Is(err, auth.ErrSignInExpired):
		clearCookie(context, signInCookie, loginPath)
		ui.signInPage(context, http.StatusUnauthorized, "password", &uiNotice{Kind: "error", Text: "That took too long. Sign in again."})
		return
	case errors.Is(err, auth.ErrPasswordsDiffer), errors.Is(err, auth.ErrPasswordRules), errors.Is(err, auth.ErrPasswordReused):
		text, status = sentenceCase(err.Error())+".", http.StatusBadRequest
	default:
		logger.Log.Error("Failed to sign in to the web UI. Error: " + err.Error())
		text, status = "Something went wrong; the log says what.", http.StatusInternalServerError
	}
	ui.signInPage(context, status, step, &uiNotice{Kind: "error", Text: text})
}

func (ui *ui) logout(context *gin.Context) {
	if raw, err := context.Cookie(sessionCookie); err == nil {
		if err := ui.handlers.auth.SignOut(context.Request.Context(), raw); err != nil {
			logger.Log.Error("Failed to end a web UI session. Error: " + err.Error())
		}
	}
	clearCookie(context, sessionCookie, uiPrefix)
	if user, ok := currentUser(context); ok {
		logger.Log.Info("Signed out '" + user.Name + "' of the web UI.")
	}
	context.Redirect(http.StatusSeeOther, loginPath)
}

func currentUser(context *gin.Context) (uiUser, bool) {
	value, ok := context.Get(uiUserKey)
	user, _ := value.(uiUser)
	return user, ok && !user.Anonymous()
}

// --- Account ---

type uiAccount struct {
	Username    string
	TOTPEnabled bool
	Pending     bool
}

var accountNotices = map[string]string{
	"password": "Changed your password. Other browsers were signed out.",
	"totp-on":  "Your authenticator is set up: signing in now asks for its code.",
	"totp-off": "Removed your authenticator: signing in asks for your password alone.",
}

func (ui *ui) accountPage(context *gin.Context) {
	var notice *uiNotice
	if text, ok := accountNotices[context.Query("done")]; ok {
		notice = &uiNotice{Kind: "ok", Text: text}
	}
	ui.showAccount(context, http.StatusOK, notice)
}

func (ui *ui) showAccount(context *gin.Context, status int, notice *uiNotice) {
	user, ok := ui.accountUser(context)
	if !ok {
		return
	}
	ui.render(context, status, "account", "account", notice, uiAccount{Username: user.Username, TOTPEnabled: user.TOTPEnabled, Pending: user.TOTPPending != ""})
}

func (ui *ui) accountUser(context *gin.Context) (models.User, bool) {
	current, _ := currentUser(context)
	id, err := uuid.Parse(current.Subject)
	if err != nil {
		ui.renderError(context, http.StatusUnauthorized, "Not signed in", "Sign in again.")
		return models.User{}, false
	}
	user, err := ui.handlers.auth.User(context.Request.Context(), id)
	if err != nil {
		logger.Log.Error("Failed to load the web UI user " + current.Name + ". Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't load your account", "Something went wrong; the log says what.")
		return models.User{}, false
	}
	return user, true
}

func (ui *ui) changePassword(context *gin.Context) {
	user, ok := ui.accountUser(context)
	if !ok {
		return
	}
	token, err := ui.handlers.auth.ChangePassword(context.Request.Context(), user.ID, context.PostForm("current"), context.PostForm("password"), context.PostForm("repeat"))
	if err != nil {
		ui.accountFailed(context, err)
		return
	}
	ui.handlers.setCookie(context, sessionCookie, token, uiPrefix, auth.SessionMax)
	logger.Log.Info("'" + user.Username + "' changed their web UI password; their other sessions ended.")
	context.Redirect(http.StatusSeeOther, uiPrefix+"/account?done=password")
}

func (ui *ui) beginTOTP(context *gin.Context) {
	user, ok := ui.accountUser(context)
	if !ok {
		return
	}
	if _, _, err := ui.handlers.auth.BeginTOTP(context.Request.Context(), user.ID); err != nil {
		ui.accountFailed(context, err)
		return
	}
	context.Redirect(http.StatusSeeOther, uiPrefix+"/account/totp")
}

type uiTOTPSetup struct {
	QR     template.HTML
	Secret string
	// URI is an otpauth:// link Solstein made itself, which html/template
	// would otherwise refuse as an unknown scheme.
	URI template.URL
}

func (ui *ui) totpSetupPage(context *gin.Context) {
	ui.showTOTPSetup(context, http.StatusOK, nil)
}

func (ui *ui) showTOTPSetup(context *gin.Context, status int, notice *uiNotice) {
	user, ok := ui.accountUser(context)
	if !ok {
		return
	}
	secret, uri, pending, err := ui.handlers.auth.PendingTOTP(context.Request.Context(), user.ID)
	if err != nil {
		ui.accountFailed(context, err)
		return
	}
	if !pending {
		context.Redirect(http.StatusSeeOther, uiPrefix+"/account")
		return
	}
	code, err := qrSVG(uri)
	if err != nil {
		logger.Log.Error("Failed to make the TOTP QR code. Error: " + err.Error())
	}
	ui.render(context, status, "totp", "account", notice, uiTOTPSetup{QR: code, Secret: groupSecret(secret), URI: template.URL(uri)})
}

func (ui *ui) confirmTOTP(context *gin.Context) {
	user, ok := ui.accountUser(context)
	if !ok {
		return
	}
	err := ui.handlers.auth.ConfirmTOTP(context.Request.Context(), user.ID, context.PostForm("code"))
	if errors.Is(err, auth.ErrWrongCode) {
		ui.showTOTPSetup(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: "That code doesn't match. Check the app shows Solstein's newest code, and that your phone's clock is right."})
		return
	}
	if err != nil {
		ui.accountFailed(context, err)
		return
	}
	logger.Log.Info("'" + user.Username + "' set up an authenticator for the web UI.")
	context.Redirect(http.StatusSeeOther, uiPrefix+"/account?done=totp-on")
}

func (ui *ui) disableTOTP(context *gin.Context) {
	user, ok := ui.accountUser(context)
	if !ok {
		return
	}
	if err := ui.handlers.auth.DisableTOTP(context.Request.Context(), user.ID, context.PostForm("password")); err != nil {
		ui.accountFailed(context, err)
		return
	}
	logger.Log.Info("'" + user.Username + "' removed their web UI authenticator.")
	context.Redirect(http.StatusSeeOther, uiPrefix+"/account?done=totp-off")
}

func (ui *ui) accountFailed(context *gin.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		ui.showAccount(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: "Your current password isn't right."})
	case errors.Is(err, auth.ErrPasswordsDiffer), errors.Is(err, auth.ErrPasswordRules):
		ui.showAccount(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: sentenceCase(err.Error()) + "."})
	default:
		logger.Log.Error("Failed to change a web UI account. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't save that", "Something went wrong; the log says what.")
	}
}

// groupSecret spaces a base32 secret in fours, for typing it in by hand.
func groupSecret(secret string) string {
	var groups []string
	for len(secret) > 4 {
		groups = append(groups, secret[:4])
		secret = secret[4:]
	}
	return strings.Join(append(groups, secret), " ")
}

// qrSVG draws a QR code as inline SVG: a path of its dark modules inside a
// four-module quiet zone. Inline markup, not an image or a script, so the
// content security policy stays as it is; its colours come from the
// stylesheet (style guide, QR code).
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	var path strings.Builder
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	return template.HTML(fmt.Sprintf(`<svg class="qr" viewBox="-4 -4 %d %d" role="img" aria-label="QR code for your authenticator app"><path d="%s"/></svg>`,
		code.Size+8, code.Size+8, path.String())), nil
}
