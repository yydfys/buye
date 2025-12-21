package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

func main() {
	// 添加请求计数器
	var activeRequests int32
	var mu sync.Mutex
	
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := fmt.Fprintf(w, "ok")
		if err != nil {
			fmt.Printf("输出响应错误：%v", err)
		}
	})
	
	http.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		// 限制并发请求数（防止DoS）
		mu.Lock()
		if activeRequests >= 20 { // 允许最多20个并发请求
			mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "服务器忙，请稍后重试")
			return
		}
		activeRequests++
		mu.Unlock()
		
		defer func() {
			mu.Lock()
			activeRequests--
			mu.Unlock()
		}()
		
		// 设置请求超时（5分钟，适应大文件）
		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
		defer cancel()
		
		params := r.URL.Query()
		thread := params.Get("thread")
		chunkSize := params.Get("chunkSize")
		url := params.Get("url")

		// 基本验证
		if thread == "" || chunkSize == "" || url == "" {
			w.WriteHeader(http.StatusBadRequest)
			if thread == "" {
				_, _ = fmt.Fprint(w, "thread不能为空")
			} else if chunkSize == "" {
				_, _ = fmt.Fprint(w, "chunkSize不能为空")
			} else {
				_, _ = fmt.Fprint(w, "url不能为空")
			}
			return
		}

		// 转换参数，只验证格式
		t, err := strconv.Atoi(thread)
		if err != nil || t <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, "thread必须为正整数")
			return
		}
		
		c, err := strconv.Atoi(chunkSize)
		if err != nil || c <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, "chunkSize必须为正整数")
			return
		}
		
		// 警告日志，不拒绝大参数（但记录日志）
		if t > 20 {
			log.Printf("警告: 线程数较高 thread=%d url=%s", t, url)
		}
		if c > 8192 { // 超过8MB
			log.Printf("警告: 分块大小较大 chunkSize=%dKB url=%s", c, url)
		}

		player := NewPlayer(r.Header, t, c, url)
		defer player.Close()
		
		err = player.Play(w, ctx)
		if err != nil {
			log.Println("代理失败:", err)
			if ctx.Err() == context.DeadlineExceeded {
				w.WriteHeader(http.StatusGatewayTimeout)
				fmt.Fprint(w, "请求超时")
			} else {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "代理失败: %v", err)
			}
			return
		}
	})

	log.Println("服务启动在端口 5575")
	log.Fatal(http.ListenAndServe(":5575", nil))
}