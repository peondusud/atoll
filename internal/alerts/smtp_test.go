package alerts

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// smtpMail is what a fake server took.
type smtpMail struct {
	tls        bool
	auth, from string
	to         []string
	data       string
}

// fakeSMTP is a mail server for one test: TLS from the first byte when implicit, STARTTLS when offered, AUTH
// PLAIN, and a recipient it refuses with 550.
type fakeSMTP struct {
	addr     string
	starttls bool
	refuse   string
	// refusal is what it says of the recipient it refuses, "no such user here" when left out.
	refusal string
	// hangsUp makes it close the connection at QUIT with no answer, as a server that is done with a client does.
	hangsUp bool
	// flood makes it answer EHLO with a line of that many bytes that never ends, then wait for the client to leave.
	flood  int
	config *tls.Config
	mu     sync.Mutex
	got    []smtpMail
}

// selfSigned makes a certificate for 127.0.0.1, and the pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func newFakeSMTP(t *testing.T, implicit, starttls bool) (*fakeSMTP, *tls.Config) {
	t.Helper()
	cert, pool := selfSigned(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeSMTP{addr: ln.Addr().String(), starttls: starttls, config: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, implicit)
		}
	}()
	return f, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func (f *fakeSMTP) serve(conn net.Conn, implicit bool) {
	var m smtpMail
	if implicit {
		conn, m.tls = tls.Server(conn, f.config), true
	}
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	say := func(lines ...string) {
		for _, l := range lines {
			_, _ = conn.Write([]byte(l + "\r\n"))
		}
	}
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO") && f.flood > 0:
			_, _ = conn.Write([]byte("250-" + strings.Repeat("x", f.flood)))
			_, _ = r.ReadString('\n')
			return
		case strings.HasPrefix(upper, "EHLO"):
			lines := []string{"250-fake", "250-AUTH PLAIN"}
			if f.starttls && !m.tls {
				lines = append(lines, "250-STARTTLS")
			}
			say(append(lines, "250 8BITMIME")...)
		case upper == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, f.config)
			if tc.HandshakeContext(context.Background()) != nil {
				return
			}
			conn, r, m.tls = tc, bufio.NewReader(tc), true
		case strings.HasPrefix(upper, "AUTH PLAIN "):
			raw, _ := base64.StdEncoding.DecodeString(line[len("AUTH PLAIN "):])
			m.auth = string(raw)
			say("235 ok")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			m.from = between(line)
			say("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			to := between(line)
			if to == f.refuse {
				text := "no such user here"
				if f.refusal != "" {
					text = f.refusal
				}
				say("550 " + text)
				continue
			}
			m.to = append(m.to, to)
			say("250 ok")
		case upper == "DATA":
			say("354 go on")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(l)
			}
			m.data = data.String()
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			say("250 queued")
		case upper == "QUIT":
			if !f.hangsUp {
				say("221 bye")
			}
			return
		default:
			say("502 not here")
		}
	}
}

// between gives the address between < and > of a command, which may go on with parameters such as BODY=8BITMIME.
func between(line string) string {
	i, j := strings.Index(line, "<"), strings.Index(line, ">")
	if i < 0 || j < i {
		return ""
	}
	return line[i+1 : j]
}

func (f *fakeSMTP) mails() []smtpMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func passwordFile(t *testing.T, text string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

var urgent = &Message{Key: "k", Title: "Atoll: disque t1 absent, été", Body: "line 1\nfilms/été.mkv", Priority: 4}

// Over STARTTLS, with the password its file holds: one mail from from to each to, its subject the title in UTF-8,
// its text quoted-printable, and X-Priority 1 for priority 4 (specs.md §23).
func TestSMTPSendsOverSTARTTLS(t *testing.T) {
	f, trust := newFakeSMTP(t, false, true)
	s := &SMTP{URL: "smtp://atoll@" + f.addr + "?from=atoll@example.com&to=me@example.com&to=you@example.com",
		PasswordFile: passwordFile(t, "s3cret\n"), TLS: trust}
	if err := s.Send(t.Context(), urgent); err != nil {
		t.Fatal(err)
	}
	got := f.mails()
	if len(got) != 1 {
		t.Fatalf("%d mails", len(got))
	}
	m := got[0]
	if !m.tls || m.auth != "\x00atoll\x00s3cret" || m.from != "atoll@example.com" || !slices.Equal(m.to, []string{"me@example.com", "you@example.com"}) {
		t.Errorf("%+v", m)
	}
	for _, want := range []string{
		"From: atoll@example.com\r\n", "To: me@example.com, you@example.com\r\n", "Subject: =?utf-8?q?Atoll:_disque_t1_absent,_=C3=A9t=C3=A9?=\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n", "Content-Transfer-Encoding: quoted-printable\r\n", "X-Priority: 1\r\n",
		"\r\n\r\nline 1\r\nfilms/=C3=A9t=C3=A9.mkv\r\n",
	} {
		if !strings.Contains(m.data, want) {
			t.Errorf("the mail lacks %q:\n%s", want, m.data)
		}
	}
}

// smtps speaks TLS from the first byte, and a priority under 4 sets no X-Priority.
func TestSMTPOverTLS(t *testing.T) {
	f, trust := newFakeSMTP(t, true, false)
	s := &SMTP{URL: "smtps://" + f.addr + "?from=a@example.com&to=b@example.com", TLS: trust}
	if err := s.Send(t.Context(), &Message{Title: "t", Body: "b", Priority: 3}); err != nil {
		t.Fatal(err)
	}
	if got := f.mails(); len(got) != 1 || !got[0].tls || got[0].auth != "" || strings.Contains(got[0].data, "X-Priority") {
		t.Errorf("%+v", got)
	}
}

// A password never goes in clear: with a user and no STARTTLS, nothing is sent. With no user, the mail goes as the
// server takes it.
func TestSMTPKeepsAPasswordOutOfClearText(t *testing.T) {
	f, trust := newFakeSMTP(t, false, false)
	s := &SMTP{URL: "smtp://atoll@" + f.addr + "?from=a@example.com&to=b@example.com", PasswordFile: passwordFile(t, "s3cret"), TLS: trust}
	if err := s.Send(t.Context(), urgent); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("sent with a password in clear: %v", err)
	}
	if got := f.mails(); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	plain := &SMTP{URL: "smtp://" + f.addr + "?from=a@example.com&to=b@example.com", TLS: trust}
	if err := plain.Send(t.Context(), urgent); err != nil {
		t.Fatal(err)
	}
	if got := f.mails(); len(got) != 1 || got[0].tls {
		t.Errorf("%+v", got)
	}
}

// A recipient the server refuses is its answer, and no mail goes.
func TestSMTPRefusal(t *testing.T) {
	f, trust := newFakeSMTP(t, false, true)
	f.refuse = "nobody@example.com"
	s := &SMTP{URL: "smtp://" + f.addr + "?from=a@example.com&to=nobody@example.com", TLS: trust}
	err := s.Send(t.Context(), urgent)
	var answer *AnswerError
	if !errors.As(err, &answer) || answer.Status != "550" {
		t.Errorf("%v", err)
	}
	if got := f.mails(); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

// A mail went when the server took it after DATA (specs.md §23): what QUIT gets then changes nothing. A server that
// hung up there had its mail counted as not sent, and sent again at each try, for as long as the alert stood.
func TestSMTPAMailTakenIsSent(t *testing.T) {
	f, trust := newFakeSMTP(t, false, true)
	f.hangsUp = true
	s := &SMTP{URL: "smtp://" + f.addr + "?from=a@example.com&to=b@example.com", TLS: trust}
	if err := s.Send(t.Context(), urgent); err != nil {
		t.Errorf("a mail the server took: %v", err)
	}
	if got := f.mails(); len(got) != 1 {
		t.Errorf("%d mails", len(got))
	}
}

// A server may send only so much in one conversation: a line that never ends is not held whole for as long as the
// conversation has, as fast as the link carries it. It was, by the daemon, from any server, or from anyone on the way
// before STARTTLS.
func TestSMTPTakesOnlySoMuch(t *testing.T) {
	f, trust := newFakeSMTP(t, false, true)
	f.flood = 4 << 20
	s := &SMTP{URL: "smtp://" + f.addr + "?from=a@example.com&to=b@example.com", TLS: trust}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	began := time.Now()
	err := s.Send(ctx, urgent)
	if !errors.Is(err, errSMTPTooMuch) {
		t.Errorf("%v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("it read on for %s", took)
	}
	if got := f.mails(); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

// What a server says of a refusal is kept short, and whole characters (specs.md §23): it goes to the log.
func TestSMTPRefusalIsKeptShort(t *testing.T) {
	f, trust := newFakeSMTP(t, false, true)
	f.refuse, f.refusal = "nobody@example.com", strings.Repeat("é", 2000)
	s := &SMTP{URL: "smtp://" + f.addr + "?from=a@example.com&to=nobody@example.com", TLS: trust}
	err := s.Send(t.Context(), urgent)
	var answer *AnswerError
	if !errors.As(err, &answer) || answer.Status != "550" {
		t.Fatalf("%v", err)
	}
	if len(answer.Text) > 512 || len(answer.Text) < 500 || !utf8.ValidString(answer.Text) {
		t.Errorf("%d bytes, valid %v", len(answer.Text), utf8.ValidString(answer.Text))
	}
}

// What alerts.smtp may say (specs.md §23).
func TestSMTPAddress(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want *smtpTarget
	}{
		{"smtp://mail.example?from=a@x.example&to=b@x.example", &smtpTarget{host: "mail.example", port: "587", from: "a@x.example", to: []string{"b@x.example"}}},
		{"smtps://u%40x.example@mail.example/?from=a@x.example&to=b@x.example&to=c@x.example",
			&smtpTarget{implicitTLS: true, host: "mail.example", port: "465", user: "u@x.example", from: "a@x.example", to: []string{"b@x.example", "c@x.example"}}},
		{"smtp://mail.example:2525?from=a@x.example&to=b@x.example", &smtpTarget{host: "mail.example", port: "2525", from: "a@x.example", to: []string{"b@x.example"}}},
		{"smtp://u:pw@mail.example?from=a@x.example&to=b@x.example", nil},
		{"smtp://mail.example?to=b@x.example", nil},
		{"smtp://mail.example?from=a@x.example", nil},
		{"smtp://mail.example?from=a@x.example&from=c@x.example&to=b@x.example", nil},
		{"smtp://mail.example?from=a@x.example&to=Me%20%3Cb@x.example%3E", nil},
		{"smtp://mail.example?from=a@x.example&to=b@x.example&cc=c@x.example", nil},
		{"smtp://mail.example/path?from=a@x.example&to=b@x.example", nil},
		{"http://mail.example?from=a@x.example&to=b@x.example", nil},
		{"smtp://?from=a@x.example&to=b@x.example", nil},
	} {
		got, err := parseSMTP(tc.url)
		switch {
		case tc.want == nil && err == nil:
			t.Errorf("%s: %+v, want an error", tc.url, got)
		case tc.want != nil && (err != nil || got.implicitTLS != tc.want.implicitTLS || got.host != tc.want.host || got.port != tc.want.port ||
			got.user != tc.want.user || got.from != tc.want.from || !slices.Equal(got.to, tc.want.to)):
			t.Errorf("%s: %+v, %v, want %+v", tc.url, got, err, *tc.want)
		}
	}
}
