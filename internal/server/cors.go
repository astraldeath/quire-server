package server

import "net/http"

// WithCORS is opt-in for browser clients; native clients do not need CORS.
func WithCORS(next http.Handler, origins []string) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range origins {
		if origin != "" && origin != "*" {
			allowed[origin] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Add("Vary", "Origin")
			if !allowed[origin] {
				respond(w, 403, map[string]string{"error": "browser origin is not allowed"})
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			if r.Method == "OPTIONS" {
				w.WriteHeader(204)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
