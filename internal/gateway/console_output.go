package gateway

import (
	"fmt"
	"os"
)

// Console is an explicitly selected local transport.
// Never route its replies through an IM account that may have logged in later.
func sendConsoleText(chat, text string) error {
	_, err := fmt.Fprintf(os.Stdout, "[%s] %s\n", chat, text)
	return err
}
