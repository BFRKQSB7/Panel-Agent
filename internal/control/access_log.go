package control

import (
	"log"
	"net/http"
	"strings"
)

func accessLogPath(path string) string {
	for _, prefix := range []string{"/u/", "/s/"} {
		if strings.HasPrefix(path, prefix) {
			return prefix + "[redacted]"
		}
	}
	return path
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redact before routing or authentication, including malformed private
		// paths. Never pass the raw URL, query, headers, or body to this logger.
		log.Printf("http method=%q path=%q", r.Method, accessLogPath(r.URL.Path))
		next.ServeHTTP(w, r)
	})
}
