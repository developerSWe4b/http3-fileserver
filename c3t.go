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
		"192.168.225.94", // 4G (tethered)
		"192.168.1.7",    // WiFi
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
				InsecureSkipVerify: true,
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

	// Create temporary files for chunks
	tempFiles := make([]*os.File, numChunks)
	chunkProgress := make([]int64, numChunks)
	for i := range tempFiles {
		tempFilePath := fmt.Sprintf("chunk%d.part", i)
		tempFile, err := os.OpenFile(tempFilePath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
		if err != nil {
			log.Panicf("Failed to create temp file for chunk %d: %v", i, err)
		}
		info, err := tempFile.Stat()
		if err != nil {
			log.Panicf("Failed to stat temp file for chunk %d: %v", i, err)
		}
		chunkProgress[i] = info.Size()
		tempFiles[i] = tempFile
	}

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

	// Download chunks concurrently with 30-second cycling
	var wg sync.WaitGroup
	errChan := make(chan error, numChunks)

	for i, r := range ranges {
		wg.Add(1)
		go func(i int, start, end int64) {
			defer wg.Done()
			tempFile := tempFiles[i]
			downloaded := chunkProgress[i]

			for downloaded < end-start+1 {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
				if err != nil {
					log.Printf("Chunk %d: failed to create request: %v", i, err)
					time.Sleep(1 * time.Second)
					continue
				}
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start+downloaded, end))
				log.Printf("Downloading chunk %d: bytes=%d-%d", i, start+downloaded, end)

				rsp, err := clients[i].Do(req)
				if err != nil {
					log.Printf("Chunk %d: request failed: %v", i, err)
					time.Sleep(1 * time.Second)
					continue
				}

				reader := bar.ProxyReader(rsp.Body)
				n, err := io.Copy(tempFile, reader)
				rsp.Body.Close() // Close after copy
				if err != nil {
					log.Printf("Chunk %d: failed to write response: %v", i, err)
					if err := tempFile.Sync(); err != nil {
						log.Printf("Chunk %d: failed to sync temp file: %v", i, err)
					}
					time.Sleep(1 * time.Second)
					continue
				}
				downloaded += n
				chunkProgress[i] = downloaded
				if err := tempFile.Sync(); err != nil {
					log.Panicf("Chunk %d: failed to sync temp file: %v", i, err)
				}
				log.Printf("Chunk %d: downloaded %d/%d bytes", i, downloaded, end-start+1)
			}
			// Close temp file after download
			if err := tempFile.Close(); err != nil {
				errChan <- fmt.Errorf("chunk %d: failed to close temp file: %v", i, err)
			}
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

	// Combine chunks into final file
	file, err := os.Create(filename)
	if err != nil {
		log.Panicf("Failed to create file %s: %v", filename, err)
	}
	defer file.Close()

	for i := range tempFiles {
		tempFile, err := os.Open(fmt.Sprintf("chunk%d.part", i))
		if err != nil {
			log.Panicf("Failed to open temp file for chunk %d: %v", i, err)
		}
		defer tempFile.Close()
		_, err = tempFile.Seek(0, io.SeekStart)
		if err != nil {
			log.Panicf("Failed to seek temp file for chunk %d: %v", i, err)
		}
		_, err = io.Copy(file, tempFile)
		if err != nil {
			log.Panicf("Failed to write chunk %d to final file: %v", i, err)
		}
		// Clean up temp file
		os.Remove(tempFile.Name())
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