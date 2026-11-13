package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Gotify sends an alert to a Gotify server, as an application whose token a file holds (specs.md §23).
type Gotify struct {
	URL       string
	TokenFile string
	Client    *http.Client // http.DefaultClient when nil
}

// Name is the key of the channel.
func (g *Gotify) Name() string { return "alerts.gotify" }

// gotifyMessage is what Gotify takes at /message.
type gotifyMessage struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority int    `json:"priority"`
}

// gotifyPriority gives Gotify's priority for an alert's: 8 where ntfy has 4, which Gotify's clients sound, and 5
// otherwise (specs.md §23).
func gotifyPriority(p int) int {
	if p >= 4 {
		return 8
	}
	return 5
}

// Send posts the alert to <alerts.gotify>/message, with the application token the token file holds, read at each
// send so that a new token needs no reload.
func (g *Gotify) Send(ctx context.Context, m *Message) error {
	if g.TokenFile == "" {
		return errors.New("gotify: alerts.gotify_token_file is not set, and Gotify takes no message without an application token")
	}
	token, err := readToken(g.TokenFile)
	if err != nil {
		return fmt.Errorf("gotify: %w", err)
	}
	body, err := json.Marshal(gotifyMessage{Title: m.Title, Message: m.Body, Priority: gotifyPriority(m.Priority)})
	if err != nil {
		return fmt.Errorf("gotify: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(g.URL, "/")+"/message", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gotify: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", token)
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("gotify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("gotify: %w", &AnswerError{Status: resp.Status, Text: string(bytes.TrimSpace(text))})
	}
	return nil
}
