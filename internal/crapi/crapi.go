// Package crapi is the only code in the system that speaks to
// api.clashroyale.com. Single attempt per lease: errors are posted as
// results and the scheduler replans — retry policy lives in one place.
package crapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	base      = "https://api.clashroyale.com/v1"
	timeoutMs = 15_000
)

// Job is the leased fetch-job envelope (canonical shape:
// packages/contracts in jthingelstad/elixir-mcp; wire names exact).
type Job struct {
	Endpoint  string `json:"endpoint"`
	EntityKey string `json:"entity_key"`
	Lane      string `json:"lane,omitempty"`
}

func enc(s string) string { return url.PathEscape(s) }

// Path maps a job to its CR API path; unknown endpoints error BEFORE a
// CR call is spent (malformed jobs re-lease toward the DLQ).
func Path(j Job) (string, error) {
	switch j.Endpoint {
	case "player":
		return "/players/" + enc(j.EntityKey), nil
	case "player_battlelog":
		return "/players/" + enc(j.EntityKey) + "/battlelog", nil
	case "clan":
		return "/clans/" + enc(j.EntityKey), nil
	case "currentriverrace":
		return "/clans/" + enc(j.EntityKey) + "/currentriverrace", nil
	case "riverracelog":
		return "/clans/" + enc(j.EntityKey) + "/riverracelog", nil
	case "cards":
		return "/cards", nil
	case "rankings_players":
		return "/locations/" + enc(j.EntityKey) + "/rankings/players?limit=100", nil
	case "rankings_pol":
		return "/locations/" + enc(j.EntityKey) + "/pathoflegend/players?limit=100", nil
	}
	return "", fmt.Errorf("no CR path for endpoint: %s", j.Endpoint)
}

// Result mirrors the Node fetcher's shape: Kind "http" or "transport".
type Result struct {
	Kind              string
	Status            int
	BodyText          string
	RetryAfterSeconds *int
	Message           string
}

type Fetcher struct {
	token  string
	client *http.Client
}

func New(token string) *Fetcher {
	return &Fetcher{
		token:  token,
		client: &http.Client{Timeout: timeoutMs * time.Millisecond},
	}
}

func (f *Fetcher) Fetch(ctx context.Context, path string) Result {
	req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		return Result{Kind: "transport", Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("User-Agent", "Elixir-MCP-Gateway/0.1")
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
