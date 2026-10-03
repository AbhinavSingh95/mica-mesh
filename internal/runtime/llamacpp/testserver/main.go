// Testserver is a declared, model-free child used by runtime lifecycle tests.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	client := &http.Client{Timeout: 15 * time.Second}
	control := os.Getenv("MICA_TEST_CONTROL")
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		version := "version: 0.5.0 (build 1, commit 7fe450e)"
		if os.Getenv("MICA_TEST_VERSION") != "" {
			version = os.Getenv("MICA_TEST_VERSION")
		}
		fmt.Fprintln(os.Stderr, version)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--list-devices" {
		fmt.Println("Available devices:\n  MTL0: Apple M4 Pro")
		return
	}
	args := map[string]string{}
	for i := 1; i < len(os.Args); i++ {
		if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "--") {
			args[os.Args[i]] = os.Args[i+1]
			i++
		} else {
			args[os.Args[i]] = "true"
		}
	}
	launch, _ := json.Marshal(struct {
		Args map[string]string
		Env  []string
		PID  int
	}{args, os.Environ(), os.Getpid()})
	resp, err := client.Post(control+"/launch", "application/json", bytes.NewReader(launch))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if os.Getenv("MICA_TEST_NOISE") == "1" {
		for i := 0; i < 256; i++ {
			fmt.Fprint(os.Stderr, strings.Repeat("x", 4096))
		}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(args["--host"], args["--port"]))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/crash", func(w http.ResponseWriter, req *http.Request) { os.Exit(7) })
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 65537))
		response, err := client.Post(control+req.URL.Path, "application/json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	})
	if err := http.Serve(listener, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
