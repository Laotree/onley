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
		hc:        &http.Client{Timeout: 60 * time.Second},
	}
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
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

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
	if err := mw.WriteField("path", localPath); err != nil {
		return err
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, c.masterURL+"/v1/ingest", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ingest returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}
