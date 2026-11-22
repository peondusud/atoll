package ups

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/vectors"
)

func TestPowerVectors(t *testing.T) {
	type vector struct {
		Power       Power   `json:"power"`
		AnsweredNS  int64   `json:"answered_ns"`
		NowNS       int64   `json:"now_ns"`
		DeadNS      int64   `json:"dead_ns"`
		Status      *string `json:"status"`
		Next        Power   `json:"next"`
		Answered    bool    `json:"answered"`
		Unreachable bool    `json:"unreachable"`
		Note        string  `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "ups_power.json") {
		got := Next(v.Power, v.AnsweredNS, v.NowNS, v.DeadNS, v.Status)
		if want := (Reading{Power: v.Next, Answered: v.Answered, Unreachable: v.Unreachable}); got != want {
			t.Errorf("%s: %+v, want %+v", v.Note, got, want)
		}
	}
}

func TestAnswerVectors(t *testing.T) {
	type vector struct {
		UPS    string  `json:"ups"`
		Line   string  `json:"line"`
		Status *string `json:"status"`
		Err    string  `json:"err"`
		Note   string  `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "ups_answer.json") {
		status, code := Answer(v.UPS, v.Line)
		if (status == nil) != (v.Status == nil) || (status != nil && *status != *v.Status) || code != v.Err {
			t.Errorf("%s: %v %q, want %v %q", v.Note, status, code, v.Status, v.Err)
		}
	}
}

// upsd answers each GET VAR with what answer gives for the line, and counts the LOGOUTs.
func upsd(t *testing.T, answer func(string) string) (value.NUT, <-chan string) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	said := make(chan string, 16)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					said <- line
					if strings.HasPrefix(line, "GET VAR ") {
						if _, err := conn.Write([]byte(answer(line))); err != nil {
							return
						}
					}
				}
			}()
		}
	}()
	addr := l.Addr().(*net.TCPAddr)
	return value.NUT{UPS: "ups", Host: "127.0.0.1", Port: addr.Port}, said
}

// Read asks for ups.status of the UPS named, gives the value, and logs out.
func TestReadAsksUpsd(t *testing.T) {
	n, said := upsd(t, func(string) string { return "VAR ups ups.status \"OB DISCHRG\"\n" })
	got, err := Read(t.Context(), n)
	if err != nil || got != "OB DISCHRG" {
		t.Fatalf("%q, %v", got, err)
	}
	for _, want := range []string{"GET VAR ups ups.status\n", "LOGOUT\n"} {
		select {
		case line := <-said:
			if line != want {
				t.Errorf("upsd heard %q, want %q", line, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("upsd never heard %q", want)
		}
	}
}

// An ERR line, a line that says nothing, a line too long, no upsd: each an error.
func TestReadFails(t *testing.T) {
	n, _ := upsd(t, func(string) string { return "ERR DATA-STALE\n" })
	_, err := Read(t.Context(), n)
	var e *ErrNUT
	if !errors.As(err, &e) || e.Code != "DATA-STALE" {
		t.Errorf("an ERR: %v", err)
	}
	for _, answer := range []string{"VAR other ups.status \"OL\"\n", strings.Repeat("x", 600) + "\n"} {
		n, _ := upsd(t, func(string) string { return answer })
		if got, err := Read(t.Context(), n); err == nil {
			t.Errorf("%.20q: %q", answer, got)
		}
	}
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(t.Context(), value.NUT{UPS: "ups", Host: "127.0.0.1", Port: port}); err == nil {
		t.Errorf("no upsd: %q", got)
	}
}

// An upsd that never answers holds a reading 5 s at most.
func TestReadGivesUp(t *testing.T) {
	n, _ := upsd(t, func(string) string {
		time.Sleep(time.Minute)
		return ""
	})
	start := time.Now()
	if _, err := Read(t.Context(), n); err == nil {
		t.Error("an answer from an upsd that gave none")
	}
	if took := time.Since(start); took > readTimeout+2*time.Second {
		t.Errorf("it took %s", took)
	}
}
