package linklanding

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	// InviteLandingPathSegment is the reserved segment for account invite links.
	InviteLandingPathSegment = "invite"
	// InviteLandingPath is the fixed landing route for account invite links.
	InviteLandingPath = "/" + InviteLandingPathSegment

	landingPage         = `<!doctype html><html lang="en"><body><main>Open this in Teagram</main></body></html>`
	notFoundPage        = `<!doctype html><html lang="en"><body><main>Not found</main></body></html>`
	methodPage          = `<!doctype html><html lang="en"><body><main>Method not allowed</main></body></html>`
	unavailablePage     = `<!doctype html><html lang="en"><body><main>Temporarily unavailable</main></body></html>`
	contentPolicy       = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	webSignupEnv        = "TG_INVITE_WEB_SIGNUP_URL"
	desktopTargetEnv    = "TG_INVITE_DESKTOP_URL"
	inviteLandingScript = `(()=>{const secret=window.location.hash.slice(1);window.history.replaceState(null,"",window.location.pathname);const valid=secret.length===64&&/^[0-9a-f]{64}$/.test(secret);const title=document.getElementById("invite-title");const message=document.getElementById("invite-message");if(!valid){title.textContent="This invite can’t be used";message.textContent="Ask whoever invited you for a new link.";return;}message.textContent="Use this link to create your Teagram account. It works once, so don’t share it.";const webTarget=new URL(document.querySelector('meta[name="invite-web-target"]').content);webTarget.hash=secret;document.getElementById("web-target").href=webTarget.href;const actions=document.getElementById("invite-actions");actions.hidden=false;const desktopMeta=document.querySelector('meta[name="invite-desktop-target"]');if(desktopMeta){const desktopTarget=new URL(desktopMeta.content);desktopTarget.hash=secret;document.getElementById("desktop-target").href=desktopTarget.href;document.getElementById("desktop-help").hidden=false;}})();`
)

// InviteTargets contains the fixed destinations used by an invite landing page.
type InviteTargets struct {
	WebSignupURL string
	DesktopURL   string
}

var inviteContentPolicy = invitePolicyForScript(inviteLandingScript)

// InviteTargetsFromEnvironment reads the fixed destinations for invite links.
func InviteTargetsFromEnvironment() (InviteTargets, error) {
	webTarget, err := parseTarget(os.Getenv(webSignupEnv), "https")
	if err != nil {
		return InviteTargets{}, fmt.Errorf("%s must be an absolute HTTPS URL without credentials or a fragment", webSignupEnv)
	}
	targets := InviteTargets{WebSignupURL: webTarget}

	desktopTarget := strings.TrimSpace(os.Getenv(desktopTargetEnv))
	if desktopTarget == "" {
		return targets, nil
	}
	targets.DesktopURL, err = parseTarget(desktopTarget, "https", "teagram")
	if err != nil {
		return InviteTargets{}, fmt.Errorf("%s must be an absolute HTTPS or teagram URL without credentials or a fragment", desktopTargetEnv)
	}
	return targets, nil
}

func parseTarget(raw string, allowedSchemes ...string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("target is required")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || strings.Contains(value, "#") ||
		!parsed.IsAbs() || parsed.Opaque != "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", errors.New("target must be an absolute URL without credentials or a fragment")
	}
	for _, scheme := range allowedSchemes {
		if strings.EqualFold(parsed.Scheme, scheme) {
			return parsed.String(), nil
		}
	}
	return "", errors.New("target scheme is not allowed")
}

// NewHandler returns the landing handler using the environment's fixed invite targets.
// If the required target is missing, only the invite route reports unavailable;
// the link-edge command validates configuration before it starts serving.
func NewHandler(logger *slog.Logger) http.Handler {
	targets, err := InviteTargetsFromEnvironment()
	return newHandler(logger, targets, err)
}

// NewHandlerWithTargets returns a handler using already validated fixed targets.
func NewHandlerWithTargets(logger *slog.Logger, targets InviteTargets) http.Handler {
	return newHandler(logger, targets, nil)
}

func newHandler(logger *slog.Logger, targets InviteTargets, targetErr error) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := routeClass(r.URL.Path)
		status, body, invitePage := responseFor(r.Method, r.URL.Path, targets, targetErr)

		if invitePage {
			SetInviteSecurityHeaders(w.Header(), body)
		} else {
			SetSecurityHeaders(w.Header(), body)
		}
		if status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", "GET, HEAD")
		}
		w.WriteHeader(status)

		if r.Method != http.MethodHead {
			if _, err := io.WriteString(w, body); err != nil {
				logger.Error("landing response write failed", "route_class", class, "status", status)
				return
			}
		}
		logger.Info("landing response", "route_class", class, "status", status)
	})
}

func responseFor(method, path string, targets InviteTargets, targetErr error) (int, string, bool) {
	if method != http.MethodGet && method != http.MethodHead {
		return http.StatusMethodNotAllowed, methodPage, false
	}
	if path == "/admin" || strings.HasPrefix(path, "/admin/") {
		return http.StatusNotFound, notFoundPage, false
	}
	if path == InviteLandingPath {
		if targetErr != nil {
			return http.StatusServiceUnavailable, unavailablePage, false
		}
		return http.StatusOK, inviteLandingPage(targets), true
	}
	return http.StatusOK, landingPage, false
}

// StaticBodyForStatus returns the fixed landing body for a supported response.
func StaticBodyForStatus(status int) (string, bool) {
	switch status {
	case http.StatusOK:
		return landingPage, true
	case http.StatusNotFound:
		return notFoundPage, true
	case http.StatusMethodNotAllowed:
		return methodPage, true
	default:
		return "", false
	}
}

// StaticBodyForStatusAndPath returns the fixed body for the route selected by the link edge.
func StaticBodyForStatusAndPath(status int, path string, targets InviteTargets) (string, bool) {
	if status == http.StatusOK && path == InviteLandingPath {
		return inviteLandingPage(targets), true
	}
	return StaticBodyForStatus(status)
}

func inviteLandingPage(targets InviteTargets) string {
	desktopMeta := ""
	desktopAction := ""
	desktopHelp := ""
	if targets.DesktopURL != "" {
		desktopMeta = `<meta name="invite-desktop-target" content="` + html.EscapeString(targets.DesktopURL) + `">`
		desktopAction = `<a class="button secondary" id="desktop-target" href="#">Open in Teagram for Mac</a>`
		desktopHelp = `<p class="fine desktop-help" id="desktop-help" hidden>Don’t have Teagram for Mac? <a href="https://github.com/teagramhq/teagram-desktop/releases">Download it</a>.</p>`
	}
	replacer := strings.NewReplacer(
		"{{WEB_TARGET}}", html.EscapeString(targets.WebSignupURL),
		"{{DESKTOP_META}}", desktopMeta,
		"{{DESKTOP_ACTION}}", desktopAction,
		"{{DESKTOP_HELP}}", desktopHelp,
		"{{SCRIPT}}", inviteLandingScript,
	)
	return replacer.Replace(inviteLandingHTML)
}

func routeClass(path string) string {
	if path == "/admin" || strings.HasPrefix(path, "/admin/") {
		return "landing_other"
	}
	if path == InviteLandingPath || (strings.HasPrefix(path, "/+") && !strings.Contains(path[1:], "/")) {
		return "invite"
	}
	if path == "/" || !strings.HasPrefix(path, "/") {
		return "landing_other"
	}

	segments := strings.Split(path[1:], "/")
	switch {
	case len(segments) == 1 && segments[0] != "":
		return "username"
	case len(segments) == 2 && segments[0] != "" && segments[1] != "":
		return "message"
	case len(segments) == 3 && segments[0] == "c" && segments[1] != "" && segments[2] != "":
		return "message"
	default:
		return "landing_other"
	}
}

// SetSecurityHeaders adds the fixed response headers used by existing landing pages.
func SetSecurityHeaders(header http.Header, body string) {
	setSecurityHeaders(header, body, contentPolicy)
}

// SetInviteSecurityHeaders adds the invite page's hash-pinned inline-script policy.
func SetInviteSecurityHeaders(header http.Header, body string) {
	setSecurityHeaders(header, body, inviteContentPolicy)
}

func setSecurityHeaders(header http.Header, body, policy string) {
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Security-Policy", policy)
	header.Set("Content-Length", strconv.Itoa(len(body)))
}

func invitePolicyForScript(script string) string {
	digest := sha256.Sum256([]byte(script))
	return fmt.Sprintf("default-src 'none'; script-src 'sha256-%s'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'", base64.StdEncoding.EncodeToString(digest[:]))
}

const inviteLandingHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<meta name="referrer" content="no-referrer">
<meta name="color-scheme" content="light dark">
<meta name="theme-color" content="#F7FAF8" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#0A0F1A" media="(prefers-color-scheme: dark)">
<meta name="invite-web-target" content="{{WEB_TARGET}}">
{{DESKTOP_META}}
<title>Teagram invite</title>
<style>
:root{color-scheme:light dark;--page:#F7FAF8;--ink:#15231D;--ink-muted:#4C6257;--accent:#0F7A57;--accent-hover:#0A6144;--on-accent:#FFFFFF;--accent-tint:rgba(15,122,87,.10)}
@media(prefers-color-scheme:dark){:root{--page:#0A0F1A;--ink:#F1F7F3;--ink-muted:#B2C3BA;--accent:#34D399;--accent-hover:#65E8B0;--on-accent:#0A0F1A;--accent-tint:rgba(52,211,153,.12)}}
*{box-sizing:border-box}
body{margin:0;background:var(--page);color:var(--ink);font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif}
main{width:min(26rem,100%);margin:0 auto;padding:clamp(48px,14vh,128px) 20px 48px}
.mark{display:block;width:64px;height:64px}
h1{margin:24px 0 0;font-size:28px;font-weight:600;line-height:1.2;letter-spacing:-.01em}
.message{margin:12px 0 0;color:var(--ink-muted);font-size:17px;line-height:1.6}
.actions{display:grid;gap:12px;margin-top:28px}
.button{display:flex;min-height:48px;align-items:center;justify-content:center;padding:0 24px;border:2px solid transparent;border-radius:999px;font-size:16px;font-weight:600;line-height:1.2;text-align:center;text-decoration:none;transition:background-color 150ms,color 150ms,border-color 150ms}
.primary{background:var(--accent);color:var(--on-accent)}
.primary:hover{background:var(--accent-hover)}
.secondary{border-color:var(--accent);background:transparent;color:var(--accent)}
.secondary:hover{border-color:var(--accent-hover);background:var(--accent-tint);color:var(--accent-hover)}
a:not(.button){color:var(--accent);text-decoration-thickness:1px;text-underline-offset:.18em}
a:not(.button):hover{color:var(--accent-hover);text-decoration-thickness:2px}
a:focus-visible{outline:3px solid var(--accent);outline-offset:3px}
.fine{margin:20px 0 0;color:var(--ink-muted);font-size:15px;line-height:1.5}
[hidden]{display:none!important}
@media(prefers-reduced-motion:reduce){.button{transition:none}}
</style>
</head>
<body>
<main>
<svg class="mark" viewBox="0 0 1024 1024" aria-hidden="true" focusable="false"><defs><linearGradient id="t-primary-gradient" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#3FD49A"></stop><stop offset="1" stop-color="#0E8A63"></stop></linearGradient></defs><rect width="1024" height="1024" rx="228" fill="url(#t-primary-gradient)"></rect><rect x="244" y="244" width="536" height="136" rx="68" fill="#FFFFFF"></rect><path d="M444 316 H580 V700 C580 716 573 731 560 741 L478 806 C465 816 444 807 444 790 Z" fill="#FFFFFF"></path></svg>
<h1 id="invite-title">You’re invited to Teagram</h1>
<p class="message" id="invite-message">Turn on JavaScript to use this invite link.</p>
<div class="actions" id="invite-actions" hidden>
<a class="button primary" id="web-target" href="#">Create account on Teagram Web</a>
{{DESKTOP_ACTION}}
</div>
{{DESKTOP_HELP}}
<p class="fine">New to Teagram? Read about it at <a href="https://teagram.io/">teagram.io</a>.</p>
</main>
<script>{{SCRIPT}}</script>
</body>
</html>`
