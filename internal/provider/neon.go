// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/pg"
)

// Neon forks main with a Neon branch (spec §5.2). API paths and payloads
// are documented in docs/providers.md.
type Neon struct {
	apiKey    string
	projectID string
	parent    string
	baseURL   string
	http      *http.Client
	log       *slog.Logger
	poll      time.Duration
	timeout   time.Duration
	// ready waits until the fork accepts connections.
	ready func(ctx context.Context, dsn string) error
}

const defaultNeonAPI = "https://console.neon.tech/api/v2"

// NewNeon returns the Neon provider. It reads NEON_API_KEY and
// NEON_PROJECT_ID; PGWEIR_NEON_API_URL overrides the API base URL.
func NewNeon(opts Options) (Provider, error) {
	n := &Neon{
		apiKey:    os.Getenv("NEON_API_KEY"),
		projectID: os.Getenv("NEON_PROJECT_ID"),
		parent:    opts.NeonParent,
		baseURL:   defaultNeonAPI,
		http:      &http.Client{Timeout: 30 * time.Second},
		log:       opts.Log,
		poll:      2 * time.Second,
		timeout:   5 * time.Minute,
		ready:     waitForConnection,
	}
	if v := os.Getenv(meta.EnvPrefix + "NEON_API_URL"); v != "" {
		n.baseURL = strings.TrimRight(v, "/")
	}
	if n.apiKey == "" || n.projectID == "" {
		return nil, fmt.Errorf("the neon provider needs NEON_API_KEY and NEON_PROJECT_ID")
	}
	return n, nil
}

func (n *Neon) Name() string { return "neon" }

type neonBranch struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Default  bool   `json:"default"`
	ParentID string `json:"parent_id"`
}

type neonEndpoint struct {
	ID           string `json:"id"`
	BranchID     string `json:"branch_id"`
	Host         string `json:"host"`
	CurrentState string `json:"current_state"`
	Type         string `json:"type"`
}

type neonOperation struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Status string `json:"status"`
}

func (n *Neon) Fork(ctx context.Context, mainDSN, name string) (string, map[string]any, error) {
	cfg, err := pgconn.ParseConfig(mainDSN)
	if err != nil {
		return "", nil, fmt.Errorf("parse main connection string: %w", err)
	}
	parent, err := n.resolveParent(ctx, cfg.Host)
	if err != nil {
		return "", nil, err
	}

	var created struct {
		Branch     neonBranch      `json:"branch"`
		Endpoints  []neonEndpoint  `json:"endpoints"`
		Operations []neonOperation `json:"operations"`
	}
	body := map[string]any{
		"branch":    map[string]any{"parent_id": parent, "name": meta.DBPrefix + name},
		"endpoints": []map[string]any{{"type": "read_write"}},
	}
	if err := n.do(ctx, http.MethodPost, n.projectPath("/branches"), body, &created); err != nil {
		return "", nil, fmt.Errorf("create Neon branch: %w", err)
	}
	ref := map[string]any{"project_id": n.projectID, "branch_id": created.Branch.ID}
	if len(created.Endpoints) == 0 {
		return "", ref, n.cleanupAfter(ctx, ref, fmt.Errorf("Neon created branch %s without an endpoint", created.Branch.ID))
	}
	ep := created.Endpoints[0]
	ref["endpoint_id"] = ep.ID
	n.log.Debug("created Neon branch, waiting for it to become ready", "branch", created.Branch.ID, "endpoint", ep.ID)

	wctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	for _, op := range created.Operations {
		if err := n.waitOperation(wctx, op.ID); err != nil {
			return "", ref, n.cleanupAfter(ctx, ref, err)
		}
	}
	q := url.Values{}
	q.Set("branch_id", created.Branch.ID)
	q.Set("endpoint_id", ep.ID)
	q.Set("database_name", cfg.Database)
	q.Set("role_name", cfg.User)
	q.Set("pooled", "false") // replication cannot go through the pooler
	var uri struct {
		URI string `json:"uri"`
	}
	if err := n.do(wctx, http.MethodGet, n.projectPath("/connection_uri")+"?"+q.Encode(), nil, &uri); err != nil {
		return "", ref, n.cleanupAfter(ctx, ref, fmt.Errorf("get connection URI of the Neon branch: %w", err))
	}
	if err := n.ready(wctx, uri.URI); err != nil {
		return "", ref, n.cleanupAfter(ctx, ref, fmt.Errorf("the Neon branch does not accept connections: %w", err))
	}
	return uri.URI, ref, nil
}

func (n *Neon) cleanupAfter(ctx context.Context, ref map[string]any, cause error) error {
	if err := n.Drop(context.WithoutCancel(ctx), ref); err != nil {
		n.log.Warn("could not delete Neon branch after failure", "error", err)
	}
	return cause
}

// resolveParent returns the parent branch ID: --parent (an ID or a name),
// else the project's default branch. When main's host names a Neon endpoint,
// the parent must be the branch of that endpoint.
func (n *Neon) resolveParent(ctx context.Context, mainHost string) (string, error) {
	var list struct {
		Branches []neonBranch `json:"branches"`
	}
	if err := n.do(ctx, http.MethodGet, n.projectPath("/branches"), nil, &list); err != nil {
		return "", fmt.Errorf("list Neon branches: %w", err)
	}
	var parent *neonBranch
	for i, b := range list.Branches {
		switch {
		case n.parent != "" && (b.ID == n.parent || b.Name == n.parent):
			parent = &list.Branches[i]
		case n.parent == "" && b.Default:
			parent = &list.Branches[i]
		}
	}
	if parent == nil {
		if n.parent != "" {
			return "", fmt.Errorf("Neon branch %q not found in project %s", n.parent, n.projectID)
		}
		return "", fmt.Errorf("project %s has no default branch; pass --parent", n.projectID)
	}
	if epID := endpointID(mainHost); epID != "" {
		var ep struct {
			Endpoint neonEndpoint `json:"endpoint"`
		}
		if err := n.do(ctx, http.MethodGet, n.projectPath("/endpoints/"+url.PathEscape(epID)), nil, &ep); err == nil && ep.Endpoint.BranchID != "" && ep.Endpoint.BranchID != parent.ID {
			return "", fmt.Errorf("main (%s) is on Neon branch %s, not on %s (%s); pass --parent %s",
				mainHost, ep.Endpoint.BranchID, parent.ID, parent.Name, ep.Endpoint.BranchID)
		}
	}
	return parent.ID, nil
}

// endpointID extracts "ep-..." from a Neon host name.
func endpointID(host string) string {
	first, _, _ := strings.Cut(host, ".")
	first = strings.TrimSuffix(first, "-pooler")
	if strings.HasPrefix(first, "ep-") {
		return first
	}
	return ""
}

func (n *Neon) waitOperation(ctx context.Context, id string) error {
	for {
		var res struct {
			Operation neonOperation `json:"operation"`
		}
		if err := n.do(ctx, http.MethodGet, n.projectPath("/operations/"+url.PathEscape(id)), nil, &res); err != nil {
			return fmt.Errorf("poll Neon operation %s: %w", id, err)
		}
		switch res.Operation.Status {
		case "finished", "skipped":
			return nil
		case "failed", "error", "cancelled":
			return fmt.Errorf("Neon operation %s (%s) ended with status %s", id, res.Operation.Action, res.Operation.Status)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Neon operation %s: %w", id, ctx.Err())
		case <-time.After(n.poll):
		}
	}
}

func (n *Neon) Drop(ctx context.Context, ref map[string]any) error {
	id, _ := ref["branch_id"].(string)
	project, _ := ref["project_id"].(string)
	if id == "" {
		return fmt.Errorf("provider_ref has no Neon branch_id")
	}
	if project != "" && project != n.projectID {
		return fmt.Errorf("branch belongs to Neon project %s, NEON_PROJECT_ID is %s", project, n.projectID)
	}
	err := n.do(ctx, http.MethodDelete, n.projectPath("/branches/"+url.PathEscape(id)), nil, nil)
	if err != nil {
		return fmt.Errorf("delete Neon branch %s: %w", id, err)
	}
	return nil
}

func (n *Neon) projectPath(p string) string {
	return "/projects/" + url.PathEscape(n.projectID) + p
}

// apiError is a non-2xx response.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Neon API returned %d: %s", e.Status, e.Message)
}

// do sends a request. 423 (operations in progress), 429 and 503 are retried
// with exponential backoff; Neon documents these as safe to retry.
func (n *Neon) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, n.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+n.apiKey)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := n.http.Do(req) // #nosec G107 -- fixed API host or operator override
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		retryable := resp.StatusCode == http.StatusLocked || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable
		if retryable && attempt < 6 {
			n.log.Debug("Neon API busy, retrying", "status", resp.StatusCode, "attempt", attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}
		if resp.StatusCode == http.StatusNoContent || (method == http.MethodDelete && resp.StatusCode == http.StatusNotFound) {
			return nil
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &e)
			if e.Message == "" {
				e.Message = strings.TrimSpace(string(data))
			}
			return &apiError{Status: resp.StatusCode, Message: e.Message}
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("decode Neon response: %w", err)
			}
		}
		return nil
	}
}

// waitForConnection retries connecting until the server accepts the
// connection (a fresh compute may still be starting).
func waitForConnection(ctx context.Context, dsn string) error {
	var last error
	for {
		conn, err := pg.Connect(ctx, dsn)
		if err == nil {
			_ = conn.Close(ctx)
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), last)
		case <-time.After(2 * time.Second):
		}
	}
}
