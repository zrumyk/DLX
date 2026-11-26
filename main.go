/*
 * @Author: Vincent Yang
 * @Date: 2023-07-01 21:45:34
 * @LastEditors: Jason Lyu
 * @LastEditTime: 2025-04-08 13:45:00
 * @FilePath: /DLX/main.go
 * @Telegram: https://t.me/missuo
 * @GitHub: https://github.com/missuo
 *
 * Copyright © 2024 by Vincent, All Rights Reserved.
 */

package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/OwO-Network/DLX/service"
)

func main() {
	cfg := service.InitConfig()

	// Setting the application to release mode
	gin.SetMode(gin.ReleaseMode)

	app := service.Router(cfg)
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.IP, strconv.Itoa(cfg.Port)))
	if err != nil {
		log.Fatalf("Failed to bind DLX listener: %v", err)
	}
	fmt.Printf("DLX has been successfully launched! Listening on %s\n", listener.Addr())
	fmt.Println("Developed by sjlleo <i@leo.moe> and missuo <me@missuo.me>.")
	if err := http.Serve(listener, app); err != nil {
		log.Fatalf("DLX server stopped: %v", err)
	}
}
