package collector

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const (
	cloudWatchEndpointPrefix = "AWS_ENDPOINT_URL_CLOUDWATCH_"
	stsEndpointPrefix        = "AWS_ENDPOINT_URL_STS_"
)

var endpointRegionPattern = regexp.MustCompile(`^[a-z]{2,3}(-[a-z]+)+-\d+$`)

// Endpoints holds per-region endpoint overrides (e.g. VPC private
// endpoints, whose hostnames differ per region). A region without an
// entry falls back to the SDK's own resolution, which honours
// AWS_ENDPOINT_URL_CLOUDWATCH / AWS_ENDPOINT_URL_STS and otherwise the
// public endpoint.
type Endpoints struct {
	CloudWatch map[string]string
	STS        map[string]string
}

// ParseEndpoints reads AWS_ENDPOINT_URL_CLOUDWATCH_<REGION> and
// AWS_ENDPOINT_URL_STS_<REGION> entries from environ ("KEY=VALUE"
// strings, as returned by os.Environ). <REGION> is the region upper-cased
// with '-' replaced by '_', e.g. US_WEST_2. Empty values are ignored;
// malformed regions or non-https URLs are an error.
func ParseEndpoints(environ []string) (Endpoints, error) {
	ep := Endpoints{CloudWatch: map[string]string{}, STS: map[string]string{}}
	for _, kv := range environ {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" {
			continue
		}
		var dst map[string]string
		var suffix string
		switch {
		case strings.HasPrefix(key, cloudWatchEndpointPrefix):
			dst, suffix = ep.CloudWatch, strings.TrimPrefix(key, cloudWatchEndpointPrefix)
		case strings.HasPrefix(key, stsEndpointPrefix):
			dst, suffix = ep.STS, strings.TrimPrefix(key, stsEndpointPrefix)
		default:
			continue
		}
		region := strings.ToLower(strings.ReplaceAll(suffix, "_", "-"))
		if !endpointRegionPattern.MatchString(region) {
			return Endpoints{}, fmt.Errorf("invalid %s: %q is not a region suffix like US_WEST_2", key, suffix)
		}
		u, err := url.Parse(val)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return Endpoints{}, fmt.Errorf("invalid %s %q: must be an absolute https:// URL", key, val)
		}
		dst[region] = val
	}
	return ep, nil
}

// Regions returns the sorted union of regions with any override, for
// startup logging.
func (e Endpoints) Regions() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range []map[string]string{e.CloudWatch, e.STS} {
		for r := range m {
			if _, ok := seen[r]; !ok {
				seen[r] = struct{}{}
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}
