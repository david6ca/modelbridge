//go:build darwin

package desktop

import (
	"errors"
	"fmt"
	"os"

	"github.com/david6ca/modelbridge/internal/bridge"
)

func FocusExisting() (bool, error) { return false, nil }
func ShowError(message string)     { fmt.Fprintln(os.Stderr, message) }
func Run(app *bridge.App, hidden bool) error {
	return errors.New("launch ModelBridge.app for the Cocoa interface, or use --no-open for a headless bridge")
}
