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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	base      = "https://api.clashroyale.com/v1"
	timeoutMs = 15_000
)

// MaxBodyBytes bounds what one CR response may occupy in memory. No
// legitimate CR response is anywhere near it; the collector used to read
// the whole body and only then compare it against this ceiling (issue
// #6), so a hostile or broken upstream could make it allocate without
// limit. Now the read itself stops at MaxBodyBytes+1, counted after Go's
// transparent gzip decoding, so a small compressed body cannot inflate
// past it either.
const MaxBodyBytes = 8 << 20

// Result mirrors the Node fetcher's shape: Kind "http" or "transport".
type Result struct {
	Kind              string
	Status            int
	BodyText          string
	RetryAfterSeconds *int
	Message           string
	// TooLarge: the API answered with a body over MaxBodyBytes. Status
	// is real; BodyText is empty because the body was not kept.
	TooLarge bool
}

type Fetcher struct {
	token     string
	userAgent string
	client    *http.Client
	base      string // tests point this at a local server
	maxBody   int64
}

// RefuseDowngrade is a CheckRedirect policy for clients that carry a
// bearer: follow redirects (Go already drops Authorization on a hop to
// another host), but never from https to plain http, where the next
// hop's request would cross the network in the clear.
func RefuseDowngrade(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme != "https" && via[len(via)-1].URL.Scheme == "https" {
		return fmt.Errorf("refusing redirect from https to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// New builds the one CR API client. version is the release tag the build
// stamped (or "dev"): Supercell sees this string on every request, and
// "Elixir-MCP-Gateway/0.1" - the old name, no real version - is what it
// had been seeing since the rename.
func New(token, version string) *Fetcher {
	return &Fetcher{
		token:     token,
		userAgent: "Elixir-MCP-Collector/" + version + " (+https://elixir.poapkings.com/docs/operators)",
		client:    &http.Client{Timeout: timeoutMs * time.Millisecond, CheckRedirect: RefuseDowngrade},
		base:      base,
		maxBody:   MaxBodyBytes,
	}
}

func (f *Fetcher) Fetch(ctx context.Context, path string) Result {
	req, err := http.NewRequestWithContext(ctx, "GET", f.base+path, nil)
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
	out := Result{Kind: "http", Status: res.StatusCode}
	// A declared length over the bound is refused before reading a byte;
	// a chunked or compressed body is cut at max+1 as it streams.
	if res.ContentLength > f.maxBody {
		out.TooLarge = true
	} else {
		body, err := io.ReadAll(io.LimitReader(res.Body, f.maxBody+1))
		if err != nil {
			return Result{Kind: "transport", Message: err.Error()}
		}
		if int64(len(body)) > f.maxBody {
			out.TooLarge = true
		} else {
			out.BodyText = string(body)
		}
	}
	if out.TooLarge {
		out.Message = fmt.Sprintf("response body over %d bytes", f.maxBody)
	}
	if ra := res.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil {
			out.RetryAfterSeconds = &n
		}
	}
	return out
}
