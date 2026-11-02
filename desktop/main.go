package main

import (
	"log"
	"net/http"

	"austro-os/internal/webui"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func main() {
	assets, err := webui.Load()
	if err != nil {
		log.Fatalf("load embedded AUSTRO UI: %v", err)
	}

	mux := http.NewServeMux()
	for asset, body := range assets {
		mux.HandleFunc(asset.Pattern, webui.Handler(asset, body))
	}

	app := application.New(application.Options{
		Name:        "AUSTRO OS",
		Description: "AUSTRO OS Windows desktop proof of concept",
		Assets: application.AssetOptions{
			Handler: mux,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "AUSTRO OS",
		Width:     1280,
		Height:    800,
		MinWidth:  960,
		MinHeight: 640,
		URL:       "/",
	})

	if err := app.Run(); err != nil {
		log.Fatalf("run AUSTRO OS desktop: %v", err)
	}
}
