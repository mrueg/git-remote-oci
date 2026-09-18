package oci

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Credential resolution and the explanation of authentication failures.

// authOrigin identifies which credential answered the registry's challenge, so
// an authentication failure can say which one to go and fix.
//
// It is an enum with a String method rather than string constants because
// constants named after credentials trip gosec's hardcoded-credential check,
// and silencing that check here would silence it for the whole file.
type authOrigin int

const (
	originAnonymous authOrigin = iota
	originEnvBearer
	originEnvToken
	originEnvUserPass
	originDockerStore
)

func (o authOrigin) String() string {
	switch o {
	case originEnvBearer:
		return "the OCI_BEARER_TOKEN environment variable"
	case originEnvToken:
		return "the OCI_TOKEN environment variable"
	case originEnvUserPass:
		return "the OCI_USERNAME/OCI_PASSWORD environment variables"
	case originDockerStore:
		return "the Docker credential store (~/.docker/config.json or a credential helper)"
	default:
		return "anonymous access"
	}
}

// IsAuthError reports whether err is the registry rejecting our credentials.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var errResp *errcode.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
	}
	var codeErr errcode.Error
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case errcode.ErrorCodeUnauthorized, errcode.ErrorCodeDenied:
			return true
		}
	}
	// A Basic challenge with nothing to answer it is rejected by the auth
	// client before a request is ever sent, so it carries no HTTP status.
	return strings.Contains(err.Error(), "credential not found")
}

// explainAuth annotates an authentication failure with the credential that was
// actually used.
//
// A bare "401 Unauthorized" gives no clue whether the request went out
// anonymously, with a stale token from the environment, or with something the
// Docker credential store produced - which is exactly what the user needs to
// know, and is invisible from the outside because the resolution order is
// internal.
func (c *Client) explainAuth(err error) error {
	if !IsAuthError(err) {
		return err
	}
	source, _ := c.authFrom.Load().(authOrigin)

	// Rejected after the registry had already been serving this client: the
	// credential was good and has stopped being good. Saying "they may be
	// wrong" here would be actively misleading — they demonstrably were not.
	if c.authWorked.Load() {
		if source == originEnvBearer || source == originEnvToken {
			return fmt.Errorf("%w (the token from %s was accepted earlier in this operation and is now being rejected, so it has most likely expired; "+
				"a static token cannot be renewed automatically, so reissue it and run the command again)", err, source)
		}
		return fmt.Errorf("%w (the credentials from %s were accepted earlier in this operation and are now being rejected; "+
			"the registry session has most likely expired, and re-authenticating — `docker login`, or fresh OCI_USERNAME/OCI_PASSWORD — should clear it)", err, source)
	}

	if source == originAnonymous {
		return fmt.Errorf("%w (the request was made anonymously; set OCI_USERNAME and OCI_PASSWORD, or OCI_BEARER_TOKEN, or run `docker login` for this registry)", err)
	}
	return fmt.Errorf("%w (the credentials came from %s and were never accepted by this registry; they may be wrong or lack access to this repository)", err, source)
}

// noteResponse records whether the registry is accepting this client.
//
// Only a 2xx against this repository counts. This used to accept any non-5xx
// from anywhere, which included the token endpoint's 200 for a credential with
// no access to the repository and the /v2/ ping's 200 for one with no scope at
// all -- after which the repository's own 401 was explained as a session that
// had expired, when the credentials had simply never had access.
func (c *Client) noteResponse(resp *http.Response) {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	u := resp.Request.URL
	if u.Host != c.Repo.Reference.Host() {
		return
	}
	if !strings.HasPrefix(u.Path, "/v2/"+c.Repo.Reference.Repository+"/") {
		return
	}
	c.authWorked.Store(true)
}
