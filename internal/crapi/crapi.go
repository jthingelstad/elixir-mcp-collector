// Package crapi is the only code in the system that speaks to
// api.clashroyale.com. Single attempt per lease: errors are posted as
// results and the scheduler replans — retry policy lives in one place.
//
// The PATH is the hub's: every lease carries cr_path, computed by
// packages/contracts in the hub from the job's endpoint and entity, and
// this package fetches exactly that. It used to hold its own copy of the
// path table as well, unreferenced, with a "?limit=100" baked into the
// ranking paths - which is how a limit nobody remembered choosing read
// as a collector bug for a day (2026-09-11). One owner, no copy.
package crapi

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	base      = "https://api.clashroyale.com/v1"
	timeoutMs = 15_000
)

// Result mirrors the Node fetcher's shape: Kind "http" or "transport".
type Result struct {
	Kind              string
	Status            int
	BodyText          string
	RetryAfterSeconds *int
	Message           string
}

type Fetcher struct {
	token     string
	userAgent string
	client    *http.Client
}

// New builds the one CR API client. version is the release tag the build
// stamped (or "dev"): Supercell sees this string on every request, and
// "Elixir-MCP-Gateway/0.1" - the old name, no real version - is what it
// had been seeing since the rename.
func New(token, version string) *Fetcher {
	return &Fetcher{
		token:     token,
		userAgent: "Elixir-MCP-Collector/" + version + " (+https://elixir.poapkings.com/docs/operators)",
		client:    &http.Client{Timeout: timeoutMs * time.Millisecond},
	}
}

func (f *Fetcher) Fetch(ctx context.Context, path string) Result {
	req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		return Result{Kind: "transport", Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("User-Agent", f.userAgent)
	res, err := f.client.Do(req)
	if err != nil {
		return Result{Kind: "transport", Message: err.Error()}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return Result{Kind: "transport", Message: err.Error()}
	}
	out := Result{Kind: "http", Status: res.StatusCode, BodyText: string(body)}
	if ra := res.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil {
			out.RetryAfterSeconds = &n
		}
	}
	return out
}
