package alerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// SMTP sends an alert by mail (specs.md §23).
type SMTP struct {
	URL          string
	PasswordFile string
	// TLS is what a TLS connection trusts: the system's roots when nil. A test gives its own.
	TLS *tls.Config
}

// Name is the key of the channel.
func (s *SMTP) Name() string { return "alerts.smtp" }

// smtpTarget is what alerts.smtp says: smtp://[user@]host[:port]?from=…&to=…, or smtps:// for TLS from the first byte.
type smtpTarget struct {
	implicitTLS bool
	host, port  string
	user        string
	from        string
	to          []string
}

// parseSMTP reads alerts.smtp. A password in it is refused: it goes in alerts.smtp_password_file.
func parseSMTP(raw string) (smtpTarget, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return smtpTarget{}, fmt.Errorf("alerts.smtp: %w", err)
	}
	t := smtpTarget{host: u.Hostname(), port: u.Port()}
	switch u.Scheme {
	case "smtp":
		if t.port == "" {
			t.port = "587"
		}
	case "smtps":
		t.implicitTLS = true
		if t.port == "" {
			t.port = "465"
		}
	default:
		return smtpTarget{}, fmt.Errorf("alerts.smtp: the scheme %q is neither smtp nor smtps", u.Scheme)
	}
	switch {
	case t.host == "":
		return smtpTarget{}, errors.New("alerts.smtp: no host")
	case u.Path != "" && u.Path != "/", u.Fragment != "", u.Opaque != "":
		return smtpTarget{}, errors.New("alerts.smtp: only a host, a port and a user before the query")
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return smtpTarget{}, errors.New("alerts.smtp: the password goes in alerts.smtp_password_file, never in the address")
		}
		t.user = u.User.Username()
	}
	q := u.Query()
	for key := range q {
		if key != "from" && key != "to" {
			return smtpTarget{}, fmt.Errorf("alerts.smtp: %q means nothing: from and to", key)
		}
	}
	if len(q["from"]) != 1 || len(q["to"]) == 0 {
		return smtpTarget{}, errors.New("alerts.smtp: one from and one to at least")
	}
	for _, a := range append([]string{q.Get("from")}, q["to"]...) {
		parsed, err := mail.ParseAddress(a)
		if err != nil || parsed.Name != "" || parsed.Address != a {
			return smtpTarget{}, fmt.Errorf("alerts.smtp: %q is not a bare address", a)
		}
	}
	t.from, t.to = q.Get("from"), q["to"]
	return t, nil
}

// mailOf writes the mail of an alert: its title the subject, its text quoted-printable in UTF-8, and X-Priority 1
// for what ntfy sends at 4. Every line ends with CRLF.
func mailOf(t *smtpTarget, m *Message, now time.Time) ([]byte, error) {
	var body bytes.Buffer
	w := quotedprintable.NewWriter(&body)
	if _, err := w.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n"))); err != nil {
		return nil, fmt.Errorf("smtp: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("smtp: %w", err)
	}
	var out bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&out, "%s: %s\r\n", k, v) }
	header("From", t.from)
	header("To", strings.Join(t.to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", m.Title))
	header("Date", now.Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	if m.Priority >= 4 {
		header("X-Priority", "1")
	}
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	out.WriteString("\r\n")
	return out.Bytes(), nil
}

// Send mails the alert from from to each to. smtps speaks TLS from the first byte; smtp takes STARTTLS when the
// server offers it, and must when a user is given, as a password never goes in clear. The password is read at each
// send. A refusal by the server is its answer.
func (s *SMTP) Send(ctx context.Context, m *Message) error {
	t, err := parseSMTP(s.URL)
	if err != nil {
		return err
	}
	var password string
	if t.user != "" {
		if s.PasswordFile == "" {
			return errors.New("smtp: a user, and alerts.smtp_password_file is not set")
		}
		if password, err = readToken(s.PasswordFile); err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	data, err := mailOf(&t, m, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(t.host, t.port))
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	if t.implicitTLS {
		conn = tls.Client(conn, s.tlsFor(t.host))
	}
	c, err := smtp.NewClient(conn, t.host)
	if err != nil {
		_ = conn.Close()
		return answer(err)
	}
	defer func() { _ = c.Close() }()
	if !t.implicitTLS {
		offered, _ := c.Extension("STARTTLS")
		switch {
		case offered:
			if err := c.StartTLS(s.tlsFor(t.host)); err != nil {
				return answer(err)
			}
		case t.user != "":
			return errors.New("smtp: the server offers no STARTTLS, and a password never goes in clear")
		}
	}
	if t.user != "" {
		if err := c.Auth(smtp.PlainAuth("", t.user, password, t.host)); err != nil {
			return answer(err)
		}
	}
	if err := c.Mail(t.from); err != nil {
		return answer(err)
	}
	for _, to := range t.to {
		if err := c.Rcpt(to); err != nil {
			return answer(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return answer(err)
	}
	if _, err := w.Write(data); err != nil {
		return answer(err)
	}
	if err := w.Close(); err != nil {
		return answer(err)
	}
	return answer(c.Quit())
}

// tlsFor gives the TLS configuration for a host: what s.TLS trusts, for that name.
func (s *SMTP) tlsFor(host string) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.TLS != nil {
		c = s.TLS.Clone()
	}
	c.ServerName = host
	return c
}

// answer gives what an SMTP server refused as its answer, and wraps anything else.
func answer(err error) error {
	if err == nil {
		return nil
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		return fmt.Errorf("smtp: %w", &AnswerError{Status: fmt.Sprint(te.Code), Text: te.Msg})
	}
	return fmt.Errorf("smtp: %w", err)
}
