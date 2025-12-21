package main

import (
    "context"
    "crypto/tls"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "regexp"
    "runtime"
    "strconv"
    "sync"
    "sync/atomic"
    "time"
)

// chunkData 分片数据结构
type chunkData struct {
    index int
    data  []byte
    start int64
}

// Player 结构体
type Player struct {
    client    *http.Client
    header    http.Header
    start     int64
    end       int64
    thread    int
    chunkSize int
    url       string
    closed    bool
    mu        sync.Mutex
}

// 正则表达式
var crRegex = regexp.MustCompile(`bytes\s+(\d+)-(\d+)/(\d+)`)
var seRegex = regexp.MustCompile(`bytes=(\d+)-(\d*)`)

// NewPlayer 创建播放器 - 大视频优化版
func NewPlayer(header http.Header, thread, chunkSize int, url string) *Player {
    h := http.Header{}
    for _, key := range []string{"User-Agent", "Cookie", "Referer", "Range"} {
        v := header.Get(key)
        if v != "" {
            h.Set(key, v)
        }
    }
    start, end := parseRange(h.Get("Range"))

    // 自适应调整（内部限制，不影响外部参数传递）
    adjustedThread := thread
    adjustedChunkSize := chunkSize
    
    // 内部安全限制
    if adjustedThread > 50 {
        log.Printf("警告: 线程数 %d 超过内部限制，调整为 50", adjustedThread)
        adjustedThread = 50
    }
    if adjustedThread <= 0 {
        adjustedThread = 8 // 默认值
    }
    
    if adjustedChunkSize > 16384 { // 超过16MB
        log.Printf("警告: 分块大小 %dKB 超过内部限制，调整为 8192KB", adjustedChunkSize)
        adjustedChunkSize = 8192
    }
    if adjustedChunkSize <= 0 {
        adjustedChunkSize = 512 // 默认512KB
    }

    return &Player{
        client: &http.Client{
            Timeout: 180 * time.Second, // 大文件增加超时时间
            Transport: &http.Transport{
                MaxIdleConns:        adjustedThread + 20,
                MaxIdleConnsPerHost: adjustedThread + 10,
                IdleConnTimeout:     120 * time.Second,
                TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
                // 为大文件优化
                MaxConnsPerHost:   adjustedThread + 20,
                WriteBufferSize:   128 * 1024, // 128KB 写缓冲区
                ReadBufferSize:    256 * 1024, // 256KB 读缓冲区
                ForceAttemptHTTP2: true,
                ExpectContinueTimeout: 3 * time.Second,
            },
        },
        header:    h,
        start:     start,
        end:       end,
        thread:    adjustedThread,
        chunkSize: adjustedChunkSize * 1024, // 转为字节
        url:       url,
    }
}

// Close 释放资源 - 增强内存管理版
func (p *Player) Close() {
    p.mu.Lock()
    defer p.mu.Unlock()
    
    if !p.closed {
        // 关闭空闲连接
        if trans, ok := p.client.Transport.(*http.Transport); ok {
            trans.CloseIdleConnections()
        }
        
        // 主动释放资源，帮助GC
        p.header = nil
        p.url = ""
        p.closed = true
        
        // 触发一次GC（轻度）
        runtime.GC()
    }
}

// Play 主下载逻辑 - 大视频优化+内存管理版
func (p *Player) Play(w http.ResponseWriter, ctx context.Context) error {
    var cleanupDone bool
    defer func() {
        if !cleanupDone {
            p.cleanupResources()
        }
    }()
    
    // 快速切换检测：如果上下文已取消，立即返回
    select {
    case <-ctx.Done():
        p.cleanupResources()
        cleanupDone = true
        return ctx.Err()
    default:
    }
    
    s, e, err := p.downloadFirst(w, ctx)
    if err != nil {
        p.cleanupResources()
        cleanupDone = true
        return err
    }
    
    if ctx.Err() != nil {
        p.cleanupResources()
        cleanupDone = true
        return ctx.Err()
    }
    
    fileSize := e + 1
    log.Printf("文件大小: %.2f GB", float64(fileSize)/(1024*1024*1024))
    
    // 针对大视频的关键优化：动态调整分块大小（保持大视频优化！）
    actualChunkSize := p.chunkSize
    actualThreads := p.thread
    
    if fileSize > 20*1024*1024*1024 { // 超过20GB
        // 大视频使用更大的分块，减少分块数量
        if actualChunkSize < 4*1024*1024 { // 小于4MB
            actualChunkSize = 4 * 1024 * 1024 // 4MB
            log.Printf("大视频优化: 调整分块大小为 %d MB", actualChunkSize/(1024*1024))
        }
        // 减少线程数，避免过多并发导致拥塞
        if actualThreads > 8 {
            actualThreads = 8
            log.Printf("大视频优化: 调整线程数为 %d", actualThreads)
        }
    } else if fileSize > 10*1024*1024*1024 { // 10-20GB
        if actualChunkSize < 2*1024*1024 { // 小于2MB
            actualChunkSize = 2 * 1024 * 1024 // 2MB
        }
        if actualThreads > 12 {
            actualThreads = 12
        }
    }
    
    log.Printf("实际使用: 线程数=%d, 分块大小=%d字节", actualThreads, actualChunkSize)

    // 计算总块数（使用实际分块大小）
    totalChunks := int64(0)
    if fileSize-s > 0 && actualChunkSize > 0 {
        totalChunks = (fileSize - s + int64(actualChunkSize) - 1) / int64(actualChunkSize)
    }
    if totalChunks <= 0 {
        totalChunks = 1
    }
    
    log.Printf("总块数: %d (每块约 %.2f MB)", 
        totalChunks, float64(actualChunkSize)/(1024*1024))
    
    // 针对大视频优化：如果块数过多，进一步调整（保持大视频优化！）
    if totalChunks > 5000 && actualChunkSize < 8*1024*1024 {
        // 块数超过5000，增大分块大小
        newChunkSize := actualChunkSize * 2
        if newChunkSize <= 16*1024*1024 { // 最大16MB
            actualChunkSize = newChunkSize
            totalChunks = (fileSize - s + int64(actualChunkSize) - 1) / int64(actualChunkSize)
            log.Printf("优化: 块数过多，增大分块到 %d MB，新块数: %d", 
                actualChunkSize/(1024*1024), totalChunks)
        }
    }

    semaphore := make(chan struct{}, actualThreads)
    errChan := make(chan error, actualThreads*2)
    
    // 优化：大视频使用更大的缓冲队列
    bufferSize := actualThreads * 4
    if fileSize > 10*1024*1024*1024 {
        bufferSize = actualThreads * 8
    }
    dataChan := make(chan *chunkData, bufferSize)
    
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()
    
    var wg sync.WaitGroup
    
    // 添加下载计数器
    var downloadedChunks int32
    var totalBytes int64
    
    // 进度监控
    progressInterval := int32(50)
    if totalChunks > 1000 {
        progressInterval = int32(totalChunks / 20) // 5%进度报告一次
    }

    // 内存监控goroutine
    var maxMemory uint64
    memMonitorCtx, memMonitorCancel := context.WithCancel(ctx)
    defer memMonitorCancel()
    
    go func() {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        
        for {
            select {
            case <-memMonitorCtx.Done():
                return
            case <-ticker.C:
                var m runtime.MemStats
                runtime.ReadMemStats(&m)
                currentMem := m.Alloc / 1024 / 1024 // MB
                if uint64(currentMem) > maxMemory {
                    maxMemory = uint64(currentMem)
                }
                
                // 内存使用预警
                if currentMem > 512 { // 超过512MB
                    log.Printf("内存预警: 当前使用 %d MB，峰值 %d MB", currentMem, maxMemory)
                }
            }
        }
    }()

    // 启动分块下载器 - 保持大视频的重试策略
    wg.Add(1)
    go func() {
        defer wg.Done()
        defer close(dataChan)
        
        var downloadWg sync.WaitGroup
        chunkIndex := 0
        
        for startPos := s; startPos < fileSize; {
            chunkStart := startPos
            chunkEnd := chunkStart + int64(actualChunkSize)
            if chunkEnd > fileSize {
                chunkEnd = fileSize
            }
            
            select {
            case <-ctx.Done():
                return
            case semaphore <- struct{}{}:
            }
            
            downloadWg.Add(1)
            currentIndex := chunkIndex
            go func(idx int, startPos, endPos int64) {
                defer downloadWg.Done()
                defer func() { <-semaphore }()
                
                // 大视频专用重试策略（保持大视频优化！）
                var lastError error
                maxRetries := 5
                baseTimeout := 45 * time.Second
                
                // 根据文件大小调整重试策略
                if fileSize > 50*1024*1024*1024 { // 超过50GB
                    maxRetries = 8
                    baseTimeout = 90 * time.Second
                } else if fileSize > 20*1024*1024*1024 { // 20-50GB
                    maxRetries = 6
                    baseTimeout = 60 * time.Second
                }

                for retry := 0; retry < maxRetries; retry++ {
                    select {
                    case <-ctx.Done():
                        return
                    default:
                    }
                    
                    // 动态超时：基于分块大小计算
                    chunkSize := endPos - startPos
                    timeout := baseTimeout
                    if chunkSize > 10*1024*1024 { // 分块大于10MB
                        timeout = time.Duration(chunkSize/(100*1024)) * time.Second // 每100KB 1秒
                        if timeout < 30*time.Second {
                            timeout = 30 * time.Second
                        }
                        if timeout > 120*time.Second {
                            timeout = 120 * time.Second
                        }
                    }
                    
                    chunkCtx, chunkCancel := context.WithTimeout(ctx, timeout)
                    data, _, _, err := p.downloadChunk(chunkCtx, startPos, endPos, 0)
                    chunkCancel()
                    
                    if err == nil {
                        downloaded := atomic.AddInt32(&downloadedChunks, 1)
                        atomic.AddInt64(&totalBytes, int64(len(data)))
                        
                        // 进度报告
                        if downloaded%progressInterval == 0 {
                            progress := float64(downloaded) / float64(totalChunks) * 100
                            log.Printf("下载进度: %.1f%% (%d/%d), 已下载: %.2f GB", 
                                progress, downloaded, totalChunks, 
                                float64(atomic.LoadInt64(&totalBytes))/(1024*1024*1024))
                        }
                        
                        select {
                        case <-ctx.Done():
                            return
                        case dataChan <- &chunkData{
                            index: idx,
                            data:  data,
                            start: startPos,
                        }:
                        }
                        return
                    }
                    
                    lastError = err
                    
                    // 指数退避，大文件等待时间更长
                    waitBase := 2 * time.Second
                    if fileSize > 20*1024*1024*1024 {
                        waitBase = 3 * time.Second
                    }
                    
                    if retry < maxRetries-1 {
                        waitTime := time.Duration(retry+1) * waitBase
                        select {
                        case <-ctx.Done():
                            return
                        case <-time.After(waitTime):
                        }
                    }
                }
                
                // 所有重试都失败
                select {
                case errChan <- fmt.Errorf("分片 %d (范围 %d-%d) 下载失败: %v", 
                    idx, startPos, endPos, lastError):
                case <-ctx.Done():
                }
            }(currentIndex, chunkStart, chunkEnd)
            
            startPos = chunkEnd
            chunkIndex++
        }
        
        downloadWg.Wait()
    }()
    
    // 启动数据写入器 - 优化内存使用
    wg.Add(1)
    go func() {
        defer wg.Done()
        
        buffer := make(map[int][]byte)
        nextIndex := 0
        lastFlush := time.Now()
        lastWriteTime := time.Now()
        lastHeartbeat := time.Now()
        bytesWritten := int64(0)
        
        // 大视频写入优化：批量写入
        var writeBuffer []byte
        bufferThreshold := 256 * 1024 // 256KB
        
        // 清理函数，用于释放内存
        cleanupBuffer := func() {
            // 定期清理已处理的缓冲区
            if len(buffer) > 50 {
                for i := 0; i < nextIndex-20; i++ {
                    if _, exists := buffer[i]; exists {
                        delete(buffer, i)
                    }
                }
            }
        }
        
        defer cleanupBuffer() // 确保退出时清理
        
        for {
            select {
            case <-ctx.Done():
                // 快速退出：清理所有资源
                buffer = nil
                writeBuffer = nil
                runtime.GC()
                return
            case err := <-errChan:
                log.Printf("分片下载失败: %v", err)
                continue
            case data, ok := <-dataChan:
                if !ok {
                    // 通道关闭，写入剩余缓冲数据
                    if len(writeBuffer) > 0 {
                        if _, err := w.Write(writeBuffer); err != nil {
                            log.Println("写入剩余数据错误:", err)
                            return
                        }
                        writeBuffer = writeBuffer[:0]
                    }
                    
                    // 清理缓冲映射
                    for i := nextIndex; i < int(totalChunks); i++ {
                        if bufferData, exists := buffer[i]; exists {
                            if _, err := w.Write(bufferData); err != nil {
                                log.Println("写入剩余缓冲错误:", err)
                                return
                            }
                            delete(buffer, i)
                        }
                    }
                    
                    // 释放内存
                    buffer = nil
                    return
                }
                
                buffer[data.index] = data.data
                
                // 按顺序写入
                for buffer[nextIndex] != nil {
                    chunkData := buffer[nextIndex]
                    
                    // 使用缓冲写入提高性能
                    if len(writeBuffer)+len(chunkData) > bufferThreshold {
                        if _, err := w.Write(writeBuffer); err != nil {
                            log.Println("批量写入错误:", err)
                            return
                        }
                        bytesWritten += int64(len(writeBuffer))
                        writeBuffer = writeBuffer[:0]
                    }
                    
                    writeBuffer = append(writeBuffer, chunkData...)
                    bytesWritten += int64(len(chunkData))
                    
                    // 立即释放已写入的分片内存
                    delete(buffer, nextIndex)
                    nextIndex++
                    lastWriteTime = time.Now()
                    
                    // 检查是否完成
                    if nextIndex >= int(totalChunks) {
                        // 写入最后的缓冲数据
                        if len(writeBuffer) > 0 {
                            if _, err := w.Write(writeBuffer); err != nil {
                                log.Println("最终写入错误:", err)
                                return
                            }
                        }
                        
                        log.Printf("所有数据写入完成，共写入 %d 个分片，%.2f GB", 
                            nextIndex, float64(bytesWritten)/(1024*1024*1024))
                        return
                    }
                    
                    // 定期清理缓冲映射（避免内存泄漏）
                    if nextIndex%20 == 0 {
                        cleanupBuffer()
                    }
                }
                
                // 优化刷新策略：大视频需要更频繁刷新
                flushInterval := 200 * time.Millisecond
                if fileSize > 20*1024*1024*1024 {
                    flushInterval = 100 * time.Millisecond // 大视频更频繁刷新
                }
                
                // 批量刷新写入缓冲
                if time.Since(lastFlush) > flushInterval && len(writeBuffer) > 0 {
                    if _, err := w.Write(writeBuffer); err != nil {
                        log.Println("定时写入错误:", err)
                        return
                    }
                    bytesWritten += int64(len(writeBuffer))
                    writeBuffer = writeBuffer[:0]
                    
                    if f, ok := w.(http.Flusher); ok {
                        f.Flush()
                    }
                    lastFlush = time.Now()
                }
                
                // 关键：大视频心跳机制，防止播放器超时
                if fileSize > 10*1024*1024*1024 && time.Since(lastHeartbeat) > 3*time.Second {
                    if f, ok := w.(http.Flusher); ok {
                        f.Flush()
                        lastHeartbeat = time.Now()
                    }
                }
                
                // 检测写入停滞
                if time.Since(lastWriteTime) > 20*time.Second && nextIndex < int(totalChunks) {
                    log.Printf("警告: 写入停滞 %.1f 秒，等待分片 %d，缓冲分片数: %d", 
                        time.Since(lastWriteTime).Seconds(), nextIndex, len(buffer))
                    
                    // 尝试强制刷新
                    if f, ok := w.(http.Flusher); ok {
                        if len(writeBuffer) > 0 {
                            if _, err := w.Write(writeBuffer); err != nil {
                                log.Println("停滞时写入错误:", err)
                                return
                            }
                            writeBuffer = writeBuffer[:0]
                        }
                        f.Flush()
                        lastWriteTime = time.Now()
                    }
                }
            }
        }
    }()
    
    // 等待完成
    wg.Wait()
    
    // 最终刷新
    if f, ok := w.(http.Flusher); ok {
        f.Flush()
    }
    
    // 最终内存报告
    log.Printf("下载完成: %d 分片，总计 %.2f GB，峰值内存: %d MB", 
        atomic.LoadInt32(&downloadedChunks), 
        float64(atomic.LoadInt64(&totalBytes))/(1024*1024*1024),
        maxMemory)
    
    // 播放结束时触发一次GC
    runtime.GC()
    
    // 标记清理已完成
    cleanupDone = true
    
    return nil
}

// cleanupResources 清理资源（比Close更轻量）
func (p *Player) cleanupResources() {
    p.mu.Lock()
    defer p.mu.Unlock()
    
    if !p.closed {
        // 只清理必要资源，不关闭连接池
        p.header = nil
        p.url = ""
        p.closed = true
    }
}

// downloadFirst 下载第一个分片获取文件信息 - 大视频优化版
func (p *Player) downloadFirst(w http.ResponseWriter, ctx context.Context) (int64, int64, error) {
    start, end := p.start, p.end
    
    // 重要修复：电视端切换下一集时可能发送 bytes=0-，需要特殊处理
    if start == 0 && end == -1 {
        // 电视端可能只需要前1MB来探测文件
        end = 1024 * 1024 // 1MB
        log.Printf("电视端切换检测: 初始请求范围为 0-%d", end)
    } else if end <= 0 {
        end = 100
    } else {
        end += 1
    }
    
    end = start + m(end, int64(p.chunkSize))
    log.Printf("首次请求范围: %d-%d", start, end)

    // 大视频使用更长的首次请求超时
    firstCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
    defer cancel()
    
    chunk, header, status, err := p.downloadChunk(firstCtx, start, end, 1)
    if err != nil {
        return 0, 0, err
    }
    
    select {
    case <-ctx.Done():
        return 0, 0, ctx.Err()
    default:
    }
    
    matches := crRegex.FindStringSubmatch(header.Get("Content-Range"))
    if len(matches) != 4 {
        return 0, 0, errors.New("未获取到文件总大小")
    }
    totalLengthStr := matches[3]
    totalLength, err := strconv.ParseInt(totalLengthStr, 10, 64)
    if err != nil {
        return 0, 0, fmt.Errorf("转换文件总大小失败 %w", err)
    }

    // 关键：确保 end 不超过文件大小
    if p.end <= 0 {
        end = totalLength - 1
    } else {
        end = p.end
        if end >= totalLength {
            end = totalLength - 1
        }
    }
    
    // 修复花屏问题：确保Content-Range正确且完整
    if end >= totalLength {
        end = totalLength - 1
    }
    contentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, totalLength)
    header.Set("Content-Range", contentRange)
    
    // 添加播放器兼容性头部，防止花屏
    header.Set("Accept-Ranges", "bytes")
    header.Set("Connection", "keep-alive")
    
    // 尝试从URL推断Content-Type
    if header.Get("Content-Type") == "" {
        header.Set("Content-Type", "video/mp4") // 默认设为mp4
    }
    
    // 电视端特殊处理：如果请求是 bytes=0-，返回正确的范围
    if p.start == 0 && p.end == -1 {
        // 电视端需要整个文件的范围信息
        header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", totalLength-1, totalLength))
        header.Set("Content-Length", strconv.FormatInt(totalLength, 10))
    } else {
        // 设置Content-Length为完整范围（大视频需要）
        contentLength := end - start + 1
        header.Set("Content-Length", strconv.FormatInt(contentLength, 10))
    }
    
    // 设置响应头
    for k, v := range header {
        w.Header()[k] = v
    }
    w.WriteHeader(status)
    
    // 立即写入第一块数据
    _, err = w.Write(chunk)
    if err != nil {
        return 0, 0, err
    }
    
    // 立即刷新，确保播放器能立即开始播放
    if f, ok := w.(http.Flusher); ok {
        f.Flush()
    }
    
    // 关键：返回正确的结束位置
    // 如果电视端请求的是 bytes=0-，那么返回 0 到 totalLength-1 的范围
    if p.start == 0 && p.end == -1 {
        return int64(len(chunk)), totalLength - 1, nil
    }
    
    return start + int64(len(chunk)), end, nil
}

// downloadChunk2 底层分片下载方法 - 大视频优化
func (p *Player) downloadChunk2(ctx context.Context, start, end int64) ([]byte, http.Header, int, error) {
    req, err := http.NewRequest("GET", p.url, nil)
    if err != nil {
        return nil, nil, -1, fmt.Errorf("创建请求失败 %v", err)
    }
    
    req.Header = p.header.Clone()
    rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end-1)
    req.Header.Set("Range", rangeHeader)
    
    // 大视频优化：设置更合适的请求头
    req.Header.Set("Connection", "keep-alive")
    req.Header.Set("Accept-Encoding", "identity") // 禁用压缩，避免花屏
    req = req.WithContext(ctx)
    
    resp, err := p.client.Do(req)
    if err != nil {
        return nil, nil, -1, fmt.Errorf("下载失败 (%s) %v", rangeHeader, err)
    }
    defer func(Body io.ReadCloser) {
        err := Body.Close()
        if err != nil {
            log.Println("body关闭失败:", err)
        }
    }(resp.Body)

    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
        // 读取数据
        data, err := io.ReadAll(resp.Body)
        if err != nil {
            return nil, nil, -1, fmt.Errorf("读取数据失败 %v", err)
        }
        
        return data, resp.Header, resp.StatusCode, nil
    }

    return nil, nil, resp.StatusCode, fmt.Errorf("请求数据失败 %s", resp.Status)
}

// downloadChunk 分片下载（带重试）
func (p *Player) downloadChunk(ctx context.Context, start, end int64, maxRetries int) ([]byte, http.Header, int, error) {
    if maxRetries == 0 {
        maxRetries = 3 // 默认值
    }
    
    var lastErr error
    for retry := 0; retry < maxRetries; retry++ {
        select {
        case <-ctx.Done():
            return nil, nil, -1, ctx.Err()
        default:
        }
        
        // 创建带超时的子上下文
        chunkCtx, cancel := context.WithTimeout(ctx, 60*time.Second) // 增加超时时间
        chunk, header, status, err := p.downloadChunk2(chunkCtx, start, end)
        cancel()
        
        if err == nil {
            return chunk, header, status, nil
        }
        
        lastErr = err
        
        // 不是最后一次重试则等待
        if retry < maxRetries-1 {
            select {
            case <-ctx.Done():
                return nil, nil, -1, ctx.Err()
            case <-time.After(time.Duration(retry+1) * 2 * time.Second):
            }
        }
    }
    
    return nil, nil, -1, fmt.Errorf("达到最大重试次数 (%d), 最后错误: %v", maxRetries, lastErr)
}

// parseRange 解析Range头
func parseRange(rangeStr string) (int64, int64) {
    match := seRegex.FindStringSubmatch(rangeStr)
    if len(match) == 0 {
        return 0, -1
    }
    startStr, endStr := match[1], match[2]

    start, err := strconv.ParseInt(startStr, 10, 64)
    if err != nil {
        start = 0
    }
    end := int64(-1)
    if endStr != "" {
        end, err = strconv.ParseInt(endStr, 10, 64)
        if err != nil {
            end = -1
        }
    }
    return start, end
}

// m 取最小值
func m(a, b int64) int64 {
    if a < b {
        return a
    }
    return b
}
