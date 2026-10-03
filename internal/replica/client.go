package replica

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// responseHeaderTimeout bounds how long a request waits for the server to start
// responding.
const responseHeaderTimeout = 60 * time.Second

// Action describes what should happen to a file on the replica.
type Action string

const (
	// ActionDeleteLocal means the master already has this file; delete it locally.
	ActionDeleteLocal Action = "delete_local"
	// ActionMigrate means the master lacks this file; transfer it to master.
	ActionMigrate Action = "migrate"
)

// PlanEntry is one item in the replica reconciliation plan.
type PlanEntry struct {
	Path   string `json:"path"`
	MD5    string `json:"md5"`
	Size   int64  `json:"size"`
	Action Action `json:"action"`
}

// Client queries and uploads to a master onley server over HTTP.
type Client struct {
	masterURL string
	hc        *http.Client
}

// NewClient creates a replica client pointing at masterURL
// (e.g. "http://master-host:8080").
func NewClient(masterURL string) *Client {
	return &Client{
		masterURL: masterURL,
		hc:        &http.Client{Transport: newTransport()},
	}
}

// newTransport bounds how long a request waits for the server to begin
// responding, and nothing beyond that.
//
// The client used to carry a 60 second overall timeout. That reads like a guard
// against an unresponsive master, and it is not one: an overall timeout also
// bounds how long the request body may take to send, which depends on the size
// of the file and the speed of the link. Neither says anything about whether the
// master is healthy, so the one thing the timeout was for, it did not protect,
// and the side effect was that a large upload over a slow link failed
// regardless of how healthy the master was.
//
// Uploads stream now, so how long they take is not a signal about anything, and
// the timeout is spent where it belongs.
//
// The default transport is cloned rather than written from scratch because it
// carries the dial, TLS handshake and idle connection timeouts that this only
// needs to keep.
func newTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Only reachable if something replaced the package default, in which
		// case there is nothing worth cloning and this is the honest minimum.
		return &http.Transport{ResponseHeaderTimeout: responseHeaderTimeout}
	}
	t := base.Clone()
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}

// Ping returns nil if the master is reachable and healthy.
func (c *Client) Ping() error {
	resp, err := c.hc.Get(c.masterURL + "/v1/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// Check returns true if the master already holds a file with the given MD5.
func (c *Client) Check(md5sum string) (bool, error) {
	resp, err := c.hc.Get(fmt.Sprintf("%s/v1/check?md5=%s", c.masterURL, md5sum))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("check returned %d", resp.StatusCode)
	}
	var result checkResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.Found, nil
}

// Ingest uploads the file at localPath to the master.
// md5sum is the precomputed MD5 hex string for the file.
func (c *Client) Ingest(localPath, md5sum string) error {
	req, bodyErr, err := c.ingestRequest(localPath, md5sum)
	if err != nil {
		return err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// The transport error already carries whatever went wrong while reading
		// the file, because closing the pipe with an error surfaces here. Waiting
		// on bodyErr as well would only risk blocking on a goroutine that a failed
		// request has already stopped reading from.
		return err
	}
	defer resp.Body.Close()

	if err := <-bodyErr; err != nil {
		return fmt.Errorf("upload %s: %w", localPath, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ingest returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// ingestRequest builds the upload request with a body that streams the file.
//
// Building the multipart body into a buffer held the whole file in memory, so a
// 2 GB file needed 2 GB of RAM to send. The body is a pipe instead: memory use
// is the pipe's buffer and nothing else, whatever the file size.
//
// The returned channel carries whatever the body goroutine hit, if anything.
// It is buffered so the goroutine never blocks on a caller that has gone away.
func (c *Client) ingestRequest(localPath, md5sum string) (*http.Request, <-chan error, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, nil, err
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	errc := make(chan error, 1)

	go func() {
		defer f.Close()
		err := func() error {
			part, err := mw.CreateFormFile("file", filepath.Base(localPath))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			if err := mw.WriteField("md5", md5sum); err != nil {
				return err
			}
			return mw.WriteField("path", localPath)
		}()
		if err == nil {
			// mw.Close writes the trailing boundary. Without it the body has a
			// beginning and no end, which is a request the transport will not
			// finish sending.
			err = mw.Close()
		}
		// Closing with the error is what tells the transport the body is over and
		// why. A plain Close would present a short body as a complete one, and the
		// upload would land as a truncated file with no indication of it.
		//
		// The error goes to errc first, because CloseWithError returns nil whatever
		// it is given and reporting that would swallow the failure entirely.
		errc <- err
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequest(http.MethodPost, c.masterURL+"/v1/ingest", pr)
	if err != nil {
		f.Close()
		pr.CloseWithError(err)
		return nil, nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// ContentLength is deliberately left unset. Computing it would mean splitting
	// the multipart framing into a header and a footer and adding the file size,
	// which the multipart writer offers no clean way to do; without it net/http
	// uses chunked transfer encoding, which the master reads.
	return req, errc, nil
}
