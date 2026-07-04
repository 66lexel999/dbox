//go:build !windows

package gui

import "errors"

var errUnsupported = errors.New("native GUI is only available on Windows; run with -gui=false")

func EnableDPIAwareness()                                     {}
func AllowForeground()                                        {}
func RunMain(serverURL, dataDir string, onClose func()) error { return errUnsupported }
func RunDialog(serverURL, query string) error                 { return errUnsupported }
func RunDone(serverURL, id string) error                      { return errUnsupported }
func RunDetail(serverURL, id string) error                    { return errUnsupported }
