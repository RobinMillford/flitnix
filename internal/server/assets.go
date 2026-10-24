package server

import "embed"

// pageAssets embeds the phone-facing webpage. The assets live inside this Go
// package directory because //go:embed patterns cannot reference parent
// directories of the source file.
//
//go:embed all:assets
var pageAssets embed.FS
