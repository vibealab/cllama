package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cllama/server"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func main() {
	listen := flag.String("listen", ":11434", "host:port to listen on")
	maxQueue := flag.Int("maxqueue", 100, "maximum number of queued requests when no backends available")
	name := flag.String("name", "", "comma-separated list of model names exposed by this proxy")
	var parents stringList
	flag.Var(&parents, "parent", "parent cllama server: http://[token@]host:port/<parent-model> (repeatable; ?to=<local-model> to remap)")
	parentAuth := flag.String("parentauth", "", "token child cllama servers must present to connect as tunnel backends")
	apiToken := flag.String("token", "", "bearer token required on LLM API and model-list requests")
	genTimeoutSec := flag.Int("gen-timeout", 0, "seconds before aborting an upstream generation request (0 = no timeout)")
	flag.Parse()

	var models []string
	for _, m := range strings.Split(*name, ",") {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}

	srv, err := server.New(*maxQueue, models, parents, *parentAuth, *apiToken, time.Duration(*genTimeoutSec)*time.Second)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// Note: no global Read/Write timeouts — chat streams and parent/child
	// tunnel SSE connections are long-lived.
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		fmt.Printf("cllama listening on %s\n", *listen)
		if len(models) > 0 {
			fmt.Printf("models: %s\n", strings.Join(models, ", "))
		} else {
			fmt.Println("warning: no models configured (use -name model1,model2)")
		}
		if len(parents) > 0 {
			fmt.Printf("parents: %s\n", strings.Join(parents, ", "))
		}
		if *apiToken != "" {
			fmt.Println("API token required on /api and /v1 endpoints")
		}
		if *genTimeoutSec > 0 {
			fmt.Printf("upstream generation timeout: %ds\n", *genTimeoutSec)
		} else {
			fmt.Println("upstream generation timeout: none (-gen-timeout <sec> to enable)")
		}
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http serve: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	srv.ShutDown()
	fmt.Println("cllama shut down gracefully")
}
