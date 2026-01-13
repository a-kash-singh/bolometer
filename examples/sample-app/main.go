package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"
)

func main() {
	pprofPort := os.Getenv("PPROF_PORT")
	if pprofPort == "" {
		pprofPort = "6060"
	}

	// Start pprof server
	go func() {
		log.Printf("Starting pprof server on :%s", pprofPort)
		if err := http.ListenAndServe(":"+pprofPort, nil); err != nil {
			log.Printf("pprof server error: %v", err)
		}
	}()

	// Start main application server
	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/load", handleLoad)

	log.Println("Starting application server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Hello from profiling demo app!\n")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK\n")
}

// handleLoad simulates CPU and memory load
func handleLoad(w http.ResponseWriter, r *http.Request) {
	// Simulate some work
	data := make([]byte, 10*1024*1024) // 10MB allocation
	for i := 0; i < len(data); i++ {
		data[i] = byte(i % 256)
	}

	// CPU intensive work
	sum := 0
	for i := 0; i < 10000000; i++ {
		sum += i
	}

	time.Sleep(100 * time.Millisecond)

	fmt.Fprintf(w, "Load generated: sum=%d, data_size=%d\n", sum, len(data))
}
