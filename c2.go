package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

func main() {
	currentPath, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	roundTripper := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{
			RootCAs: getRootCA(currentPath),
		},
	}
	defer roundTripper.Close()

	client := &http.Client{
		Transport: roundTripper,
	}

	addr := "https://localhost:8080"

	fmt.Print("Enter the filename to download: ")
	var filename string
	fmt.Scanln(&filename)

	url := addr + "/" + filename

	rsp, err := client.Get(url)
	if err != nil {
		panic(err)
	}
	defer rsp.Body.Close()

	// Create a new file to save the downloaded content
	file, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	// Set up the progress bar
	progress := mpb.New(
		mpb.WithRefreshRate(180 * time.Millisecond),
		mpb.WithWidth(64),
	)
	defer progress.Wait()

	total := int64(rsp.ContentLength)
	bar := progress.AddBar(
		total,
		mpb.PrependDecorators(
			decor.CountersNoUnit(" % .2f / % .2f "),
			decor.Percentage(),
		),
		mpb.AppendDecorators(
			decor.AverageSpeed(decor.UnitKB, " % .2f"),
			decor.AverageETA(decor.ET_STYLE_GO),
		),
	)

	// Proxy the response body to the file and update the progress bar
	reader := bar.ProxyReader(rsp.Body)
	_, err = io.Copy(file, reader)
	if err != nil {
		panic(err)
	}

	log.Printf("File %s downloaded successfully", filename)
}

func getRootCA(certPath string) *x509.CertPool {
	caCertPath := path.Join(certPath, "ca.pem")
	caCertRaw, err := os.ReadFile(caCertPath)
	if err != nil {
		panic(err)
	}

	p, _ := pem.Decode(caCertRaw)
	if p.Type != "CERTIFICATE" {
		panic("expected a certificate")
	}

	caCert, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		panic(err)
	}

	certPool := x509.NewCertPool()
	certPool.AddCert(caCert)
	return certPool
}
