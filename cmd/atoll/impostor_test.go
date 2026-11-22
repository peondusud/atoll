package main

import (
	"os"
	"time"
)

// impostorEnv makes this test binary, started with it, a process that only sleeps: started with the arguments of the
// daemon that runs for good, it is what any account that may run the executable could start (scrutineer F061). The
// check comes before the tests' own flags are read, as those arguments hold none.
const impostorEnv = "ATOLL_TEST_IMPOSTOR"

func init() {
	if os.Getenv(impostorEnv) != "" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}
