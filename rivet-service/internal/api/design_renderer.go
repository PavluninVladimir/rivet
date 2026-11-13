package api

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/design"
)

// NewDesignScreenshotRenderer wires the trusted schema renderer to an operator-
// configured Chromium executable. An empty path leaves handoff fail-closed.
func NewDesignScreenshotRenderer(binaryPath string) DesignScreenshotRenderer {
	if binaryPath == "" {
		return nil
	}
	return func(ctx context.Context, m design.Model, assets map[string]DesignScreenshotAsset) ([]byte, error) {
		images := map[string]design.ImageAsset{}
		for id, a := range assets {
			images[id] = design.ImageAsset{MIME: a.MIME, Data: a.Data}
		}
		return design.ChromiumScreenshot(ctx, binaryPath, m, images)
	}
}
