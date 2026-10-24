// Command hermes-go is the Go port of the Hermes Agent CLI.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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
	}
	code := Run(ctx, os.Args[1:], env)
	stop()
	os.Exit(code)
}
