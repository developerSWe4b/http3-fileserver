package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

func main() {
	currentPath, err := os.Getwd()
	if err != nil {
		log.Panicf("Failed to get current path: %v", err)
	}

	// Define local IPs for 4G and WiFi interfaces
	localIPs := []string{
		"100.64.221.73", // 4G (rmnet_data3)
		"192.168.1.6",   // WiFi (wlan0)
	}

	// Set up UDP connections for each interface
	conns := make([]*net.UDPConn, len(localIPs))
	for i, ip := range localIPs {
		addr, err := net.ResolveUDPAddr("udp", ip+":0")
		if err != nil {
			log.Panicf("Failed to resolve UDP addr for %s: %v", ip, err)
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			log.Panicf("Failed to listen on %s: %v", ip, err)
		}
		conns[i] = conn
		log.Printf("Bound to %s", addr.String())
	}

	// Create HTTP/3 clients with custom QUIC transports
	clients := make([]*http.Client, len(conns))
	for i, conn := range conns {
		transport := &quic.Transport{Conn: conn}
		roundTripper := &http3.RoundTripper{
			TLSClientConfig: &tls.Config{
				RootCAs:            getRootCA(currentPath),
				InsecureSkipVerify: true, // Match curl -k
			},
			QUICConfig: &quic.Config{
				HandshakeIdleTimeout: 60 * time.Second,
				MaxIdleTimeout:       120 * time.Second,
			},
			Dial: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlyConnection, error) {
				udpAddr, err := net.ResolveUDPAddr("udp", addr)
				if err != nil {
					return nil, fmt.Errorf("failed to resolve UDP addr %s: %v", addr, err)
				}
				conn, err := transport.DialEarly(ctx, udpAddr, tlsCfg, cfg)
				if err != nil {
					return nil, fmt.Errorf("failed to dial %s: %v", addr, err)
				}
				return conn, nil
			},
		}
		clients[i] = &http.Client{
			Transport: roundTripper,
			Timeout:   60 * time.Second,
		}
		defer roundTripper.Close()
	}

	// Prompt for server address and filename
	fmt.Print("Server address? ")
	var addr string
	fmt.Scanln(&addr)

	fmt.Print("Enter the filename to download: ")
	var filename string
	fmt.Scanln(&filename)

	url := addr + "/" + filename
	log.Printf("Attempting to fetch %s", url)

	// Test connectivity with each client
	for i, client := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
		if err != nil {
			log.Printf("Client %d: Failed to create HEAD request: %v", i, err)
			continue
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Client %d: HEAD request failed: %v", i, err)
			continue
		}
		resp.Body.Close()
		log.Printf("Client %d: HEAD succeeded in %v, Content-Length: %d", i, time.Since(start), resp.ContentLength)
	}

	// Get file size with HEAD request
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		log.Panicf("Failed to create HEAD request: %v", err)
	}
	headRsp, err := clients[0].Do(req)
	if err != nil {
		log.Panicf("HEAD request failed: %v", err)
	}
	defer headRsp.Body.Close()
	totalSize := headRsp.ContentLength
	if totalSize < 0 {
		log.Panicf("Server did not provide Content-Length")
	}
	log.Printf("File size: %d bytes", totalSize)

	// Split file into chunks
	numChunks := len(clients)
	chunkSize := totalSize / int64(numChunks)
	ranges := make([][2]int64, numChunks)
	for i := range ranges {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == numChunks-1 {
			end = totalSize - 1
		}
		ranges[i] = [2]int64{start, end}
		log.Printf("Chunk %d: bytes=%d-%d", i, start, end)
	}

	// Create output file
	file, err := os.Create(filename)
	if err != nil {
		log.Panicf("Failed to create file %s: %v", filename, err)
	}
	defer file.Close()

	// Set up progress bar
	progress := mpb.New(
		mpb.WithRefreshRate(180 * time.Millisecond),
		mpb.WithWidth(64),
	)
	defer progress.Wait()

	bar := progress.AddBar(
		totalSize,
		mpb.PrependDecorators(
			decor.CountersKibiByte(" % .2f / % .2f "),
			decor.Percentage(),
		),
		mpb.AppendDecorators(
			decor.AverageSpeed(decor.UnitKiB, " % .2f"),
			decor.AverageETA(decor.ET_STYLE_GO),
		),
	)

	// Download chunks concurrently
	var wg sync.WaitGroup
	chunks := make([][]byte, numChunks)
	errChan := make(chan error, numChunks)

	for i, r := range ranges {
		wg.Add(1)
		go func(i int, start, end int64) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				errChan <- fmt.Errorf("chunk %d: failed to create request: %v", i, err)
				return
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
			log.Printf("Downloading chunk %d: bytes=%d-%d", i, start, end)

			rsp, err := clients[i].Do(req)
			if err != nil {
				errChan <- fmt.Errorf("chunk %d: request failed: %v", i, err)
				return
			}
			defer rsp.Body.Close()

			reader := bar.ProxyReader(rsp.Body)
			body, err := io.ReadAll(reader)
			if err != nil {
				errChan <- fmt.Errorf("chunk %d: failed to read response: %v", i, err)
				return
			}
			chunks[i] = body
			log.Printf("Chunk %d downloaded: %d bytes", i, len(body))
		}(i, r[0], r[1])
	}

	// Wait for downloads
	wg.Wait()
	close(errChan)

	// Check errors
	for err := range errChan {
		if err != nil {
			log.Panicf("Download error: %v", err)
		}
	}

	// Write chunks to file
	for i, chunk := range chunks {
		_, err := file.Write(chunk)
		if err != nil {
			log.Panicf("Failed to write chunk %d: %v", i, err)
		}
	}

	log.Printf("File %s downloaded successfully", filename)
}

func getRootCA(certPath string) *x509.CertPool {
	caCertPath := path.Join(certPath, "ca.pem")
	caCertRaw, err := os.ReadFile(caCertPath)
	if err != nil {
		log.Panicf("Failed to read ca.pem: %v", err)
	}

	p, _ := pem.Decode(caCertRaw)
	if p == nil || p.Type != "CERTIFICATE" {
		log.Panicf("Failed to decode certificate: expected a certificate")
	}

	caCert, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		log.Panicf("Failed to parse certificate: %v", err)
	}

	certPool := x509.NewCertPool()
	certPool.AddCert(caCert)
	return certPool
}
