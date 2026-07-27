package unifiedui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterServesUnifiedAdminAndPublicArtifacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	Register(router)
	for _, test := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/", contentType: "text/html", contains: "卡密查询"},
		{path: "/static/user.css", contentType: "text/css", contains: ":root"},
		{path: "/static/user.js", contentType: "application/javascript", contains: "navigator.clipboard.writeText"},
		{path: "/admin", contentType: "text/html", contains: "<div id=\"app\"></div>"},
		{path: "/admin/allocation/cards", contentType: "text/html", contains: "<div id=\"app\"></div>"},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), test.contentType) || !strings.Contains(response.Body.String(), test.contains) {
			t.Fatalf("GET %s = %d type=%q body=%q", test.path, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store, max-age=0" {
			t.Fatalf("GET %s cache-control=%q", test.path, response.Header().Get("Cache-Control"))
		}
	}
}

func TestBuiltHTMLContainsNoInlineScriptOrStyle(t *testing.T) {
	root, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(root)
	if strings.Contains(html, "<style") || strings.Contains(html, "<script>") || strings.Contains(html, "style=") {
		t.Fatalf("admin HTML contains inline execution or style: %s", html)
	}
}
