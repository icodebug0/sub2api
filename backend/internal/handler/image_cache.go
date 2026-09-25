package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	ImageCacheMaxAge = 3 * time.Hour
	cleanupInterval  = 10 * time.Minute
)

var (
	imageCacheDirOnce sync.Once
	cachedImageDir    string
	validFilenameRe   = regexp.MustCompile(`^[a-zA-Z0-9_\-]+\.(png|jpg|jpeg|gif|webp)$`)
	downloadClient    = &http.Client{
		Timeout: 30 * time.Second,
	}
)

// GetImageCacheDir 获取本地图片缓存目录并确保存在，启动后台清理协程
func GetImageCacheDir() string {
	imageCacheDirOnce.Do(func() {
		dir := os.Getenv("IMAGE_CACHE_DIR")
		if dir == "" {
			// 优先使用挂载的 /app/data
			if _, err := os.Stat("/app/data"); err == nil {
				dir = "/app/data/image_cache"
			} else {
				dir = "./data/image_cache"
			}
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("failed to create image cache directory", "dir", dir, "error", err)
		}
		cachedImageDir = dir

		// 启动后台定时清理协程
		go runImageCacheCleaner(cachedImageDir, ImageCacheMaxAge, cleanupInterval)
	})
	return cachedImageDir
}

func runImageCacheCleaner(dir string, maxAge time.Duration, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 启动时先执行一次清理
	cleanExpiredImages(dir, maxAge)

	for range ticker.C {
		cleanExpiredImages(dir, maxAge)
	}
}

func cleanExpiredImages(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	removedCount := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			path := filepath.Join(dir, entry.Name())
			if err := os.Remove(path); err == nil {
				removedCount++
			}
		}
	}
	if removedCount > 0 {
		slog.Info("image cache cleanup completed", "removed_files", removedCount, "max_age", maxAge)
	}
}

func generateImageFilename(ext string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	randHex := hex.EncodeToString(b)
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return fmt.Sprintf("img_%d_%s%s", time.Now().Unix(), randHex, ext)
}

func detectImageExtension(data []byte) string {
	if len(data) >= 8 && string(data[0:8]) == "\x89PNG\r\n\x1a\n" {
		return ".png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return ".jpg"
	}
	if len(data) >= 6 && (string(data[0:6]) == "GIF87a" || string(data[0:6]) == "GIF89a") {
		return ".gif"
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return ".webp"
	}
	return ".png"
}

// CacheImage 下载或解码图片并缓存到本地，返回替换为本站自建域名的公开图片URL
func CacheImage(r *http.Request, rawURLOrBase64 string) string {
	if rawURLOrBase64 == "" {
		return rawURLOrBase64
	}

	var data []byte
	var ext string

	if strings.HasPrefix(rawURLOrBase64, "http://") || strings.HasPrefix(rawURLOrBase64, "https://") {
		// 网络图片外链，由服务端下载
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURLOrBase64, nil)
		if err != nil {
			slog.Warn("cache image: failed to create request", "url", rawURLOrBase64, "error", err)
			return rawURLOrBase64
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		resp, err := downloadClient.Do(req)
		if err != nil {
			slog.Warn("cache image: failed to download", "url", rawURLOrBase64, "error", err)
			return rawURLOrBase64
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			slog.Warn("cache image: bad status code", "url", rawURLOrBase64, "status", resp.StatusCode)
			return rawURLOrBase64
		}

		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 最大20MB限制
		if err != nil || len(bodyBytes) == 0 {
			slog.Warn("cache image: read body failed", "error", err)
			return rawURLOrBase64
		}
		data = bodyBytes
		ext = detectImageExtension(data)
	} else {
		// Base64 图片
		rawStr := rawURLOrBase64
		if idx := strings.Index(rawStr, ","); idx != -1 {
			rawStr = rawStr[idx+1:]
		}
		decoded, err := base64.StdEncoding.DecodeString(rawStr)
		if err != nil {
			slog.Warn("cache image: decode base64 failed", "error", err)
			return rawURLOrBase64
		}
		data = decoded
		ext = detectImageExtension(data)
	}

	dir := GetImageCacheDir()
	filename := generateImageFilename(ext)
	filePath := filepath.Join(dir, filename)

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		slog.Error("cache image: failed to write file", "path", filePath, "error", err)
		return rawURLOrBase64
	}

	// 动态拼接对外访问域名
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "https" // 外部通常为 Cloudflare https 反代
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" || strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost") {
		host = "gptapi2.2035000.xyz"
	}

	cachedURL := fmt.Sprintf("%s://%s/v1/images/cache/%s", proto, host, filename)
	slog.Info("image cached and URL rewritten", "filename", filename, "bytes", len(data), "cached_url", cachedURL)
	return cachedURL
}

// HandleGetCachedImage 处理外部无鉴权获取本地缓存图片的请求
func HandleGetCachedImage(c *gin.Context) {
	filename := c.Param("filename")
	if !validFilenameRe.MatchString(filename) {
		c.String(http.StatusBadRequest, "Invalid image filename")
		return
	}

	dir := GetImageCacheDir()
	filePath := filepath.Join(dir, filepath.Clean(filename))

	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.String(http.StatusNotFound, "Image not found")
			return
		}
		c.String(http.StatusInternalServerError, "Failed to read image")
		return
	}

	// 检查 3 小时过期
	if time.Since(info.ModTime()) > ImageCacheMaxAge {
		_ = os.Remove(filePath)
		c.String(http.StatusNotFound, "Image has expired")
		return
	}

	c.Header("Cache-Control", "public, max-age=10800")
	c.Header("Access-Control-Allow-Origin", "*")
	c.File(filePath)
}
