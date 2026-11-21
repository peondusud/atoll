package ups

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/peondusud/atoll/internal/value"
)

// readTimeout is how long one reading may take, the connection included: upsmon's poll comes every 5 s.
const readTimeout = 5 * time.Second

// maxLine is the longest line of upsd's answer read: ups.status is a few flags.
const maxLine = 512

// ErrNUT is an ERR line of upsd, such as DATA-STALE, which says that its driver has nothing fresh.
type ErrNUT struct {
	Code string
}

func (e *ErrNUT) Error() string {
	return "upsd answered ERR " + e.Code
}

// Read asks the upsd at n for ups.status, over NUT's own protocol: GET VAR, one line back, then LOGOUT (specs.md §20).
// Reading takes no login. It gives the value, or an error when upsd cannot be reached, answers ERR, or gives a line
// that says nothing, all within 5 s.
func Read(ctx context.Context, n value.NUT) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(n.Host, strconv.Itoa(n.Port)))
	if err != nil {
		return "", fmt.Errorf("upsd: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return "", fmt.Errorf("upsd: %w", err)
		}
	}
	if _, err := conn.Write([]byte("GET VAR " + n.UPS + " ups.status\n")); err != nil {
		return "", fmt.Errorf("upsd: %w", err)
	}
	line, err := bufio.NewReaderSize(conn, maxLine).ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errors.New("upsd: a line longer than 512 bytes")
		}
		return "", fmt.Errorf("upsd: %w", err)
	}
	// Polite, as upsmon is; what it answers, or whether it does, changes nothing.
	_, _ = conn.Write([]byte("LOGOUT\n"))
	status, code := Answer(n.UPS, string(line[:len(line)-1]))
	switch {
	case code != "":
		return "", &ErrNUT{Code: code}
	case status == nil:
		return "", fmt.Errorf("upsd: an answer that says nothing: %q", line)
	}
	return *status, nil
}
