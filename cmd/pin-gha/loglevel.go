package main

import (
	"log/slog"
)

// LevelIds maps zap log levels to their corresponding string identifiers.
var LevelIds = map[slog.Level][]string{
	slog.LevelDebug: {"debug"},
	slog.LevelInfo:  {"info"},
	slog.LevelWarn:  {"warn"},
	slog.LevelError: {"error"},
}

var loglevel slog.Level = slog.LevelInfo
