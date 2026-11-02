package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	corev1 "k8s.io/api/core/v1"
)

// apiTimeout bounds every call to the BMC API, including token requests
const apiTimeout = 60 * time.Second

// apiClient is an HTTP client shared by a controller's reconciles, so they share one token
type apiClient struct {
	once   sync.Once
	client *http.Client
}

func (a *apiClient) get(cfg clientcredentials.Config) *http.Client {
	a.once.Do(func() {
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: apiTimeout})
		a.client = cfg.Client(ctx)
		a.client.Timeout = apiTimeout
	})
	return a.client
}

// callAPI sends body, if any, as JSON and returns the response status and body
func callAPI(ctx context.Context, c *http.Client, method, url string, body interface{}) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

// listedResource holds the fields of a listed server or IP block used to find it
type listedResource struct {
	ID            string     `json:"id"`
	Hostname      string     `json:"hostname"`
	Location      string     `json:"location"`
	Description   string     `json:"description"`
	ProvisionedOn *time.Time `json:"provisionedOn"`
	CreatedOn     *time.Time `json:"createdOn"`
}

// listResources returns each listed item's raw JSON and its listedResource fields
func listResources(ctx context.Context, c *http.Client, url string) ([]json.RawMessage, []listedResource, error) {
	code, body, err := callAPI(ctx, c, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	if code != http.StatusOK {
		return nil, nil, fmt.Errorf("listing %s: status %d: %s", url, code, body)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}
	listed := make([]listedResource, len(raw))
	for i := range raw {
		if err := json.Unmarshal(raw[i], &listed[i]); err != nil {
			return nil, nil, err
		}
	}
	return raw, listed, nil
}

// confirmGone reports whether id is missing from the list. The API answers 403 both for
// deleted resources and for ones the credentials may not read, so a 403 alone is ambiguous.
func confirmGone(ctx context.Context, c *http.Client, listURL, id string) (bool, error) {
	_, listed, err := listResources(ctx, c, listURL)
	if err != nil {
		return false, err
	}
	for _, r := range listed {
		if r.ID == id {
			return false, nil
		}
	}
	return true, nil
}

// firstIP returns s, or the first address of a "first - last" range, or "" if s is neither
func firstIP(s string) string {
	first, _, _ := strings.Cut(s, " - ")
	first = strings.TrimSpace(first)
	if net.ParseIP(first) == nil {
		return ""
	}
	return first
}

func machineAddresses(public, private []string) []corev1.NodeAddress {
	addrs := []corev1.NodeAddress{}
	for _, a := range public {
		if ip := firstIP(a); ip != "" {
			addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: ip})
		}
	}
	for _, a := range private {
		if ip := firstIP(a); ip != "" {
			addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
		}
	}
	return addrs
}
