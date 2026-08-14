package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ghRelease is the subset of GitHub's release payload the generator needs.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

func (r ghRelease) asset(name string) (ghAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ghAsset{}, false
}

// channel classifies a release. Upstream prereleases become "alpha" so an
// operator can opt into them without them ever being picked by default.
func (r ghRelease) channel() string {
	if r.Prerelease {
		return "alpha"
	}
	return "stable"
}

type client struct {
	http  *http.Client
	token string
}

func newClient() *client {
	return &client{
		http:  &http.Client{Timeout: 15 * time.Minute},
		token: os.Getenv("GITHUB_TOKEN"),
	}
}

// get performs an authenticated request. A token is not required, but without
// one the generator will hit GitHub's 60/hour anonymous rate limit partway
// through a full run.
func (c *client) get(ctx context.Context, url string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "meshflash-catalog-gen")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("GET %s: %s (set GITHUB_TOKEN to raise the rate limit)", url, resp.Status)
		}
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, body)
	}
	return resp, nil
}

func (c *client) getJSON(ctx context.Context, url string, out any) error {
	resp, err := c.get(ctx, url, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// releases lists a repository's releases, newest first.
func (c *client) releases(ctx context.Context, repo string, perPage int) ([]ghRelease, error) {
	var out []ghRelease
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", repo, perPage)
	if err := c.getJSON(ctx, url, &out); err != nil {
		return nil, err
	}

	kept := out[:0]
	for _, r := range out {
		if !r.Draft {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// download fetches a URL into a local cache file, reusing it when the size
// already matches. A full generator run pulls hundreds of megabytes; caching
// makes iterating on the generator tolerable.
func (c *client) download(ctx context.Context, url, dest string, expectSize int64) error {
	if st, err := os.Stat(dest); err == nil && (expectSize == 0 || st.Size() == expectSize) {
		return nil
	}

	resp, err := c.get(ctx, url, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
