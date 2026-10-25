// Command hermes-go is the Go port of the Hermes Agent CLI.
package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"time"
)

func main() {
	info, ok := debug.ReadBuildInfo()
	version = moduleVersion(version, info, ok)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	interrupt := make(chan struct{}, 1)
	go func() {
		for range sig {
			select {
			case interrupt <- struct{}{}:
			default: // one pending Ctrl-C is enough
			}
		}
	}()
	env := Env{
		GOOS:        runtime.GOOS,
		Getenv:      os.Getenv,
		LookupEnv:   os.LookupEnv,
		UserHomeDir: os.UserHomeDir,
		Getwd:       os.Getwd,
		Now:         time.Now,
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Interrupt:   interrupt,
		LookPath:    exec.LookPath,
	}
	code := Run(context.Background(), os.Args[1:], env)
	signal.Stop(sig)
	os.Exit(code)
}
