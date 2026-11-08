package review

import "log/slog"

// logger is the program's logger, marked with this library's name. The program chooses the level
// and format; Codist logs at info unless CODIST_LOG=debug.
func logger() *slog.Logger { return slog.Default().With("lib", "review") }

// warnIf logs a failure that the review carries on past, such as a progress save that did not stick.
func warnIf(err error, msg string, args ...any) {
	if err != nil {
		logger().Warn(msg, append(args, "err", err)...)
	}
}
