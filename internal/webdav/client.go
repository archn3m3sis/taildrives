// Package webdav is a minimal WebDAV client for Tailscale Drive.
package webdav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is the magic Tailscale Drive WebDAV address (same on every
// device on the tailnet).
const DefaultEndpoint = "http://100.100.100.100:8080"

// Entry is a single PROPFIND result.
type Entry struct {
	Href  string // absolute path on the WebDAV server, e.g. "/user/device/share/"
	Name  string // basename
	IsDir bool
	Size  int64
}

// Client wraps an http.Client for the Tailscale Drive endpoint.
type Client struct {
	Endpoint string
	HTTP     *http.Client
}

// New returns a client with sensible defaults.
func New(endpoint string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		Endpoint: strings.TrimRight(endpoint, "/"),
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WithTimeout returns a shallow copy of the client with a tighter HTTP timeout.
// Safe for concurrent use because it doesn't mutate the original client.
func (c *Client) WithTimeout(timeout time.Duration) *Client {
	cp := *c
	cp.HTTP = &http.Client{Timeout: timeout}
	return &cp
}

// propfindBody is the XML body we send for every PROPFIND.
const propfindBody = `<?xml version="1.0"?>
<propfind xmlns="DAV:">
  <prop>
    <resourcetype/>
    <getcontentlength/>
    <getlastmodified/>
  </prop>
</propfind>`

// xml structs for parsing the multistatus response.
type multistatus struct {
	XMLName   xml.Name   `xml:"multistatus"`
	Responses []response `xml:"response"`
}
type response struct {
	Href     string     `xml:"href"`
	Propstat []propstat `xml:"propstat"`
}
type propstat struct {
	Prop prop `xml:"prop"`
}
type prop struct {
	ResourceType  *resourceType `xml:"resourcetype"`
	ContentLength string        `xml:"getcontentlength"`
	LastModified  string        `xml:"getlastmodified"`
}
type resourceType struct {
	Collection *struct{} `xml:"collection"`
}

// List does a PROPFIND with Depth:1 and returns the children of the given path
// (excludes the path itself).
func (c *Client) List(path string) ([]Entry, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	req, err := http.NewRequest("PROPFIND", c.Endpoint+escape(path), strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PROPFIND %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 && resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("PROPFIND %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, fmt.Errorf("parse multistatus: %w", err)
	}

	out := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		href, err := url.PathUnescape(r.Href)
		if err != nil {
			href = r.Href
		}
		// Skip the parent itself.
		if pathEqual(href, path) {
			continue
		}
		e := Entry{Href: href}
		// Basename: strip trailing slash then last segment.
		name := strings.TrimRight(href, "/")
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		e.Name = name
		for _, ps := range r.Propstat {
			if ps.Prop.ResourceType != nil && ps.Prop.ResourceType.Collection != nil {
				e.IsDir = true
			}
			if ps.Prop.ContentLength != "" {
				if n, err := strconv.ParseInt(ps.Prop.ContentLength, 10, 64); err == nil {
					e.Size = n
				}
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// Get downloads the file at path.
func (c *Client) Get(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.Endpoint+escape(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// Put uploads body to path.
func (c *Client) Put(path string, body io.Reader, contentLength int64) error {
	req, err := http.NewRequest("PUT", c.Endpoint+escape(path), body)
	if err != nil {
		return err
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT %s: HTTP %d: %s", path, resp.StatusCode, b)
	}
	return nil
}

// Copy issues a server-side WebDAV COPY. Returns nil on success.
// Falls back to ErrUnsupported if the server returned 4xx (caller should
// stream-copy in that case).
var ErrUnsupported = errors.New("server did not support COPY")

func (c *Client) Copy(src, dst string, overwrite bool) error {
	req, err := http.NewRequest("COPY", c.Endpoint+escape(src), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", c.Endpoint+escape(dst))
	if overwrite {
		req.Header.Set("Overwrite", "T")
	} else {
		req.Header.Set("Overwrite", "F")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == 405 || resp.StatusCode == 501 {
		return ErrUnsupported
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("COPY %s → %s: HTTP %d: %s", src, dst, resp.StatusCode, b)
}

// Mkdir creates a collection.
func (c *Client) Mkdir(path string) error {
	req, err := http.NewRequest("MKCOL", c.Endpoint+escape(path), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 201 || resp.StatusCode == 405 { // 405 = already exists
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("MKCOL %s: HTTP %d: %s", path, resp.StatusCode, b)
}

// Delete removes an entry.
func (c *Client) Delete(path string) error {
	req, err := http.NewRequest("DELETE", c.Endpoint+escape(path), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 || resp.StatusCode == 200 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("DELETE %s: HTTP %d: %s", path, resp.StatusCode, b)
}

// AutoUserNamespace returns the single child folder at root (Tailscale Drive
// always has exactly one for a single-user tailnet).
func (c *Client) AutoUserNamespace() (string, error) {
	entries, err := c.List("/")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir {
			return strings.Trim(e.Href, "/"), nil
		}
	}
	return "", errors.New("no user namespace found at root")
}

// IsSidecarDevice heuristically detects auto-spun container sidecar devices
// (n3m-<service>-ts, n3m-<service>-ts-N) so they don't pollute the device pane.
// These devices never publish shares but the tailnet sees them by name.
func IsSidecarDevice(name string) bool {
	if !strings.HasPrefix(name, "n3m-") {
		return false
	}
	// trailing -ts or -ts-<digit>
	if strings.HasSuffix(name, "-ts") {
		return true
	}
	if i := strings.LastIndex(name, "-ts-"); i >= 0 {
		suffix := name[i+4:]
		if suffix == "" {
			return true
		}
		allDigits := true
		for _, r := range suffix {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		return allDigits
	}
	return false
}

// escape URL-encodes path segments without touching the slashes.
func escape(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// pathEqual compares two WebDAV paths ignoring trailing-slash differences and
// URL escaping.
func pathEqual(a, b string) bool {
	au, _ := url.PathUnescape(a)
	bu, _ := url.PathUnescape(b)
	return strings.TrimRight(au, "/") == strings.TrimRight(bu, "/")
}
