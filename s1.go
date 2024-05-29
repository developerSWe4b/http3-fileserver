package main

import (
	"github.com/gin-gonic/gin"
	"github.com/quic-go/quic-go/http3"
	"log"
	"net/http"
	"os"
	"path"
	
)

func main() {
	currentPath, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	router := gin.New()

	// Serve static files from the current directory
	router.StaticFS("/", http.Dir(currentPath))

	server := http3.Server{
		Addr:    "127.0.0.1:8080",
		Handler: router,
	}

	err = server.ListenAndServeTLS(path.Join(currentPath, "cert.pem"), path.Join(currentPath, "private.key"))
	if err != nil {
		log.Printf("Server error: %v", err)
	}
}
