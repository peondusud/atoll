package alerts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

// requestTimeout is how long a request to a channel has (specs.md §23).
const requestTimeout = 10 * time.Second

// AnswerError is a server that answered, with a refusal: the channel works, and another alert may go.
type AnswerError struct {
	Status string
	Text   string
}

func (e *AnswerError) Error() string { return e.Status + ": " + e.Text }

// direct is the client of a request that carries a token: it follows no redirect, as the token, and the alert with
// it, would go to whatever host the answer names, and a POST redirected with 301 or 302 becomes a GET that sends
// nothing. The address in the configuration is the server's own.
var direct = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// notFollowed is what an answer that redirects says in place of its text, which names where it leads: on a public
// ntfy server that address holds the topic, which is the secret.
const notFollowed = "a redirect, which is not followed: write the address it leads to in the configuration"

// answerText gives what an answer that is not 2xx says, from at most 512 bytes of it.
func answerText(resp *http.Response) string {
	if resp.StatusCode/100 == 3 {
		return notFollowed
	}
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return string(bytes.TrimSpace(text))
}

// Channel sends alerts somewhere.
type Channel interface {
	// Name is the configuration key that sets the channel.
	Name() string
	Send(ctx context.Context, m *Message) error
}

// Channels gives the channels the configuration sets, in the order of specs.md §23.
func Channels(c *config.Config) []Channel {
	effective := c.Effective()
	var out []Channel
	if url, _ := effective["alerts.ntfy"].(string); url != "" {
		token, _ := effective["alerts.ntfy_token_file"].(string)
		out = append(out, &Ntfy{URL: url, TokenFile: token})
	}
	if url, _ := effective["alerts.gotify"].(string); url != "" {
		token, _ := effective["alerts.gotify_token_file"].(string)
		out = append(out, &Gotify{URL: url, TokenFile: token})
	}
	if url, _ := effective["alerts.smtp"].(string); url != "" {
		password, _ := effective["alerts.smtp_password_file"].(string)
		out = append(out, &SMTP{URL: url, PasswordFile: password})
	}
	return out
}

// readToken reads the token a file holds, on one line, at each send, so that a new token needs no reload.
func readToken(file string) (string, error) {
	data, err := fsx.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("the token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("the token file %s holds no token on one line", file)
	}
	return token, nil
}

// Ntfy sends an alert to a ntfy topic, with an HTTP POST of its text (specs.md §23).
type Ntfy struct {
	URL       string
	TokenFile string
	Client    *http.Client // one that follows no redirect when nil
}

// Name is the key of the channel.
func (n *Ntfy) Name() string { return "alerts.ntfy" }

// Send posts the alert, with the token the token file holds, read at each send so that a new token needs no reload.
func (n *Ntfy) Send(ctx context.Context, m *Message) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(m.Body))
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	req.Header.Set("Title", m.Title)
	req.Header.Set("Priority", strconv.Itoa(m.Priority))
	req.Header.Set("Tags", "warning")
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if n.TokenFile != "" {
		token, err := readToken(n.TokenFile)
		if err != nil {
			return fmt.Errorf("ntfy: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := n.Client
	if client == nil {
		client = direct
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL stays out of the message: on a public server, the topic is the secret.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("ntfy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy: %w", &AnswerError{Status: resp.Status, Text: answerText(resp)})
	}
	return nil
}
