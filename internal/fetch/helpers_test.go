package fetch

import (
	"io"
	"log/slog"
	"net"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func parseIP(s string) net.IP { return net.ParseIP(s) }
