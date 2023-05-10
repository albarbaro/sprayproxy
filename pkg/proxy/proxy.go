/*
Copyright © 2023 The Spray Proxy Contributors

SPDX-License-Identifier: Apache-2.0
*/
package proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redhat-appstudio/sprayproxy/pkg/metrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// GitHub webhook request max size is 25MB
const maxReqSize = 1024 * 1024 * 25

type BackendsFunc func() []string

type SprayProxy struct {
	backends    []string
	insecureTLS bool
	logger      *zap.Logger
	fwdReqTmout time.Duration
}

func NewSprayProxy(insecureTLS bool, logger *zap.Logger, backends ...string) (*SprayProxy, error) {

	// forwarding request timeout of 15s, can be overriden by SPRAYPROXY_FORWARDING_REQUEST_TIMEOUT env var
	fwdReqTmout := 15 * time.Second
	if duration, err := time.ParseDuration(os.Getenv("SPRAYPROXY_FORWARDING_REQUEST_TIMEOUT")); err == nil {
		fwdReqTmout = duration
	}
	logger.Info(fmt.Sprintf("proxy forwarding request timeout set to %s", fwdReqTmout.String()))

	return &SprayProxy{
		backends:    backends,
		insecureTLS: insecureTLS,
		logger:      logger,
		fwdReqTmout: fwdReqTmout,
	}, nil
}

func (p *SprayProxy) HandleProxy(c *gin.Context) {
	// currently not distinguishing between requests we can parse and those we cannot parse
	metrics.IncInboundCount()
	errors := []error{}
	zapCommonFields := []zapcore.Field{
		zap.String("method", c.Request.Method),
		zap.String("path", c.Request.URL.Path),
		zap.String("query", c.Request.URL.RawQuery),
		zap.Bool("insecure-tls", p.insecureTLS),
		zap.String("request-id", c.GetString("requestId")),
	}
	// Read in body from incoming request
	buf := &bytes.Buffer{}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxReqSize)
	defer c.Request.Body.Close()
	_, err := buf.ReadFrom(c.Request.Body)
	if err != nil {
		c.String(http.StatusRequestEntityTooLarge, "request body too large")
		p.logger.Error(err.Error(), zapCommonFields...)
		return
	}
	body := buf.Bytes()

	client := &http.Client{
		// set forwarding request timeout
		Timeout: p.fwdReqTmout,
	}
	if p.insecureTLS {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}

	for _, backend := range p.backends {
		backendURL, err := url.Parse(backend)
		if err != nil {
			p.logger.Error("failed to parse backend "+err.Error(), zapCommonFields...)
			continue
		}
		copy := c.Copy()
		newURL := copy.Request.URL
		newURL.Host = backendURL.Host
		newURL.Scheme = backendURL.Scheme
		// zap always append and does not override field entries, so we create
		// per backend list of fields
		zapBackendFields := append(zapCommonFields, zap.String("backend", newURL.Host))
		newRequest, err := http.NewRequest(copy.Request.Method, newURL.String(), bytes.NewReader(body))
		if err != nil {
			p.logger.Error("failed to create request: "+err.Error(), zapBackendFields...)
			errors = append(errors, err)
			continue
		}
		newRequest.Header = copy.Request.Header
		// currently not distinguishing between requests we send and requests that return without error
		metrics.IncForwardedCount(backendURL.Host)

		// for response time, we are making it "simpler" and including everything in the client.Do call
		start := time.Now()
		resp, err := client.Do(newRequest)
		responseTime := time.Now().Sub(start)
		metrics.AddForwardedResponseTime(responseTime.Seconds())
		// standartize on what ginzap logs
		zapBackendFields = append(zapBackendFields, zap.Duration("latency", responseTime))
		if err != nil {
			p.logger.Error("proxy error: "+err.Error(), zapBackendFields...)
			errors = append(errors, err)
			continue
		}
		defer resp.Body.Close()
		zapBackendFields = append(zapBackendFields, zap.Int("status", resp.StatusCode))
		p.logger.Info("proxied request", zapBackendFields...)
		if resp.StatusCode >= 400 {
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				p.logger.Info("failed to read response: "+err.Error(), zapBackendFields...)
			} else {
				p.logger.Info("response body: "+string(respBody), zapBackendFields...)
			}
		}

		// // Create a new request with a disconnected context
		// newRequest := copy.Request.Clone(context.Background())
		// // Deep copy the request body since this needs to be read multiple times
		// newRequest.Body = io.NopCloser(bytes.NewReader(body))

		// proxy := httputil.NewSingleHostReverseProxy(backendURL)
		// proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		// 	errors = append(errors, err)
		// 	rw.WriteHeader(http.StatusBadGateway)
		// }
		// if p.insecureTLS {
		// 	proxy.Transport = &http.Transport{
		// 		TLSClientConfig: &tls.Config{
		// 			InsecureSkipVerify: true,
		// 		},
		// 	}
		// }
		// doProxy(backend, proxy, newRequest)
	}
	if len(errors) > 0 {
		// we have a bad gateway/connection somewhere
		c.String(http.StatusBadGateway, "failed to proxy")
		return
	}
	c.String(http.StatusOK, "proxied")
}

func (p *SprayProxy) Backends() []string {
	return p.backends
}

func (p *SprayProxy) Register(c *gin.Context) {
	server := c.Query("server")
	if server == "" {
		c.String(http.StatusBadRequest, "server parameter is missing")
		return
	}
	for _, v := range p.backends {
		if v == server {
			c.String(http.StatusBadRequest, "already there")
			return
		}
	}
	p.backends = append(p.backends, server)
	c.String(http.StatusOK, strings.Join(p.backends, " "))
}

func (p *SprayProxy) List(c *gin.Context) {
	b := p.Backends()

	c.String(http.StatusOK, strings.Join(b, " "))
}

func (p *SprayProxy) Unregister(c *gin.Context) {
	server := c.Query("server")
	if server == "" {
		c.String(http.StatusBadRequest, "server parameter is missing")
		return
	}
	newBackend := []string{}
	for _, v := range p.backends {
		if v != server {
			newBackend = append(newBackend, v)
		}
	}
	p.backends = newBackend
	c.String(http.StatusOK, strings.Join(p.backends, " "))
}

// InsecureSkipTLSVerify indicates if the proxy is skipping TLS verification.
// This setting is insecure and should not be used in production.
func (p *SprayProxy) InsecureSkipTLSVerify() bool {
	return p.insecureTLS
}

// doProxy proxies the provided request to a backend, with response data to an "empty" response instance.
func doProxy(dest string, proxy *httputil.ReverseProxy, req *http.Request) {
	writer := NewSprayWriter()
	proxy.ServeHTTP(writer, req)
	fmt.Printf("proxied %s to backend %s\n", req.URL, dest)
}
