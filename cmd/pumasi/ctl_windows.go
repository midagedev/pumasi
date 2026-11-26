package main

import "errors"

var errNoCtlService = errors.New("the controller as a service is for Linux (systemd user unit) and macOS (LaunchAgent); on Windows run `pumasi up` from Task Scheduler")

func (s ctlService) can() error                       { return errNoCtlService }
func (s ctlService) start() (string, []string, error) { return "", nil, errNoCtlService }
func (s ctlService) stop() error                      { return nil }
func (s ctlService) logHow() string                   { return s.log }
func (s ctlService) status() error                    { return errNoCtlService }
