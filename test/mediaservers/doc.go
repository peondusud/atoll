// Package mediaservers tries Atoll's notifications to media servers (specs.md §21.5) on real Plex, Jellyfin
// and Emby servers, each run in a container of its vendor's image. Its files carry the mediaservers build tag:
// run them with test/mediaservers/run.sh, which needs only Docker.
//
// Plex is left unclaimed, in no account, and gets no token: the test runs in its network namespace and reaches it at
// 127.0.0.1, as an unclaimed Plex takes no other host's requests. With PLEX_CLAIM set, a claim token of an account,
// run.sh has the server claimed instead, and the test sends its token.
//
// testdata/sample.mkv is two seconds of FFmpeg's test pattern, made with
//
//	ffmpeg -f lavfi -i testsrc=duration=2:size=160x120:rate=5 -c:v libx264 -pix_fmt yuv420p sample.mkv
package mediaservers
