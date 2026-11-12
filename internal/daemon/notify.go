package daemon

import (
	"fmt"
	"net"
	"os"
)

// sdNotify tells the service manager a state, as sd_notify(3) does, when NOTIFY_SOCKET names its socket; it
// does nothing otherwise. A name that starts with @ is in the abstract namespace.
func sdNotify(state string) error {
	name := os.Getenv("NOTIFY_SOCKET")
	if name == "" {
		return nil
	}
	if name[0] == '@' {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	return nil
}
