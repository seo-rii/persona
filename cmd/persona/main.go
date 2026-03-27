package main

import (
	"errors"
	"fmt"
	"os"

	"persona/internal/model"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var exitErr *exitError
		if errors.As(err, &exitErr) {
			os.Exit(int(exitErr.code))
		}
		var personaErr *model.PersonaError
		if errors.As(err, &personaErr) {
			if personaErr.Error() != "" {
				fmt.Fprintln(os.Stderr, personaErr)
			}
			os.Exit(int(personaErr.Code))
		}
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(int(model.ExitEnv))
	}
}
