package main

import (
	"context"
	"errors"
	"os"
)

// A Windows host can run `pumasi agent` (runners in Job Objects) but cannot
// join by itself yet: starting the agent at logon is not written.

var errNoService = errors.New("joining from Windows is not supported yet: the agent cannot be installed to start at logon")

func installService(hostPaths, string) (how, note string, err error) { return "", "", errNoService }

func serviceRunning(hostPaths) (bool, string) { return false, "no service on Windows yet" }

func stopService(hostPaths, bool) {}

func pumasiContainers(context.Context, bool) []string { return nil }

func removeContainers(context.Context) (int, error) { return 0, nil }

func removeFiles(p hostPaths) { _ = os.RemoveAll(p.dir) }
