package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// PageClient is what `kenaz approve` uses: the same page routes a browser uses, with
// what a browser sends. It exists only for that command, which refuses to run without a
// real terminal. Nothing in the MCP server or its tools uses it, and the control API the
// MCP process can reach has no way to approve.
type PageClient struct {
	// PageURL is the page root, from the relay's listing.
	PageURL string
	HTTP    *http.Client

	origin, csrf, cookie string
}

var csrfMetaRe = regexp.MustCompile(`name="kenaz-csrf" content="([^"]+)"`)

func (c *PageClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Transport: &http.Transport{}} // never through a proxy
}

// open loads the page once, for its token.
func (c *PageClient) open(ctx context.Context) error {
	if c.csrf != "" {
		return nil
	}
	u, err := url.Parse(c.PageURL)
	if err != nil || u.Host == "" {
		return errors.New("approvals: bad page address")
	}
	c.origin = u.Scheme + "://" + u.Host
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.PageURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Sec-Fetch-Site", "none")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	m := csrfMetaRe.FindSubmatch(b)
	if m == nil || len(resp.Cookies()) == 0 {
		return errors.New("approvals: the approval page didn't load")
	}
	c.csrf = string(m[1])
	c.cookie = resp.Cookies()[0].String()
	if i := strings.IndexByte(c.cookie, ';'); i > 0 {
		c.cookie = c.cookie[:i]
	}
	return nil
}

func (c *PageClient) do(ctx context.Context, method, path string, out any) error {
	if err := c.open(ctx); err != nil {
		return err
	}
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+PagePrefix+"api/"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Cookie", c.cookie)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(csrfHeader, c.csrf)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return errors.New(e.Message)
		}
		return fmt.Errorf("approvals: HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}

// View returns what the page shows for id.
func (c *PageClient) View(ctx context.Context, id string) (View, error) {
	var v View
	err := c.do(ctx, http.MethodGet, "requests/"+id, &v)
	return v, err
}

// Approve runs id.
func (c *PageClient) Approve(ctx context.Context, id string) (Status, error) {
	var out struct {
		Status Status `json:"status"`
	}
	err := c.do(ctx, http.MethodPost, "requests/"+id+"/approve", &out)
	return out.Status, err
}

// Deny closes id.
func (c *PageClient) Deny(ctx context.Context, id string) error {
	var out struct {
		Status Status `json:"status"`
	}
	return c.do(ctx, http.MethodPost, "requests/"+id+"/deny", &out)
}
