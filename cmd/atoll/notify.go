package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peondusud/atoll/internal/alerts"
)

// notifyChannel is one channel of api/schemas/notify-test.schema.json.
type notifyChannel struct {
	Channel string `json:"channel"`
	Sent    bool   `json:"sent"`
	Error   string `json:"error,omitempty"`
}

// notifyOutput is the output of api/schemas/notify-test.schema.json.
type notifyOutput struct {
	Channels []notifyChannel `json:"channels"`
}

// runNotify runs atoll notify test: a test alert through each channel set (specs.md §23). It needs no catalog.
func runNotify(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, T("notify: want test"))
	}
	if args[0] != "test" {
		return usage(stderr, fmt.Sprintf(T("notify: unknown command %q"), args[0]))
	}
	asJSON := false
	for _, a := range args[1:] {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("notify test: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "notify test: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	channels := alerts.Channels(c)
	if len(channels) == 0 {
		return failure(stderr, errors.New(T("notify test: no channel is set: alerts.ntfy is empty")))
	}
	m := &alerts.Message{
		Key: "test", Title: "Atoll: a test alert", Priority: 3,
		Body: fmt.Sprintf(T("atoll notify test, from the pool %s: its alerts reach you here."), c.PoolID),
	}
	out := notifyOutput{Channels: make([]notifyChannel, 0, len(channels))}
	code := exitOK
	var text strings.Builder
	for _, ch := range channels {
		line := notifyChannel{Channel: ch.Name(), Sent: true}
		if err := ch.Send(context.Background(), m); err != nil {
			line.Sent, line.Error = false, err.Error()
			code = exitPartial
			fmt.Fprintf(&text, T("%s: not sent: %s\n"), line.Channel, line.Error)
		} else {
			fmt.Fprintf(&text, T("%s: sent\n"), line.Channel)
		}
		out.Channels = append(out.Channels, line)
	}
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, text.String())
	}
	if err != nil {
		return failure(stderr, err)
	}
	return code
}
