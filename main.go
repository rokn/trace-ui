package main

import (
	"flag"
	"fmt"
	"os"

	"trace-ui/jaeger"
	"trace-ui/logger"
	"trace-ui/ui"
)

func main() {
	host := flag.String("host", "http://localhost:16686", "Jaeger query URL")
	logFile := flag.String("log", "/tmp/trace-ui.log", "Log file path (empty to disable)")
	flag.Parse()

	if *logFile != "" {
		if err := logger.Init(*logFile); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not open log file: %v\n", err)
		} else {
			defer logger.Close()
			fmt.Fprintf(os.Stderr, "logging to %s\n", *logFile)
		}
	}

	logger.Log("starting trace-ui, host=%s", *host)
	fmt.Fprintf(os.Stderr, "trace-ui connecting to %s\n", *host)

	client := jaeger.NewClient(*host)
	app := ui.NewApp(client)

	go app.LoadInitial()

	if err := app.Run(); err != nil {
		logger.Log("app.Run error: %v", err)
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
