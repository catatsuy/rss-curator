package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// 明示的なフラグでのみ使用する。通常のHTTP/TLS設定には混ぜない。
func newHTTPWorkaroundClient(client *http.Client, host string) (*http.Client, error) {
	normal := client.Transport
	if normal == nil {
		normal = http.DefaultTransport
	}
	base, ok := normal.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("HTTP回避設定には標準HTTP Transportが必要です")
	}
	workaround := base.Clone()
	workaround.Protocols = new(http.Protocols)
	workaround.Protocols.SetHTTP1(true)
	workaround.ForceAttemptHTTP2 = false
	workaround.TLSNextProto = nil
	if workaround.TLSClientConfig == nil {
		workaround.TLSClientConfig = &tls.Config{}
	}
	workaround.TLSClientConfig.NextProtos = []string{"http/1.1"}
	config := workaround.TLSClientConfig
	if config.InsecureSkipVerify || config.VerifyConnection != nil || config.GetClientCertificate != nil || len(config.Certificates) != 0 || len(config.CipherSuites) != 0 || len(config.CurvePreferences) != 0 {
		return nil, fmt.Errorf("HTTP回避設定で未対応のカスタムTLS設定が指定されています")
	}
	dial := workaround.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	workaround.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if workaround.TLSHandshakeTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, workaround.TLSHandshakeTimeout)
			defer cancel()
		}
		return dialHTTPWorkaround(ctx, network, addr, dial, config)
	}
	c := *client
	c.Transport = &httpWorkaroundTransport{normal: normal, workaround: workaround, host: strings.ToLower(host)}
	return &c, nil
}

// net/httpのHTTP/1.1とALPNを揃え、TLS接続だけをuTLSへ差し替える。
func dialHTTPWorkaround(ctx context.Context, network, addr string, dial func(context.Context, string, string) (net.Conn, error), config *tls.Config) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if config.ServerName != "" {
		host = config.ServerName
	}
	minVersion, maxVersion := config.MinVersion, config.MaxVersion
	if minVersion == 0 {
		minVersion = tls.VersionTLS12
	}
	if maxVersion == 0 {
		maxVersion = tls.VersionTLS13
	}
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_133)
	if err != nil {
		return nil, err
	}
	spec.TLSVersMin, spec.TLSVersMax = minVersion, maxVersion
	for _, extension := range spec.Extensions {
		switch ext := extension.(type) {
		case *utls.ALPNExtension:
			ext.AlpnProtocols = []string{"http/1.1"}
		case *utls.SupportedVersionsExtension:
			versions := ext.Versions[:0]
			for _, version := range ext.Versions {
				if version == utls.GREASE_PLACEHOLDER || version >= minVersion && version <= maxVersion {
					versions = append(versions, version)
				}
			}
			ext.Versions = versions
		}
	}
	raw, err := dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn := utls.UClient(raw, &utls.Config{ServerName: host, RootCAs: config.RootCAs, MinVersion: minVersion, MaxVersion: maxVersion, VerifyPeerCertificate: config.VerifyPeerCertificate}, utls.HelloCustom)
	if err := conn.ApplyPreset(&spec); err != nil {
		raw.Close()
		return nil, err
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	if protocol := conn.ConnectionState().NegotiatedProtocol; protocol != "" && protocol != "http/1.1" {
		conn.Close()
		return nil, fmt.Errorf("HTTP回避設定で想定外のALPNが選択されました")
	}
	return conn, nil
}

type httpWorkaroundTransport struct {
	host       string
	normal     http.RoundTripper
	workaround *http.Transport
}

func (t *httpWorkaroundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	if host == t.host || strings.HasSuffix(host, "."+t.host) {
		return t.workaround.RoundTrip(req)
	}
	return t.normal.RoundTrip(req)
}

func (t *httpWorkaroundTransport) CloseIdleConnections() {
	t.workaround.CloseIdleConnections()
}
