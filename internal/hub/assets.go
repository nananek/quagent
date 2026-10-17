package hub

import (
	"embed"
	"html/template"
)

//go:embed assets
var assetsFS embed.FS

// indexTemplate はブラウザに返す HTML。セッション名だけを差し込む。
var indexTemplate = template.Must(template.ParseFS(assetsFS, "assets/index.html"))
