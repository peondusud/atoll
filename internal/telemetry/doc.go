// Package telemetry writes the log of each process, and sends the metrics and the log over OpenTelemetry's protocol
// (specs.md §23.1, §23.2). Logs is the slog handler of a process: one record a line on standard error, in
// OpenTelemetry's log data model as JSON by default, or as Go's slog writes text; Export sends the metrics of /metrics,
// from the same snapshot, and the same records, over OTLP/HTTP with protobuf, with the official OpenTelemetry SDK. The
// export never waits on its collector: what it cannot send in time is dropped, counted, and told of in the log at
// most once every reportEvery.
package telemetry
