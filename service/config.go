/*
 * @Author: Vincent Yang
 * @Date: 2024-04-23 00:39:03
 * @LastEditors: Jason Lyu
 * @LastEditTime: 2025-04-08 13:45:00
 * @FilePath: /DLX/service/config.go
 * @Telegram: https://t.me/missuo
 * @GitHub: https://github.com/missuo
 *
 * Copyright © 2024 by Vincent, All Rights Reserved.
 */

package service

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	IP        string
	Port      int
	Token     string
	DlSession string
	Proxy     string
}

func InitConfig() *Config {
	cfg := &Config{
		IP:   "0.0.0.0",
		Port: 1188,
	}

	// IP flag
	if ip, ok := os.LookupEnv("IP"); ok && ip != "" {
		cfg.IP = ip
	}
	flag.StringVar(&cfg.IP, "ip", cfg.IP, "set up the IP address to bind to")
	flag.StringVar(&cfg.IP, "i", cfg.IP, "set up the IP address to bind to")

	// Port flag
	if port, ok := os.LookupEnv("PORT"); ok && port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}
	flag.IntVar(&cfg.Port, "port", cfg.Port, "set up the port to listen on")
	flag.IntVar(&cfg.Port, "p", cfg.Port, "set up the port to listen on")

	// DL Session flag
	flag.StringVar(&cfg.DlSession, "s", "", "set the dl-session for /v1/translate endpoint")
	if cfg.DlSession == "" {
		if dlSession, ok := os.LookupEnv("DL_SESSION"); ok {
			cfg.DlSession = dlSession
		}
	}

	// Access token flag
	flag.StringVar(&cfg.Token, "token", "", "set the access token for /translate endpoint")
	if cfg.Token == "" {
		if token, ok := os.LookupEnv("TOKEN"); ok {
			cfg.Token = token
		}
	}

	// HTTP Proxy flag
	flag.StringVar(&cfg.Proxy, "proxy", "", "set the proxy URL for HTTP requests")
	if cfg.Proxy == "" {
		if proxy, ok := os.LookupEnv("PROXY"); ok {
			cfg.Proxy = proxy
		}
	}

	flag.Parse()

	// Fail fast on a configuration that cannot work, instead of binding a
	// nonsensical port or discovering a malformed proxy on the first request.
	if err := cfg.validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}
	return cfg
}

// validate checks the values that flag/env parsing cannot express: a port in
// the TCP range and a proxy URL that net/http and req can actually dial.
func (c *Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d is out of range 1-65535", c.Port)
	}
	if c.Proxy == "" {
		return nil
	}

	u, err := url.Parse(c.Proxy)
	if err != nil {
		return fmt.Errorf("proxy %q: %w", c.Proxy, err)
	}
	if u.Host == "" {
		return fmt.Errorf("proxy %q has no host", c.Proxy)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy %q: unsupported scheme %q (want http, https, socks5 or socks5h)", c.Proxy, u.Scheme)
	}
	return nil
}
