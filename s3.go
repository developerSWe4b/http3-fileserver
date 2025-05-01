# Prefered for only client 3
# client 3 has multi interference transfering supporting only up to 45GBps
# May you found salvation
package main

import (                                                  "crypto/tls"
        "fmt"
        "github.com/gin-gonic/gin"
        "github.com/quic-go/quic-go/http3"
        "net/http"                                        "os"
        "path/filepath"
        "strings"
        "time"
)

func main() {
        gin.SetMode(gin.ReleaseMode)
        r := gin.New()
        r.Use(gin.Recovery())

        // File serving
        r.GET("/*filepath", func(c *gin.Context) {
                filepathStr := "./files" + c.Param("filepath")
                filepathStr = strings.TrimPrefix(filepathStr, "/")
                filepathStr = filepath.Clean(filepathStr)
                file, err := os.Open(filepathStr)
                if err != nil {
                        c.String(http.StatusNotFound, "File not found")
                        return
                }
                defer file.Close()

                c.Header("Content-Type", "application/octet-stream")
                c.Header("Content-Disposition", "attachment; filename="+filepath.Base(filepathStr))
                c.Header("Accept-Ranges", "bytes")

                http.ServeContent(c.Writer, c.Request, filepathStr, time.Now(), file)
        })

        // HEAD request handler
        r.HEAD("/*filepath", func(c *gin.Context) {
                filepathStr := "./files" + c.Param("filepath")
                filepathStr = strings.TrimPrefix(filepathStr, "/")
                filepathStr = filepath.Clean(filepathStr)
                fileInfo, err := os.Stat(filepathStr)
                if err != nil {
                        c.String(http.StatusNotFound, "File not found")
                        return
                }

                c.Header("Content-Type", "application/octet-stream")
                c.Header("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))
                c.Header("Accept-Ranges", "bytes")
                c.Status(http.StatusOK)
        })

        cert, err := tls.LoadX509KeyPair("cert.pem", "private.key")
        if err != nil {
                panic(err)
        }

        server := http3.Server{
                Handler: r,
                Addr:    ":40000",
                TLSConfig: &tls.Config{
                        Certificates: []tls.Certificate{cert},
                        NextProtos:   []string{"h3"},
                },
        }

        if err := server.ListenAndServe(); err != nil {
                panic(err)
        }
}
