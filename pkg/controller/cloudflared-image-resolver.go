package controller

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/pkg/errors"
)

// Picks the image the connector runs in place of [CloudflaredConfig.Image].
type ImageResolver interface {
	// Returns the image the connector should run.
	//
	// The image is empty while none is known yet. When err reports a failed
	// lookup, the image is the last one resolved and may be stale.
	Image(ctx context.Context) (string, error)
}

// How long a failed lookup waits before the next attempt.
//
// It is capped by the check interval of the [LatestTagResolver].
const latestTagRetryInterval = 5 * time.Minute

// Finds the newest tag in the configured image's repository.
//
// The registry is consulted at most once per interval. Tags are split on "-"
// and compared segment by segment: segments that parse as a version compare
// as versions, the others lexically. Only tags laid out like the configured
// tag are candidates, i.e. same number of segments and versions in the same
// positions:
//
//   - "2026.9.1" follows "2026.10.0", but not "2026.10.0-arm64" or "latest"
//   - "2026.7.3-host-metrics.1" follows "2026.9.1-host-metrics.1"
//
// The configured tag is always a candidate, so the result is never older
// than it.
//
// A tag without any version segment, such as "latest", names a moving target
// instead: it is resolved to the digest it currently points at, so the
// connector runs "repository:latest@sha256:..." and rolls whenever the tag
// moves.
type LatestTagResolver struct {
	// Configured repository as spelled by the user.
	//
	// A [name.Repository] would normalize "cloudflare/cloudflared" to
	// "index.docker.io/cloudflare/cloudflared", changing the image of the
	// connector without a new release.
	repository string
	tag        name.Tag
	// Whether the tag has a version segment, a tag without one is tracked by
	// digest.
	versioned  bool
	interval   time.Duration
	listTags   func(ctx context.Context, repository name.Repository) ([]string, error)
	headDigest func(ctx context.Context, ref name.Reference) (string, error)
	now        func() time.Time

	mu        sync.Mutex
	resolved  string
	nextCheck time.Time
}

// Builds a [LatestTagResolver] for image.
//
// The image must be a "repository:tag" reference, such as
// "cloudflare/cloudflared:2026.9.1" or "cloudflare/cloudflared:latest".
func NewLatestTagResolver(image string, interval time.Duration) (*LatestTagResolver, error) {
	if interval <= 0 {
		return nil, errors.Errorf("image check interval must be positive, got %s", interval)
	}
	tag, err := name.NewTag(image)
	if err != nil {
		return nil, errors.Wrapf(err, "parse image %q, a repository:tag reference is required to track the latest version", image)
	}
	versioned := false
	for _, segment := range parseTag(tag.TagStr()) {
		versioned = versioned || segment.version != nil
	}
	return &LatestTagResolver{
		repository: strings.TrimSuffix(image, ":"+tag.TagStr()),
		tag:        tag,
		versioned:  versioned,
		interval:   interval,
		listTags:   listRegistryTags,
		headDigest: registryDigest,
		now:        time.Now,
	}, nil
}

// Lists the tags of repository through the registry HTTP API.
//
// It relies on anonymous pull access, see [explainRegistryError].
func listRegistryTags(ctx context.Context, repository name.Repository) ([]string, error) {
	tags, err := remote.List(repository, remote.WithContext(ctx))
	return tags, explainRegistryError(err)
}

// Returns the digest of the manifest ref points at.
//
// It relies on anonymous pull access, see [explainRegistryError]. A HEAD
// request does not count as an image pull against registry rate limits.
func registryDigest(ctx context.Context, ref name.Reference) (string, error) {
	descriptor, err := remote.Head(ref, remote.WithContext(ctx))
	if err != nil {
		return "", explainRegistryError(err)
	}
	return descriptor.Digest.String(), nil
}

// Adds the steps to resolve a refused anonymous request to err.
//
// Private registries are not supported. The hint mentions a missing
// repository too, since registries such as Docker Hub also answer 401 for
// repositories that do not exist.
func explainRegistryError(err error) error {
	var transportErr *transport.Error
	if errors.As(err, &transportErr) &&
		(transportErr.StatusCode == http.StatusUnauthorized || transportErr.StatusCode == http.StatusForbidden) {
		return errors.Wrap(err, "registry requires authentication, only repositories with anonymous pull access "+
			"can be tracked: check that the repository exists and is public, or pin cloudflared.image.tag to a "+
			"version and disable cloudflared.image.useLatest")
	}
	return err
}

// Returns the newest image of the configured repository.
//
// It consults the registry at most once per interval, and retries a failed
// lookup after [latestTagRetryInterval].
func (r *LatestTagResolver) Image(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if now.Before(r.nextCheck) {
		return r.resolved, nil
	}

	resolved, err := r.lookup(ctx)
	if err != nil {
		r.nextCheck = now.Add(min(latestTagRetryInterval, r.interval))
		return r.resolved, err
	}
	r.resolved = resolved
	r.nextCheck = now.Add(r.interval)
	return r.resolved, nil
}

// Asks the registry which image the configured tag currently stands for.
//
// A versioned tag resolves to the newest tag laid out like it, a tag without
// version to the digest it points at, keeping the tag for readability.
func (r *LatestTagResolver) lookup(ctx context.Context) (string, error) {
	if !r.versioned {
		digest, err := r.headDigest(ctx, r.tag)
		if err != nil {
			return "", errors.Wrapf(err, "resolve digest of %s:%s", r.repository, r.tag.TagStr())
		}
		return r.repository + ":" + r.tag.TagStr() + "@" + digest, nil
	}

	tags, err := r.listTags(ctx, r.tag.Context())
	if err != nil {
		return "", errors.Wrapf(err, "list tags of %s", r.repository)
	}
	return r.repository + ":" + newestTag(r.tag.TagStr(), tags), nil
}

// One "-" separated segment of a tag, either a version or plain text.
type tagSegment struct {
	text string
	// Parsed segment, nil when it does not parse as a version.
	version *semver.Version
}

// Splits tag into its "-" separated segments.
func parseTag(tag string) []tagSegment {
	parts := strings.Split(tag, "-")
	segments := make([]tagSegment, len(parts))
	for i, part := range parts {
		segments[i].text = part
		if version, err := semver.NewVersion(part); err == nil {
			segments[i].version = version
		}
	}
	return segments
}

// Reports whether both tags carry versions in the same segments.
func sameLayout(a, b []tagSegment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if (a[i].version == nil) != (b[i].version == nil) {
			return false
		}
	}
	return true
}

// Compares two tags of the same layout segment by segment.
//
// The result follows [strings.Compare]: negative when a is older than b,
// zero when both are equal, positive when a is newer.
func compareTags(a, b []tagSegment) int {
	for i := range a {
		c := strings.Compare(a[i].text, b[i].text)
		if a[i].version != nil {
			c = a[i].version.Compare(b[i].version)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// Returns the newest of tags laid out like configured.
//
// The configured tag is the floor, it is returned when no listed tag is
// newer.
func newestTag(configured string, tags []string) string {
	newest, newestSegments := configured, parseTag(configured)
	for _, tag := range tags {
		segments := parseTag(tag)
		if sameLayout(segments, newestSegments) && compareTags(segments, newestSegments) > 0 {
			newest, newestSegments = tag, segments
		}
	}
	return newest
}
