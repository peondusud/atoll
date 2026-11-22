// Package integration holds the integration tests, on disks simulated with
// directories; they need no privileges. They drive a conformance build of
// atoll, which takes folders for disks, as the daemon that runs for good: with
// its watcher, its signals, a stop and a start, and its local API over its
// socket, which the scenarios of conformance/ cannot express, as their daemon
// runs --until-idle with a clock that stands still. Its clock runs here (the
// control file's running). Like the conformance runner, they talk to the
// binary through its command line, its signals, its socket and the filesystem
// alone, and import nothing from internal/. Their files carry the integration
// build tag: run them with make test-integration, which builds the binary and
// names it in ATOLL_BIN; without ATOLL_BIN, they build one of this checkout.
package integration
