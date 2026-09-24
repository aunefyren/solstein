package server

import (
	"crypto/subtle"
	"net/http"
	"net/netip"
	"strings"

	"aunefyren/solstein/logger"

	"github.com/gin-gonic/gin"
)

// access holds what the handlers need to decide who may do what.
type access struct {
	disableAuth    bool
	token          string
	clientNetworks []netip.Prefix // empty allows every address
	trustedProxies []netip.Prefix
}

func parsePrefixes(networks []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(networks))
	for _, network := range networks {
		prefix, err := netip.ParsePrefix(network)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func containsAddress(prefixes []netip.Prefix, address string) bool {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// tokenValid compares in constant time, so the token can't be guessed a
// character at a time from response timings.
func (access access) tokenValid(provided string) bool {
	if access.disableAuth {
		return true
	}
	return provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(access.token)) == 1
}

// clientNetworkCheck refuses clients outside allowed_client_networks. The
// health check is exempt, so container health checks keep working.
func (access access) clientNetworkCheck() gin.HandlerFunc {
	return func(context *gin.Context) {
		if len(access.clientNetworks) == 0 || context.Request.URL.Path == healthPath {
			context.Next()
			return
		}
		if !containsAddress(access.clientNetworks, context.ClientIP()) {
			logger.Log.Warn("Refused request from " + context.ClientIP() + ", which is outside allowed_client_networks.")
			context.JSON(http.StatusForbidden, gin.H{"error": "Forbidden."})
			context.Abort()
			return
		}
		context.Next()
	}
}

// requireToken guards the feed API. The token can be sent as a bearer token
// (for scripts) or a token query parameter (for a quick check in a browser).
func (access access) requireToken() gin.HandlerFunc {
	return func(context *gin.Context) {
		provided := context.Query("token")
		if header := context.GetHeader("Authorization"); strings.HasPrefix(header, "Bearer ") {
			provided = strings.TrimPrefix(header, "Bearer ")
		}
		if !access.tokenValid(provided) {
			logger.Log.Warn("Refused feed API request from " + context.ClientIP() + " with a missing or wrong token.")
			context.JSON(http.StatusUnauthorized, gin.H{"error": "Missing or invalid token."})
			context.Abort()
			return
		}
		context.Next()
	}
}

// baseURL is the scheme and host clients reach Solstein at: external_url if
// set, else worked out from the request. X-Forwarded-Proto and
// X-Forwarded-Host are believed only from trusted proxies, so a client can't
// make Solstein write links to another host.
func (access access) baseURL(context *gin.Context, externalURL string) string {
	if externalURL != "" {
		return externalURL
	}
	scheme := "http"
	if context.Request.TLS != nil {
		scheme = "https"
	}
	host := context.Request.Host
	if containsAddress(access.trustedProxies, context.RemoteIP()) {
		if forwarded := context.GetHeader("X-Forwarded-Proto"); forwarded == "http" || forwarded == "https" {
			scheme = forwarded
		}
		if forwarded := context.GetHeader("X-Forwarded-Host"); forwarded != "" {
			host = forwarded
		}
	}
	return scheme + "://" + host
}
