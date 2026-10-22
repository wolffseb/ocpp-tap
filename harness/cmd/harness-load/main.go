// Command harness-load runs N simulated chargers in one process against a
// CPMS or proxy and prints a summary.
//
//	harness-load -url ws://127.0.0.1:9000 -n 100 -messages 30 -interval 1s
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/wolffseb/ocpp-tap/harness/loadgen"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:9000", "base URL; chargers dial URL/ocpp/{id}")
	n := flag.Int("n", 10, "number of chargers")
	messages := flag.Int("messages", 10, "MeterValues per charger")
	interval := flag.Duration("interval", time.Second, "pause between a charger's messages")
	prefix := flag.String("prefix", "LOAD", "charger id prefix")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fleet := loadgen.Start(ctx, loadgen.Options{URL: *url, N: *n, IDPrefix: *prefix, Messages: *messages, Interval: *interval, Log: log})
	res := fleet.Wait()
	fleet.Close()

	fmt.Printf("chargers %d, booted %d, calls %d, errors %d\n", *n, res.Booted, len(res.Latencies), len(res.Errors))
	if l := res.Latencies; len(l) > 0 {
		slices.Sort(l)
		fmt.Printf("round trip p50 %s, p99 %s, max %s\n", l[len(l)/2], l[len(l)*99/100], l[len(l)-1])
	}
	for _, err := range res.Errors {
		fmt.Fprintln(os.Stderr, err)
	}
	if len(res.Errors) > 0 {
		os.Exit(1)
	}
}
