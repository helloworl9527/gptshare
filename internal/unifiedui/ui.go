package unifiedui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed static
var assets embed.FS

func Register(router *gin.Engine) {
	root, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	router.GET("/", fileHandler(root, "user.html", "text/html; charset=utf-8"))
	router.GET("/static/user.css", fileHandler(root, "static/user.css", "text/css; charset=utf-8"))
	router.GET("/static/user.js", fileHandler(root, "static/user.js", "application/javascript; charset=utf-8"))
	router.GET("/admin", adminHandler(root))
	router.GET("/admin/*path", adminHandler(root))
}

func fileHandler(root fs.FS, name, contentType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := fs.ReadFile(root, name)
		if err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-store, max-age=0")
		c.Data(http.StatusOK, contentType, body)
	}
}

func adminHandler(root fs.FS) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.TrimPrefix(c.Request.URL.Path, "/admin/")
		if path != "" && strings.HasPrefix(path, "assets/") {
			c.FileFromFS(path, http.FS(root))
			return
		}
		fileHandler(root, "index.html", "text/html; charset=utf-8")(c)
	}
}
