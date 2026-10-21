package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"
)

// authConfig names the Auth0 tenant, application and API that `agentiscaled
// up` signs the owner in with. The application is a native one, so
// its client ID is public and it has no secret.
type authConfig struct {
	issuer   string
	clientID string
	audience string
}

// defaultAuth is agentiscale's hosted sign-in: its Auth0 tenant, the "CLI"
// application with the Device Code grant, and the control service's API.
var defaultAuth = authConfig{
	issuer:   "https://whiteredhouse.eu.auth0.com/",
	clientID: "zPqccXu1JRBfhMKHP3tNcWzFVBNgMaeT",
	audience: "https://control.agentiscale.net",
}

// deviceLogin signs the owner in with the OAuth 2.0 device authorization
// grant (RFC 8628), the way `tailscale up` does: it shows a code and a link,
// opens the browser where there is one, and waits for the owner to approve.
// The access token it returns is for enrollment only and is never stored;
// no refresh token is asked for.
func deviceLogin(ctx context.Context, auth authConfig, out io.Writer) (string, error) {
	base := strings.TrimSuffix(auth.issuer, "/")
	var code struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if _, err := postForm(ctx, base+"/oauth/device/code", url.Values{
		"client_id": {auth.clientID},
		"audience":  {auth.audience},
		"scope":     {"openid email"},
	}, &code); err != nil {
		return "", fmt.Errorf("start sign-in: %w", err)
	}
	link := code.VerificationURIComplete
	if link == "" {
		link = code.VerificationURI
	}
	fmt.Fprintf(out, "To sign in, open\n\n  %s\n\nand check that it shows the code %s.\n", link, code.UserCode)
	if openBrowser(link) {
		fmt.Fprintln(out, "(Opened your browser.)")
	}
	fmt.Fprintln(out, "Waiting for you to sign in...")

	interval := time.Duration(code.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	if code.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		var tok struct {
			AccessToken      string `json:"access_token"`
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_, err := postForm(ctx, base+"/oauth/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {code.DeviceCode},
			"client_id":   {auth.clientID},
		}, &tok)
		var oauthErr *oauthError
		switch {
		case err == nil && tok.AccessToken != "":
			fmt.Fprintln(out, "Signed in.")
			return tok.AccessToken, nil
		case errors.As(err, &oauthErr) && oauthErr.Code == "authorization_pending":
		case errors.As(err, &oauthErr) && oauthErr.Code == "slow_down":
			interval += 5 * time.Second
		case errors.As(err, &oauthErr) && oauthErr.Code == "expired_token":
			return "", errors.New("the sign-in code expired; run 'agentiscaled up' again")
		case errors.As(err, &oauthErr) && oauthErr.Code == "access_denied":
			return "", errors.New("sign-in was cancelled")
		case err != nil:
			return "", fmt.Errorf("sign-in: %w", err)
		default:
			return "", errors.New("sign-in: Auth0 answered without an access token")
		}
		if time.Now().After(deadline) {
			return "", errors.New("the sign-in code expired; run 'agentiscaled up' again")
		}
	}
}

// oauthError is an OAuth error response (RFC 6749 §5.2).
type oauthError struct {
	Code        string
	Description string
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

func postForm(ctx context.Context, endpoint string, form url.Values, into any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return resp.StatusCode, &oauthError{Code: e.Error, Description: e.ErrorDescription}
		}
		return resp.StatusCode, fmt.Errorf("%s", resp.Status)
	}
	return resp.StatusCode, json.Unmarshal(body, into)
}

// openBrowser opens link in the desktop's browser, if there is a desktop. On
// a server without one, the owner opens the printed link on another device.
func openBrowser(link string) bool {
	if os.Getenv("AGENTISCALE_NO_BROWSER") != "" {
		return false
	}
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		cmd = exec.Command("xdg-open", link)
	default:
		return false
	}
	return cmd.Start() == nil
}
