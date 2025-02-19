/*
Copyright 2015 All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/purell"
	"github.com/go-chi/chi/middleware"
	"github.com/google/uuid"
	gcsrf "github.com/gorilla/csrf"
	"github.com/unrolled/secure"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// normalizeFlags is the options to purell
	normalizeFlags purell.NormalizationFlags = purell.FlagRemoveDotSegments | purell.FlagRemoveDuplicateSlashes
)

// entrypointMiddleware is custom filtering for incoming requests
func entrypointMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		keep := req.URL.Path
		purell.NormalizeURL(req.URL, normalizeFlags)

		// ensure we have a slash in the url
		if !strings.HasPrefix(req.URL.Path, "/") {
			req.URL.Path = "/" + req.URL.Path
		}
		req.RequestURI = req.URL.RawPath
		req.URL.RawPath = req.URL.Path

		// @step: create a context for the request
		scope := &RequestScope{}
		resp := middleware.NewWrapResponseWriter(w, 1)
		start := time.Now()
		next.ServeHTTP(resp, req.WithContext(context.WithValue(req.Context(), contextScopeName, scope)))

		// @metric record the time taken then response code
		latencyMetric.Observe(time.Since(start).Seconds())
		statusMetric.WithLabelValues(fmt.Sprintf("%d", resp.Status()), req.Method).Inc()

		// place back the original uri for proxying request
		req.URL.Path = keep
		req.URL.RawPath = keep
		req.RequestURI = keep
	})
}

// requestIDMiddleware is responsible for adding a request id if none found
func (r *oauthProxy) requestIDMiddleware(header string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if v := req.Header.Get(header); v == "" {
				req.Header.Set(header, uuid.NewString())
			}

			next.ServeHTTP(w, req)
		})
	}
}

// loggingMiddleware is a custom http logger
func (r *oauthProxy) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, span, logger := r.traceSpan(req.Context(), "logging middleware")
		if span != nil {
			defer span.End()
		}

		start := time.Now()
		resp, ok := w.(middleware.WrapResponseWriter)
		if !ok {
			panic("middleware does not implement go-chi.middleware.WrapResponseWriter")
		}
		next.ServeHTTP(resp, req.WithContext(ctx))
		addr := req.RemoteAddr
		logger.Info("client request",
			zap.Duration("latency", time.Since(start)),
			zap.Int("status", resp.Status()),
			zap.Int("bytes", resp.BytesWritten()),
			zap.String("client_ip", addr),
			zap.String("method", req.Method),
			zap.String("path", req.URL.Path),
			zap.String("protocol", req.Proto))
	})
}

// authenticationMiddleware is responsible for verifying the access token
func (r *oauthProxy) authenticationMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx, span, logger := r.traceSpan(req.Context(), "authentication middleware")
			if span != nil {
				defer span.End()
			}

			clientIP := req.RemoteAddr

			// grab the user identity from the request
			user, err := r.getIdentity(req.WithContext(ctx))
			if err != nil {
				logger.Warn("no session found in request, redirecting for authorization", zap.Error(err))
				next.ServeHTTP(w, req.WithContext(r.redirectToAuthorization(w, req.WithContext(ctx))))
				return
			}

			// create the request scope
			scope, ok := req.Context().Value(contextScopeName).(*RequestScope)
			if !ok {
				panic("corrupted context: expected *RequestScope")
			}
			scope.Identity = user
			ctx = context.WithValue(ctx, contextScopeName, scope)

			// step: skip if we are running skip-token-verification
			if r.config.SkipTokenVerification {
				r.log.Warn("skip token verification enabled, skipping verification - TESTING ONLY")
				if user.isExpired() {
					logger.Warn("the session has expired and token verification is switched off",
						zap.String("client_ip", clientIP),
						zap.String("username", user.name),
						zap.String("expired_on", user.expiresAt.String()))

					next.ServeHTTP(w, req.WithContext(r.redirectToAuthorization(w, req.WithContext(ctx))))
					return
				}
				next.ServeHTTP(w, req.WithContext(ctx))
				return
			}

			if err := r.verifyToken(r.client, user.token); err != nil {
				// step: if the error post verification is anything other than a token
				// expired error we immediately throw an access forbidden - as there is
				// something messed up in the token
				if err != ErrAccessTokenExpired {
					logger.Warn("access token failed verification",
						zap.String("client_ip", clientIP),
						zap.Error(err))

					next.ServeHTTP(w, req.WithContext(r.accessForbidden(w, req.WithContext(ctx))))
					return
				}

				// step: check if we are refreshing the access tokens and if not re-auth
				if !r.config.EnableRefreshTokens {
					logger.Warn("session expired and access token refresh is disabled",
						zap.String("client_ip", clientIP),
						zap.String("email", user.name),
						zap.String("expired_on", user.expiresAt.String()))

					next.ServeHTTP(w, req.WithContext(r.redirectToAuthorization(w, req)))
					return
				}

				logger.Info("accces token for user has expired, attempting to refresh the token",
					zap.String("client_ip", clientIP),
					zap.String("email", user.email))

				// step : refresh the token, update user and session
				if err = r.refreshToken(w, req.WithContext(ctx), user); err != nil {
					switch err {
					case ErrEncode, ErrEncryption:
						r.errorResponse(w, req, err.Error(), http.StatusInternalServerError, err)
					default:
						next.ServeHTTP(w, req.WithContext(r.redirectToAuthorization(w, req.WithContext(ctx))))
					}
					return
				}
				// store user in scope
				ctx = context.WithValue(ctx, contextScopeName, scope)
			}

			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

// checkClaim checks whether claim in userContext matches claimName, match. It can be String or Strings claim.
func (r *oauthProxy) checkClaim(user *userContext, claimName string, match *regexp.Regexp, resourceURL string) bool {
	errFields := []zapcore.Field{
		zap.String("claim", claimName),
		zap.String("access", "denied"),
		zap.String("email", user.email),
		zap.String("resource", resourceURL),
	}

	if _, found := user.claims[claimName]; !found {
		r.log.Warn("the token does not have the claim", errFields...)
		return false
	}

	// Check string claim.
	valueStr, foundStr, errStr := user.claims.StringClaim(claimName)
	// We have found string claim, so let's check whether it matches.
	if foundStr {
		if match.MatchString(valueStr) {
			return true
		}
		r.log.Warn("claim requirement does not match claim in token", append(errFields,
			zap.String("issued", valueStr),
			zap.String("required", match.String()),
		)...)

		return false
	}

	// Check strings claim.
	valueStrs, foundStrs, errStrs := user.claims.StringsClaim(claimName)
	// We have found strings claim, so let's check whether it matches.
	if foundStrs {
		for _, value := range valueStrs {
			if match.MatchString(value) {
				return true
			}
		}
		r.log.Warn("claim requirement does not match any claim in token", append(errFields,
			zap.String("issued", fmt.Sprintf("%v", valueStrs)),
			zap.String("required", match.String()),
		)...)

		return false
	}

	// If this fails, the claim is probably float or int.
	if errStr != nil && errStrs != nil {
		r.log.Warn("unable to extract the claim from token (tried string and strings)", append(errFields,
			zap.Error(errStr),
			zap.Error(errStrs),
		)...)
		return false
	}

	r.log.Warn("unexpected error", errFields...)
	return false
}

// admissionMiddleware is responsible for checking the access token against the protected resource
func (r *oauthProxy) admissionMiddleware(resource *Resource) func(http.Handler) http.Handler {
	claimMatches := make(map[string]*regexp.Regexp)
	for k, v := range r.config.MatchClaims {
		claimMatches[k] = regexp.MustCompile(v)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx, span, logger := r.traceSpan(req.Context(), "admission middleware")
			if span != nil {
				defer span.End()
			}

			// we don't need to continue is a decision has been made
			scope, ok := ctx.Value(contextScopeName).(*RequestScope)
			if !ok {
				panic("corrupted context: expected *RequestScope")
			}
			if scope.AccessDenied {
				next.ServeHTTP(w, req)
				return
			}
			user := scope.Identity

			// @step: we need to check the roles
			if !hasAccess(resource.Roles, user.roles, !resource.RequireAnyRole, false) {
				logger.Warn("access denied, invalid roles",
					zap.String("access", "denied"),
					zap.String("email", user.email),
					zap.String("resource", resource.URL),
					zap.String("roles", resource.getRoles()))

				next.ServeHTTP(w, req.WithContext(r.accessForbidden(w, req.WithContext(ctx))))
				return
			}

			// @step: check if we have any groups, the groups are there
			if !hasAccess(resource.Groups, user.groups, false, true) {
				logger.Warn("access denied, invalid groups",
					zap.String("access", "denied"),
					zap.String("email", user.email),
					zap.String("resource", resource.URL),
					zap.String("groups", strings.Join(resource.Groups, ",")))

				next.ServeHTTP(w, req.WithContext(r.accessForbidden(w, req.WithContext(ctx))))
				return
			}

			// step: if we have any claim matching, lets validate the tokens has the claims
			for claimName, match := range claimMatches {
				if !r.checkClaim(user, claimName, match, resource.URL) {
					next.ServeHTTP(w, req.WithContext(r.accessForbidden(w, req.WithContext(ctx))))
					return
				}
			}

			logger.Debug("access permitted to resource",
				zap.String("access", "permitted"),
				zap.String("email", user.email),
				zap.Duration("expires", time.Until(user.expiresAt)),
				zap.String("resource", resource.URL))

			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

// responseHeaderMiddleware is responsible for adding response headers
func (r *oauthProxy) responseHeaderMiddleware(headers map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// @step: inject any custom response headers
			for k, v := range headers {
				w.Header().Set(k, v)
			}

			next.ServeHTTP(w, req)
		})
	}
}

// identityHeadersMiddleware is responsible for adding the authentication headers to upstream
func (r *oauthProxy) identityHeadersMiddleware(custom []string) func(http.Handler) http.Handler {
	// config-driven request header setters
	setters := make([]func(*http.Request, *userContext), 0, 20)

	if r.config.EnableClaimsHeaders {
		setters = append(setters, func(req *http.Request, user *userContext) {
			req.Header.Set("X-Auth-Audience", strings.Join(user.audiences, ","))
			req.Header.Set("X-Auth-Email", user.email)
			req.Header.Set("X-Auth-ExpiresIn", user.expiresAt.String())
			req.Header.Set("X-Auth-Groups", strings.Join(user.groups, ","))
			req.Header.Set("X-Auth-Roles", strings.Join(user.roles, ","))
			req.Header.Set("X-Auth-Subject", user.id)
			req.Header.Set("X-Auth-Userid", user.name)
			req.Header.Set("X-Auth-Username", user.name)
		})
	}

	if r.config.EnableTokenHeader {
		setters = append(setters, func(req *http.Request, user *userContext) {
			req.Header.Set("X-Auth-Token", user.token.Encode())
		})
	}

	if r.config.EnableAuthorizationHeader {
		setters = append(setters, func(req *http.Request, user *userContext) {
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", user.token.Encode()))
		})
	}

	// are we filtering out the cookies to upstream ?
	// NOTE: cookies are actually just redacted
	if !r.config.EnableAuthorizationCookies {
		cookieFilter := []string{r.config.CookieAccessName, r.config.CookieRefreshName}
		setters = append(setters, func(req *http.Request, _ *userContext) {
			_ = filterCookies(req, cookieFilter)
		})
	}

	if r.config.EnableClaimsHeaders {
		customClaims := make(map[string]string)
		for _, x := range custom {
			customClaims[x] = fmt.Sprintf("X-Auth-%s", toHeader(x))
		}
		setters = append(setters, func(req *http.Request, user *userContext) {
			// inject any custom claims
			for claim, header := range customClaims {
				if claim, found := user.claims[claim]; found {
					req.Header.Set(header, fmt.Sprintf("%v", claim))
				}
			}
		})
	}

	setClaimsHeaders := func(req *http.Request, user *userContext) {
		for _, setter := range setters {
			setter(req, user)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			scope, ok := req.Context().Value(contextScopeName).(*RequestScope)
			if !ok {
				panic("corrupted context: expected *RequestScope")
			}
			if scope.Identity != nil {
				user := scope.Identity
				setClaimsHeaders(req, user)
			}
			next.ServeHTTP(w, req)
		})
	}
}

// securityMiddleware performs numerous security checks on the request
func (r *oauthProxy) securityMiddleware(next http.Handler) http.Handler {
	r.log.Info("enabling the security filter middleware",
		zap.Strings("AllowedHosts", r.config.Hostnames),
		zap.Bool("BrowserXssFilter", r.config.EnableBrowserXSSFilter),
		zap.String("ContentSecurityPolicy", r.config.ContentSecurityPolicy),
		zap.Bool("ContentTypeNosniff", r.config.EnableContentNoSniff),
		zap.Bool("FrameDeny", r.config.EnableFrameDeny),
		zap.Bool("StrictTransportSecurity", r.config.EnableSTS || r.config.EnableSTSPreload),
		zap.Bool("StrictTransportSecurity with HSTS preload", r.config.EnableSTSPreload),
	)
	opts := secure.Options{
		AllowedHosts:          r.config.Hostnames,
		BrowserXssFilter:      r.config.EnableBrowserXSSFilter,
		ContentSecurityPolicy: r.config.ContentSecurityPolicy,
		ContentTypeNosniff:    r.config.EnableContentNoSniff,
		FrameDeny:             r.config.EnableFrameDeny,
		SSLProxyHeaders:       map[string]string{"X-Forwarded-Proto": "https"},
		SSLRedirect:           r.config.EnableHTTPSRedirect,
	}
	if r.config.EnableSTS || r.config.EnableSTSPreload {
		opts.STSSeconds = 31536000
		opts.STSIncludeSubdomains = true
		opts.STSPreload = r.config.EnableSTSPreload
	}
	secureFilter := secure.New(opts)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, span, logger := r.traceSpan(req.Context(), "security middleware")
		if span != nil {
			defer span.End()
		}
		if err := secureFilter.Process(w, req.WithContext(ctx)); err != nil {
			logger.Warn("failed security middleware", zap.Error(err))
			next.ServeHTTP(w, req.WithContext(r.accessForbidden(w, req.WithContext(ctx))))
			return
		}

		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

func csrfSameSiteValue(value string) gcsrf.SameSiteMode {
	switch value {
	case SameSiteLax:
		return gcsrf.SameSiteLaxMode
	case SameSiteStrict:
		return gcsrf.SameSiteStrictMode
	case SameSiteNone:
		fallthrough
	default:
		return gcsrf.SameSiteNoneMode
	}
}

func (r *oauthProxy) csrfConfigMiddleware() func(http.Handler) http.Handler {
	if r.config.EnableCSRF {
		// CSRF protection establishes a session scoped CSRF state with an encrypted cookie.
		// Encryption algorithm is AES-256
		r.log.Info("enabling CSRF protection")
		return gcsrf.Protect([]byte(r.config.EncryptionKey),
			gcsrf.CookieName(r.config.CSRFCookieName),
			gcsrf.RequestHeader(r.config.CSRFHeader),
			gcsrf.Domain(r.config.CookieDomain),
			gcsrf.SameSite(csrfSameSiteValue(r.config.SameSiteCookie)),
			gcsrf.HttpOnly(r.config.HTTPOnlyCookie),
			gcsrf.Secure(r.config.SecureCookie),
			gcsrf.Path("/"),
			gcsrf.ErrorHandler(http.HandlerFunc(r.csrfErrorHandler)))

	}
	return nil
}

func (r *oauthProxy) csrfSkipMiddleware() func(next http.Handler) http.Handler {
	// for proxy entrypoints: unconditionnaly skips CSRF check on unsafe methods (e.g. for login or profiling routes)
	if r.config.EnableCSRF {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.Method {
				case "GET", "HEAD", "OPTIONS", "TRACE":
					next.ServeHTTP(w, req)
				default:
					next.ServeHTTP(w, gcsrf.UnsafeSkipCheck(req))
				}
			})
		}
	}
	return func(next http.Handler) http.Handler {
		return next
	}
}

func (r *oauthProxy) csrfSkipResourceMiddleware(resource *Resource) func(http.Handler) http.Handler {
	// skips CSRF check when:
	// - authorization bearer header is used and not cookie
	// - resource config skips CSRF
	if r.config.EnableCSRF {
		if !resource.EnableCSRF {
			// CSRF check managed by proxy check is disabled on this resource
			return func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					next.ServeHTTP(w, gcsrf.UnsafeSkipCheck(req))
				})
			}
		}

		r.log.Info("CSRF check enabled for resource", zap.String("resource", resource.URL))
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				scope, ok := req.Context().Value(contextScopeName).(*RequestScope)
				if !ok {
					panic("corrupted context: expected *RequestScope")
				}

				// not authenticated, CSRF is irrelevant here
				if scope == nil || scope.AccessDenied || scope.Identity == nil {
					next.ServeHTTP(w, req)
					return
				}

				// request credentials come as a bearer token: skip CSRF check
				if scope.Identity.isBearer() {
					next.ServeHTTP(w, gcsrf.UnsafeSkipCheck(req))
					return
				}

				next.ServeHTTP(w, req)
			})
		}
	}
	return func(next http.Handler) http.Handler {
		return next
	}
}

func (r *oauthProxy) csrfHeaderMiddleware() func(next http.Handler) http.Handler {
	if r.config.EnableCSRF {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {

				// skip unauthenticated requests
				scope, ok := req.Context().Value(contextScopeName).(*RequestScope)
				if !ok {
					panic("corrupted context: expected *RequestScope")
				}

				if scope == nil || scope.AccessDenied || scope.Identity == nil {
					// not authenticated, CSRF is irrelevant here
					next.ServeHTTP(w, req)
					return
				}

				// skip requests with credentials in header
				if scope.Identity.isBearer() {
					next.ServeHTTP(w, req)
					return
				}

				// skip redirected responses
				if w.Header().Get("Location") != "" {
					next.ServeHTTP(w, req)
					return
				}

				csrfToken := gcsrf.Token(req)
				if csrfToken == "" {
					next.ServeHTTP(w, req)
					return
				}

				// add CSRF header to all responses
				w.Header().Add(r.config.CSRFHeader, csrfToken)

				next.ServeHTTP(w, req)
			})
		}
	}
	return func(next http.Handler) http.Handler {
		return next
	}
}

func (r *oauthProxy) csrfProtectMiddleware() func(next http.Handler) http.Handler {
	if r.config.EnableCSRF {
		return r.csrf
	}

	return func(next http.Handler) http.Handler {
		return next
	}
}

// proxyDenyMiddleware just block everything
func proxyDenyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sc := req.Context().Value(contextScopeName)
		var scope *RequestScope
		if sc == nil {
			scope = &RequestScope{}
		} else {
			var ok bool
			scope, ok = sc.(*RequestScope)
			if !ok {
				panic("corrupted context: expected *RequestScope")
			}
		}
		scope.AccessDenied = true
		// update the request context
		ctx := context.WithValue(req.Context(), contextScopeName, scope)

		next.ServeHTTP(w, req.WithContext(ctx))
	})
}
