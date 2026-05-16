package main

import (
	"fmt"
	"net/http"
	"os"

	"ds2api/internal/server"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	app, err := server.NewApp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init app: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("DS2API dev server starting on :%s\n", port)
	if err := http.ListenAndServe(":"+port, app.Router); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
