package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewestTag(t *testing.T) {
	// a sample of the tags published for docker.io/cloudflare/cloudflared
	officialTags := []string{
		"1011-4cf462e", "1011-4cf462e-amd64", "1934-f11dea9cb707-arm64",
		"2026.7.3", "2026.8.3-amd64", "2026.8.3-arm64", "2026.9.0",
		"2026.9.1", "2026.9.1-amd64", "2026.9.1-arm64", "latest",
	}
	// sample of the tags published for ghcr.io/strrl/cloudflared
	forkTags := []string{
		"2026.7.3-host-metrics.1", "sha-b1c11e95",
		"2026.9.1-host-metrics.1", "2026.9.1-host-metrics.2", "2026.9.1",
	}

	cases := []struct {
		name       string
		configured string
		tags       []string
		want       string
	}{
		{"official release ignores arch, build and latest tags", "2026.7.3", officialTags, "2026.9.1"},
		{"patched release follows its own layout", "2026.7.3-host-metrics.1", forkTags, "2026.9.1-host-metrics.2"},
		{"versions compare numerically", "2026.9.1", []string{"2026.10.0", "2026.9.1"}, "2026.10.0"},
		{"never older than the configured tag", "2026.9.1", []string{"2026.7.3", "2026.8.0"}, "2026.9.1"},
		{"configured tag missing from the listing is kept", "2026.9.1", nil, "2026.9.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, newestTag(tc.configured, tc.tags))
		})
	}
}

// Answers tag listings and digest lookups for the resolver under test and
// counts how often the registry was consulted.
type fakeRegistry struct {
	tags   []string
	digest string
	err    error
	calls  int
}

func newTestResolver(t *testing.T, image string, registry *fakeRegistry, clock *time.Time) *LatestTagResolver {
	t.Helper()
	resolver, err := NewLatestTagResolver(image, time.Hour)
	require.NoError(t, err)
	resolver.listTags = func(ctx context.Context, repository name.Repository) ([]string, error) {
		registry.calls++
		return registry.tags, registry.err
	}
	resolver.headDigest = func(ctx context.Context, ref name.Reference) (string, error) {
		registry.calls++
		return registry.digest, registry.err
	}
	resolver.now = func() time.Time { return *clock }
	return resolver
}

func TestLatestTagResolverTracksLatestByDigest(t *testing.T) {
	clock := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	registry := &fakeRegistry{digest: "sha256:aaa", tags: []string{"2026.9.1"}}
	resolver := newTestResolver(t, "docker.io/cloudflare/cloudflared:latest", registry, &clock)

	image, err := resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "docker.io/cloudflare/cloudflared:latest@sha256:aaa", image)

	// within the interval the registry is not consulted again
	registry.digest = "sha256:bbb"
	clock = clock.Add(59 * time.Minute)
	image, err = resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "docker.io/cloudflare/cloudflared:latest@sha256:aaa", image)
	assert.Equal(t, 1, registry.calls)

	// after the interval the moved tag is picked up
	clock = clock.Add(time.Minute)
	image, err = resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "docker.io/cloudflare/cloudflared:latest@sha256:bbb", image)
	assert.Equal(t, 2, registry.calls)
}

func TestLatestTagResolverChecksOncePerInterval(t *testing.T) {
	clock := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	registry := &fakeRegistry{tags: []string{"2026.9.0"}}
	resolver := newTestResolver(t, "cloudflare/cloudflared:2026.7.3", registry, &clock)

	image, err := resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "cloudflare/cloudflared:2026.9.0", image)

	// within the interval the registry is not consulted again
	registry.tags = []string{"2026.9.0", "2026.9.1"}
	clock = clock.Add(59 * time.Minute)
	image, err = resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "cloudflare/cloudflared:2026.9.0", image)
	assert.Equal(t, 1, registry.calls)

	// after the interval the new release is picked up
	clock = clock.Add(time.Minute)
	image, err = resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "cloudflare/cloudflared:2026.9.1", image)
	assert.Equal(t, 2, registry.calls)
}

func TestLatestTagResolverKeepsLastImageWhenLookupFails(t *testing.T) {
	clock := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	registry := &fakeRegistry{tags: []string{"2026.9.0"}}
	resolver := newTestResolver(t, "ghcr.io/strrl/cloudflared:2026.7.3", registry, &clock)

	_, err := resolver.Image(context.Background())
	require.NoError(t, err)

	registry.err = errors.New("registry unavailable")
	clock = clock.Add(time.Hour)
	image, err := resolver.Image(context.Background())
	assert.ErrorContains(t, err, "list tags of ghcr.io/strrl/cloudflared: registry unavailable")
	assert.Equal(t, "ghcr.io/strrl/cloudflared:2026.9.0", image)

	// a failed lookup is retried after the retry interval, not the full hour
	registry.err = nil
	registry.tags = []string{"2026.9.1"}
	clock = clock.Add(latestTagRetryInterval - time.Second)
	image, err = resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/strrl/cloudflared:2026.9.0", image)

	clock = clock.Add(time.Second)
	image, err = resolver.Image(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/strrl/cloudflared:2026.9.1", image)
	assert.Equal(t, 3, registry.calls)
}

func TestLatestTagResolverUnknownBeforeFirstLookup(t *testing.T) {
	clock := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	registry := &fakeRegistry{err: errors.New("registry unavailable")}
	resolver := newTestResolver(t, "cloudflare/cloudflared:2026.7.3", registry, &clock)

	image, err := resolver.Image(context.Background())
	assert.Error(t, err)
	assert.Empty(t, image)

	// the error is not repeated until the next attempt
	image, err = resolver.Image(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, image)
	assert.Equal(t, 1, registry.calls)
}

// Writes a registry API error body, as the distribution spec defines it.
func writeRegistryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"access to the requested resource is not authorized"}]}`, code)
}

func TestLatestTagResolverRegistryRequiresAuthentication(t *testing.T) {
	// Challenges every unauthenticated request with a bearer token realm on
	// the same server.
	bearerChallenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="test"`, r.Host))
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED")
	}

	cases := []struct {
		name     string
		handlers map[string]http.HandlerFunc
	}{
		{
			// as ghcr.io answers for a private repository
			name: "token endpoint refuses anonymous access",
			handlers: map[string]http.HandlerFunc{
				"/v2/": bearerChallenge,
				"/token": func(w http.ResponseWriter, r *http.Request) {
					writeRegistryError(w, http.StatusForbidden, "DENIED")
				},
			},
		},
		{
			// as Docker Hub answers for a private or missing repository
			name: "repository refuses the anonymous token",
			handlers: map[string]http.HandlerFunc{
				"/v2/": bearerChallenge,
				"/token": func(w http.ResponseWriter, r *http.Request) {
					_, _ = fmt.Fprint(w, `{"token":"anonymous"}`)
				},
				"/v2/org/cloudflared/": func(w http.ResponseWriter, r *http.Request) {
					writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED")
				},
			},
		},
		{
			name: "registry only accepts basic authentication",
			handlers: map[string]http.HandlerFunc{
				"/v2/": func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
					writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED")
				},
			},
		},
	}
	// a version tag lists the repository, a tag without version resolves its
	// digest, both must explain the refusal
	lookups := []struct {
		tag       string
		operation string
	}{
		{tag: "2026.7.3", operation: "list tags of %s"},
		{tag: "latest", operation: "resolve digest of %s:latest"},
	}
	for _, tc := range cases {
		for _, lookup := range lookups {
			t.Run(tc.name+" for "+lookup.tag, func(t *testing.T) {
				mux := http.NewServeMux()
				for pattern, handler := range tc.handlers {
					mux.HandleFunc(pattern, handler)
				}
				registry := httptest.NewServer(mux)
				t.Cleanup(registry.Close)

				repository := strings.TrimPrefix(registry.URL, "http://") + "/org/cloudflared"
				resolver, err := NewLatestTagResolver(repository+":"+lookup.tag, time.Hour)
				require.NoError(t, err)

				image, err := resolver.Image(context.Background())
				assert.Empty(t, image)
				require.Error(t, err)
				assert.Contains(t, err.Error(), fmt.Sprintf(lookup.operation, repository))
				assert.Contains(t, err.Error(), "registry requires authentication")
				assert.Contains(t, err.Error(), "check that the repository exists and is public, or pin "+
					"cloudflared.image.tag to a version and disable cloudflared.image.useLatest")
			})
		}
	}

	t.Run("other registry errors are reported unchanged", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {})
		mux.HandleFunc("/v2/org/cloudflared/tags/list", func(w http.ResponseWriter, r *http.Request) {
			writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN")
		})
		registry := httptest.NewServer(mux)
		t.Cleanup(registry.Close)

		resolver, err := NewLatestTagResolver(strings.TrimPrefix(registry.URL, "http://")+"/org/cloudflared:2026.7.3", time.Hour)
		require.NoError(t, err)

		_, err = resolver.Image(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NAME_UNKNOWN")
		assert.NotContains(t, err.Error(), "requires authentication")
	})
}

func TestNewLatestTagResolverValidation(t *testing.T) {
	_, err := NewLatestTagResolver("cloudflare/cloudflared:2026.9.1", 0)
	assert.ErrorContains(t, err, "must be positive")

	_, err = NewLatestTagResolver("cloudflare/cloudflared@sha256:0000000000000000000000000000000000000000000000000000000000000000", time.Hour)
	assert.ErrorContains(t, err, "repository:tag reference is required")
}
