package httpapi

import (
	"net/http"
)

// swaggerVersion pins the Swagger UI distribution loaded from the CDN.
const swaggerVersion = "5.17.14"

// swaggerPage embeds no inline script (the CSP forbids it); the
// initialisation lives at /docs/swagger-init.js on the same origin.
const swaggerPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Content Search API - Documentation</title>
  <link rel="stylesheet" href="` + swaggerCDN + `/swagger-ui-dist@` + swaggerVersion + `/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="` + swaggerCDN + `/swagger-ui-dist@` + swaggerVersion + `/swagger-ui-bundle.js" crossorigin></script>
  <script src="/docs/swagger-init.js"></script>
</body>
</html>
`

const swaggerInit = `window.addEventListener("load", function () {
  window.ui = SwaggerUIBundle({
    url: "/openapi.yaml",
    dom_id: "#swagger-ui",
    deepLinking: true,
    displayRequestDuration: true,
    tryItOutEnabled: true
  });
});
`

// openapi serves the specification document.
func (h *handlers) openapi(w http.ResponseWriter, r *http.Request) {
	if len(h.openAPI) == 0 {
		h.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(h.openAPI)
}

// docs serves the Swagger UI page.
func (h *handlers) docs(w http.ResponseWriter, r *http.Request) {
	if len(h.openAPI) == 0 {
		h.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(swaggerPage))
}

func (h *handlers) swaggerInit(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(swaggerInit))
}
